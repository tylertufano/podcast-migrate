package apple

// kvsreader.go provides KVSReader, which builds a model.Library purely from
// Apple's iCloud KVS + RSS feeds — no local SQLite database required.
//
// Data sources:
//   - com.apple.podcasts KVS domain: subscription list (feedURL, title) and
//     per-feed episode index (guid → metadataIdentifier)
//   - com.apple.upp KVS domain: per-episode play state (played, position,
//     timestamp of last change)
//   - RSS feeds: episode titles, pub dates, durations
//
// This enables Apple Podcasts migrations from non-macOS platforms.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// subInfo holds per-subscription metadata built during the subscription scan.
type subInfo struct {
	title     string
	feedURL   string // export URL: subscriber URL if private, canonical if public
	canonical string // iTunes canonical feed URL; falls back to cleanFeedURL(sub.FeedURL)
	itunesID  string // iTunes Store ID as a string; empty for private/unindexed feeds
	author    string // from iTunes lookup; empty for private/unindexed feeds
	imageURL  string // from iTunes lookup (artworkUrl600); empty for private/unindexed feeds
	isPrivate bool   // true when KVS URL differs from iTunes canonical, or PodcastPID == 0
}

// KVSReader reads a model.Library from Apple's iCloud KVS + RSS.
// Requires APPLE_KVS_DSID and APPLE_KVS_COOKIES env vars.
type KVSReader struct {
	kvsWriter        *KVSWriter
	httpClient       *http.Client
	sinceTime        time.Time
	skipRSSFetch     bool            // when true, skip RSS fetches (subscriptions-only runs)
	allPlayState     bool            // when true, fetch RSS and emit episodes for unsubscribed feeds too
	concurrency      int             // max parallel RSS fetches (default 5)
	privateFeedMode  PrivateFeedMode // URL strategy for KVS URL ≠ iTunes canonical
}

// NewKVSReader creates a KVSReader using KVS credentials from env vars
// (APPLE_KVS_DSID, APPLE_KVS_COOKIES). Returns an error when credentials
// are absent or invalid.
func NewKVSReader() (*KVSReader, error) {
	if os.Getenv("APPLE_KVS_COOKIES") == "" {
		return nil, fmt.Errorf("apple: APPLE_KVS_COOKIES not set (required for KVS-only read)")
	}
	kw, err := NewKVSWriter("") // empty sqlitePath: credential resolution from env only
	if err != nil {
		return nil, fmt.Errorf("apple: KVS credentials: %w", err)
	}
	return &KVSReader{
		kvsWriter:       kw,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		concurrency:     5,
		privateFeedMode: PrivateFeedSubscriber, // default: keep KVS when it adds value
	}, nil
}

// SetSinceTime restricts Read to episodes whose KVS play-state timestamp is
// after t. A zero t reads all episodes.
func (r *KVSReader) SetSinceTime(t time.Time)              { r.sinceTime = t }
func (r *KVSReader) SetSkipRSSFetch(skip bool)             { r.skipRSSFetch = skip }
func (r *KVSReader) SetAllPlayState(all bool)              { r.allPlayState = all }
func (r *KVSReader) SetPrivateFeedMode(m PrivateFeedMode)  { r.privateFeedMode = m }

// Read fetches subscriptions and play state from KVS, resolves episode
// metadata from RSS feeds, and returns the merged library.
func (r *KVSReader) Read(ctx context.Context) (*model.Library, error) {
	kw := r.kvsWriter

	// Fetch com.apple.podcasts: subscription list + per-feed episode identity.
	if err := kw.initPodcastsDomain(ctx); err != nil {
		return nil, fmt.Errorf("apple: KVS podcasts domain: %w", err)
	}

	// Fetch com.apple.upp: per-episode play state.
	if err := kw.initSession(ctx); err != nil {
		return nil, fmt.Errorf("apple: KVS UPP domain: %w", err)
	}

	fmt.Printf("apple: KVS-only read active (DSID %s) — %d UPP records, %d feeds\n",
		kw.dsid, len(kw.serverRawValues), len(kw.podcastsFeeds))

	if !r.sinceTime.IsZero() {
		fmt.Printf("apple: delta mode — reading episodes modified since %s (KVS timestamp)\n",
			r.sinceTime.Local().Format("2006-01-02 15:04:05"))
	}

	// Normalize feedURLs from both sources (subscriptions + playState entries)
	// and collect the union for RSS fetching.
	//
	// Apple may append ?t=<timestamp> cache-buster params to stored feed URLs.
	// cleanFeedURL strips these so that subscription.FeedURL and playState key
	// URLs resolve to the same canonical URL, even if Apple wrote them at
	// different times with different timestamps.
	//
	// For catalog subscriptions (PodcastPID > 0), we additionally replace the
	// Apple-stored URL (which may be a subscriber JWT URL like
	// slateprivate.supportingcast.fm/content/eyJ...) with the public canonical
	// URL from the iTunes Store. This ensures destinations subscribe to the
	// correct public feed rather than treating the feed as private/unpublished.
	feedURLSet := make(map[string]bool)

	// Step 1: batch iTunes lookup for all catalog subscriptions.
	// Collect unique StoreCollectionIDs first, then do a single batched request.
	pidSet := make(map[int64]bool, len(kw.subscriptions))
	for _, sub := range kw.subscriptions {
		if sub.Subscribed != 1 || sub.FeedURL == "" || sub.StoreCollectionID <= 0 {
			continue
		}
		pidSet[sub.StoreCollectionID] = true
	}
	pids := make([]int64, 0, len(pidSet))
	for pid := range pidSet {
		pids = append(pids, pid)
	}
	itunesResults := map[int64]iTunesLookupResult{}
	if len(pids) > 0 {
		var lookupErr error
		itunesResults, lookupErr = batchITunesLookup(ctx, r.httpClient, pids)
		if lookupErr != nil {
			fmt.Fprintf(os.Stderr, "apple: iTunes lookup failed (%v) — using KVS feed URLs\n", lookupErr)
		} else {
			fmt.Printf("apple: iTunes lookup resolved %d/%d canonical feed URL(s)\n", len(itunesResults), len(pids))
		}
	}

	// Step 2: build cleanToCanonical and cleanToITunesID maps.
	// Iterate every subscription directly so that all URL variants of the same
	// podcast (e.g. a direct feed URL and a pdrl.fm redirect both pointing to the
	// same StoreCollectionID) map to the same canonical — avoiding duplicates when
	// the same podcast appears under two different stored URLs.
	cleanToCanonical := make(map[string]string, len(kw.subscriptions))
	cleanToITunesID := make(map[string]string, len(kw.subscriptions))
	for _, sub := range kw.subscriptions {
		if sub.Subscribed != 1 || sub.FeedURL == "" || sub.StoreCollectionID <= 0 {
			continue
		}
		result, ok := itunesResults[sub.StoreCollectionID]
		if !ok || result.FeedURL == "" {
			continue
		}
		clean := cleanFeedURL(sub.FeedURL)
		canonical := cleanFeedURL(result.FeedURL)
		if strings.HasPrefix(canonical, "http://") || strings.HasPrefix(canonical, "https://") {
			cleanToCanonical[clean] = canonical
			cleanToITunesID[clean] = strconv.FormatInt(sub.StoreCollectionID, 10)
		}
	}

	// Step 2.5: identify feeds where the KVS subscription URL differs from the
	// iTunes canonical. Collected unconditionally when detection is active so that
	// classification runs even on subscriptions-only runs (--only-subscriptions,
	// --overcast-out), where the correct feed URL still matters for the export.
	var mismatches []mismatchedFeed
	var classifyURLSet map[string]bool // RSS URLs needed solely for classification
	needsDetection := r.privateFeedMode == PrivateFeedSubscriber || r.privateFeedMode == PrivateFeedCustom
	if needsDetection {
		classifyURLSet = make(map[string]bool)
		seen := make(map[string]bool)
		for _, sub := range kw.subscriptions {
			if sub.Subscribed != 1 || sub.FeedURL == "" {
				continue
			}
			clean := cleanFeedURL(sub.FeedURL)
			canonical, hasCan := cleanToCanonical[clean]
			if !hasCan || clean == canonical || seen[clean] {
				continue
			}
			seen[clean] = true
			mismatches = append(mismatches, mismatchedFeed{
				clean:     clean,
				kvsURL:    clean,
				canonical: canonical,
				title:     sub.Title,
			})
			classifyURLSet[clean] = true      // KVS URL: needed for classification
			classifyURLSet[canonical] = true  // iTunes URL: also needed for comparison
		}
	}

	// Step 3: populate feedURLSet with the iTunes canonical (or KVS URL for
	// kvs mode) for every subscribed feed. subByClean is built after RSS fetch
	// once resolvedCanonical is known.
	type subEntry struct {
		clean string
		sub   podcastSubscription
	}
	var subsOrdered []subEntry
	var skippedInternal int
	for _, sub := range kw.subscriptions {
		if sub.Subscribed != 1 || sub.FeedURL == "" {
			continue
		}
		clean := cleanFeedURL(sub.FeedURL)
		if !strings.HasPrefix(clean, "http://") && !strings.HasPrefix(clean, "https://") {
			skippedInternal++
			continue
		}
		subsOrdered = append(subsOrdered, subEntry{clean: clean, sub: sub})

		canonical := clean
		if c, ok := cleanToCanonical[clean]; ok {
			canonical = c
		}
		switch r.privateFeedMode {
		case PrivateFeedKVS:
			feedURLSet[clean] = true // always use KVS URL
		default:
			feedURLSet[canonical] = true // default: iTunes canonical
		}
	}

	// When --all-play-state is set, also fetch RSS for feeds that have play state
	// history but are no longer subscribed. This enables play state migration across
	// different feed URLs for the same podcast, or recovery of historical play data.
	if r.allPlayState {
		for rawURL := range kw.podcastsFeeds {
			clean := cleanFeedURL(rawURL)
			if !strings.HasPrefix(clean, "http://") && !strings.HasPrefix(clean, "https://") {
				continue
			}
			canonical := clean
			if c, ok := cleanToCanonical[clean]; ok {
				canonical = c
			}
			feedURLSet[canonical] = true
		}
	}

	// Fetch RSS feeds concurrently to get episode metadata (title, pubDate, duration).
	// For subscriptions-only runs (skipRSSFetch), episode metadata is not needed,
	// but classification URLs are still fetched when detection is active so that
	// --private-feed subscriber produces the correct feed URL in the export.
	var rssFeeds map[string]rssFeed
	if !r.skipRSSFetch {
		// Full fetch: merge classification URLs into the main feed URL set.
		for u := range classifyURLSet {
			feedURLSet[u] = true
		}
		feedURLs := make([]string, 0, len(feedURLSet))
		for u := range feedURLSet {
			feedURLs = append(feedURLs, u)
		}
		rssFeeds = r.fetchRSSFeeds(ctx, feedURLs)
	} else if needsDetection && len(mismatches) > 0 {
		// Subscriptions-only: fetch only the two URLs per mismatched feed.
		classifyURLs := make([]string, 0, len(classifyURLSet))
		for u := range classifyURLSet {
			classifyURLs = append(classifyURLs, u)
		}
		rssFeeds = r.fetchRSSFeeds(ctx, classifyURLs)
	}

	// Step 3.5: classify mismatched feeds using the fetched RSS and build
	// resolvedCanonical — the authoritative URL to use for each feed.
	// For public/kvs modes this is trivial (no detection needed).
	resolvedCanonical := make(map[string]string, len(cleanToCanonical))
	for clean, canonical := range cleanToCanonical {
		resolvedCanonical[clean] = canonical // default: iTunes canonical
	}
	if needsDetection && rssFeeds != nil {
		if r.privateFeedMode == PrivateFeedCustom {
			fmt.Printf("\napple: --private-feed=custom — reviewing %d feed(s) where KVS URL differs from iTunes\n", len(mismatches))
		}
		for _, m := range mismatches {
			kvsRSS := rssFeeds[m.kvsURL]
			itunesRSS := rssFeeds[m.canonical]
			class, exclusive := classifyMismatchedFeed(kvsRSS, itunesRSS)
			var resolved string
			if r.privateFeedMode == PrivateFeedCustom {
				resolved = promptPrivateFeedChoice(m, class, exclusive)
			} else {
				resolved = resolveURL(r.privateFeedMode, m, class, exclusive)
			}
			resolvedCanonical[m.clean] = resolved
			// Ensure resolved URL RSS is in the cache (may be a custom URL).
			if _, ok := rssFeeds[resolved]; !ok && resolved != m.kvsURL && resolved != m.canonical {
				if feed, err := fetchRSSFeed(ctx, r.httpClient, resolved); err == nil {
					rssFeeds[resolved] = feed
				}
			}
		}
	} else if r.privateFeedMode == PrivateFeedKVS {
		// kvs mode: override every mapped entry to use the KVS URL.
		for clean := range cleanToCanonical {
			resolvedCanonical[clean] = clean
		}
	}
	// public mode: resolvedCanonical already holds iTunes canonicals (the default).

	// Step 4: build subByClean using resolvedCanonical.
	subByClean := make(map[string]subInfo, len(subsOrdered))
	for _, entry := range subsOrdered {
		clean := entry.clean
		sub := entry.sub

		resolved := clean
		if c, ok := resolvedCanonical[clean]; ok {
			resolved = c
		}
		canonical := cleanToCanonical[clean] // empty string if no iTunes entry
		isPrivate := sub.StoreCollectionID == 0 || (canonical != "" && resolved != canonical)
		var author, imageURL string
		if result, ok := itunesResults[sub.StoreCollectionID]; ok {
			author = result.Author
			imageURL = result.ImageURL
		}
		if _, exists := subByClean[resolved]; exists {
			continue // dedup: same resolved URL from multiple stored entries
		}
		subByClean[clean] = subInfo{
			title:     sub.Title,
			feedURL:   resolved,
			canonical: resolved,
			itunesID:  cleanToITunesID[clean],
			author:    author,
			imageURL:  imageURL,
			isPrivate: isPrivate,
		}
	}

	lib := &model.Library{}

	// Podcasts: subscribed feeds first (ordered by subscription list).
	inLib := make(map[string]bool, len(feedURLSet))
	for _, sub := range kw.subscriptions {
		if sub.Subscribed != 1 || sub.FeedURL == "" {
			continue
		}
		clean := cleanFeedURL(sub.FeedURL)
		info, ok := subByClean[clean]
		if !ok {
			continue // filtered (internal://, etc.)
		}
		if inLib[info.feedURL] {
			continue
		}
		pod := model.Podcast{
			FeedURL:   info.feedURL,
			Title:     sub.Title,
			Author:    info.author,
			ImageURL:  info.imageURL,
			ITunesID:  info.itunesID,
			IsPrivate: info.isPrivate,
		}
		if feed, ok := rssFeeds[info.feedURL]; ok {
			if pod.Title == "" {
				pod.Title = feed.Title
			}
		}
		lib.Podcasts = append(lib.Podcasts, pod)
		inLib[info.feedURL] = true
	}

	// Episodes: iterate over per-feed playState entries, look up UPP state,
	// fill in metadata from RSS. Use canonical URLs so episode FeedURLs match
	// the corresponding lib.Podcasts entries. Skipped entirely for subscriptions-only runs.
	var matched int
	if r.skipRSSFetch {
		goto doneEpisodes
	}
	for rawURL, psFeed := range kw.podcastsFeeds {
		clean := cleanFeedURL(rawURL)
		if !strings.HasPrefix(clean, "http://") && !strings.HasPrefix(clean, "https://") {
			continue
		}
		canonical := clean
		if c, ok := resolvedCanonical[clean]; ok {
			canonical = c
		} else if c, ok := cleanToCanonical[clean]; ok {
			canonical = c
		}
		// Use canonical URL so episode FeedURL matches the corresponding lib.Podcasts entry.
		rssFeed := rssFeeds[canonical]

		// Build GUID→rssItem index for O(1) lookup.
		rssIdx := make(map[string]*rssItem, len(rssFeed.Items))
		for i := range rssFeed.Items {
			rssIdx[rssFeed.Items[i].GUID] = &rssFeed.Items[i]
		}

		var feedTotal, feedMatched int
		for i := range psFeed.Episodes {
			psEp := &psFeed.Episodes[i]
			if psEp.MetadataIdentifier == "" {
				continue
			}

			compressed, hasUPP := kw.serverRawValues[psEp.MetadataIdentifier]
			if !hasUPP {
				continue // no UPP record → episode not played/in-progress
			}
			uppData, err := decodeUPPEntry(ctx, compressed)
			if err != nil {
				continue
			}

			// Apply --since filter on UPP play-state timestamp.
			if !r.sinceTime.IsZero() {
				ts := coreDataEpoch.Add(time.Duration(uppData.TimestampSec * float64(time.Second)))
				if ts.Before(r.sinceTime) {
					continue
				}
			}

			if !uppData.HasBeenPlayed && uppData.BookmarkTimeSec == 0 {
				continue // UPP entry exists but records no meaningful activity
			}

			feedTotal++
			ep := model.EpisodeState{
				GUID:    psEp.GUID,
				FeedURL: canonical,
			}

			if rssItem, ok := rssIdx[psEp.GUID]; ok {
				ep.Title = rssItem.Title
				ep.PubDate = rssItem.PubDate
				ep.Duration = rssItem.Duration
				feedMatched++
			}

			if uppData.HasBeenPlayed {
				ep.PlayState = model.PlayStatePlayed
			} else if uppData.BookmarkTimeSec > 0 {
				ep.PlayState = model.PlayStateInProgress
				ep.PlayPosition = time.Duration(uppData.BookmarkTimeSec * float64(time.Second))
			}

			if uppData.TimestampSec > 0 {
				ep.LastPlayed = coreDataEpoch.Add(time.Duration(uppData.TimestampSec * float64(time.Second)))
			}

			lib.Episodes = append(lib.Episodes, ep)
			matched++
		}

		if feedTotal > 0 && feedMatched < feedTotal {
			podTitle := ""
			if info, ok := subByClean[clean]; ok {
				podTitle = info.title
			}
			label := canonical
			if podTitle != "" {
				label = fmt.Sprintf("%q", podTitle)
			}
			fmt.Printf("apple: rss match %d/%d episodes for %s\n", feedMatched, feedTotal, label)
		}
	}

doneEpisodes:
	lib.SkippedInternalPodcasts = skippedInternal
	fmt.Printf("apple: KVS-only read complete — %d subscriptions, %d episodes with play state\n",
		len(lib.Podcasts), matched)

	return lib, nil
}

// uppEntry holds the decoded fields from a com.apple.upp entry.
type uppEntry struct {
	HasBeenPlayed   bool
	BookmarkTimeSec float64
	PlayCount       int
	TimestampSec    float64
}

// decodeUPPEntry decodes a DEFLATE-compressed UPP binary plist value into
// its constituent fields. The short key names (hbpl, bktm, plct, tstm) are
// Apple's abbreviations for the full field names.
func decodeUPPEntry(ctx context.Context, compressed []byte) (uppEntry, error) {
	inner, err := deflateDecompress(compressed)
	if err != nil {
		return uppEntry{}, fmt.Errorf("deflate: %w", err)
	}
	s, err := bplistToXML(ctx, inner)
	if err != nil {
		return uppEntry{}, fmt.Errorf("plist decode: %w", err)
	}
	var e uppEntry
	if idx := strings.Index(s, "<key>hbpl</key>"); idx != -1 {
		after := strings.TrimSpace(s[idx+len("<key>hbpl</key>"):])
		e.HasBeenPlayed = strings.HasPrefix(after, "<true/>")
	}
	if idx := strings.Index(s, "<key>bktm</key>"); idx != -1 {
		after := strings.TrimSpace(s[idx+len("<key>bktm</key>"):])
		after = strings.TrimPrefix(after, "<real>")
		if v := strings.SplitN(after, "<", 2)[0]; v != after {
			if f, pErr := strconv.ParseFloat(v, 64); pErr == nil {
				e.BookmarkTimeSec = f
			}
		}
	}
	if idx := strings.Index(s, "<key>plct</key>"); idx != -1 {
		after := strings.TrimSpace(s[idx+len("<key>plct</key>"):])
		after = strings.TrimPrefix(after, "<integer>")
		if v := strings.SplitN(after, "<", 2)[0]; v != after {
			if n, pErr := strconv.Atoi(strings.TrimSpace(v)); pErr == nil {
				e.PlayCount = n
			}
		}
	}
	if idx := strings.Index(s, "<key>tstm</key>"); idx != -1 {
		after := strings.TrimSpace(s[idx+len("<key>tstm</key>"):])
		after = strings.TrimPrefix(after, "<real>")
		if v := strings.SplitN(after, "<", 2)[0]; v != after {
			if f, pErr := strconv.ParseFloat(v, 64); pErr == nil {
				e.TimestampSec = f
			}
		}
	}
	return e, nil
}

// fetchRSSFeeds concurrently fetches RSS feeds for the given canonical URLs
// and returns a map of feedURL→rssFeed. Individual failures are silently
// skipped — KVS data still flows for that feed, episodes just lack title/pubDate.
func (r *KVSReader) fetchRSSFeeds(ctx context.Context, feedURLs []string) map[string]rssFeed {
	type result struct {
		url  string
		feed rssFeed
	}

	sem := make(chan struct{}, r.concurrency)
	results := make(chan result, len(feedURLs))
	var wg sync.WaitGroup

	for _, u := range feedURLs {
		wg.Add(1)
		go func(feedURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			feed, err := fetchRSSFeed(ctx, r.httpClient, feedURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "apple: rss fetch failed for %s: %v\n", feedURL, err)
			}
			results <- result{url: feedURL, feed: feed}
		}(u)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make(map[string]rssFeed, len(feedURLs))
	for r := range results {
		out[r.url] = r.feed
	}
	return out
}
