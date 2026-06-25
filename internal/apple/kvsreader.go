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

// KVSReader reads a model.Library from Apple's iCloud KVS + RSS.
// Requires APPLE_KVS_DSID and APPLE_KVS_COOKIES env vars.
type KVSReader struct {
	kvsWriter   *KVSWriter
	httpClient  *http.Client
	sinceTime   time.Time
	concurrency int // max parallel RSS fetches (default 8)
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
		kvsWriter:   kw,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		concurrency: 8,
	}, nil
}

// SetSinceTime restricts Read to episodes whose KVS play-state timestamp is
// after t. A zero t reads all episodes.
func (r *KVSReader) SetSinceTime(t time.Time) { r.sinceTime = t }

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
	feedURLSet := make(map[string]bool)

	// subByClean maps cleaned feedURL → subscription metadata.
	type subInfo struct {
		title string
	}
	subByClean := make(map[string]subInfo, len(kw.subscriptions))
	var skippedInternal int
	for _, sub := range kw.subscriptions {
		if sub.Subscribed != 1 || sub.FeedURL == "" {
			continue
		}
		clean := cleanFeedURL(sub.FeedURL)
		// Restrict to importable http/https feeds. internal:// are Apple-exclusive
		// shows with no public RSS; other schemes cannot be imported by other apps.
		if !strings.HasPrefix(clean, "http://") && !strings.HasPrefix(clean, "https://") {
			skippedInternal++
			continue
		}
		subByClean[clean] = subInfo{title: sub.Title}
		feedURLSet[clean] = true
	}

	// Also include feedURLs from playState entries (handles feeds that appear in
	// com.apple.podcasts but are not (or no longer) in the subscription list).
	// Restrict to http/https — internal:// and other non-standard schemes cannot
	// be imported by any destination app.
	for rawURL := range kw.podcastsFeeds {
		clean := cleanFeedURL(rawURL)
		if strings.HasPrefix(clean, "http://") || strings.HasPrefix(clean, "https://") {
			feedURLSet[clean] = true
		}
	}

	// Collect sorted unique feed URLs for RSS fetching.
	feedURLs := make([]string, 0, len(feedURLSet))
	for u := range feedURLSet {
		feedURLs = append(feedURLs, u)
	}

	// Fetch RSS feeds concurrently to get episode metadata (title, pubDate, duration).
	rssFeeds := r.fetchRSSFeeds(ctx, feedURLs)

	lib := &model.Library{}

	// Podcasts: subscribed feeds first (ordered by subscription list).
	inLib := make(map[string]bool, len(feedURLSet))
	for _, sub := range kw.subscriptions {
		if sub.Subscribed != 1 || sub.FeedURL == "" {
			continue
		}
		clean := cleanFeedURL(sub.FeedURL)
		if inLib[clean] {
			continue
		}
		pod := model.Podcast{
			FeedURL: clean,
			Title:   sub.Title,
		}
		if feed, ok := rssFeeds[clean]; ok {
			if feed.Author != "" {
				pod.Author = feed.Author
			}
			if feed.ImageURL != "" {
				pod.ImageURL = feed.ImageURL
			}
			if pod.Title == "" {
				pod.Title = feed.Title
			}
		}
		lib.Podcasts = append(lib.Podcasts, pod)
		inLib[clean] = true
	}

	// Also include podcasts that have playState data but are not in the
	// subscription list (user unsubscribed but still has play history).
	for rawURL := range kw.podcastsFeeds {
		clean := cleanFeedURL(rawURL)
		if inLib[clean] {
			continue
		}
		pod := model.Podcast{FeedURL: clean}
		if feed, ok := rssFeeds[clean]; ok {
			pod.Title = feed.Title
			pod.Author = feed.Author
			pod.ImageURL = feed.ImageURL
		}
		lib.Podcasts = append(lib.Podcasts, pod)
		inLib[clean] = true
	}

	// Episodes: iterate over per-feed playState entries, look up UPP state,
	// fill in metadata from RSS.
	var matched int
	for rawURL, psFeed := range kw.podcastsFeeds {
		clean := cleanFeedURL(rawURL)
		if !strings.HasPrefix(clean, "http://") && !strings.HasPrefix(clean, "https://") {
			continue
		}
		rssFeed := rssFeeds[clean]

		// Build GUID→rssItem index for O(1) lookup.
		rssIdx := make(map[string]*rssItem, len(rssFeed.Items))
		for i := range rssFeed.Items {
			rssIdx[rssFeed.Items[i].GUID] = &rssFeed.Items[i]
		}

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

			ep := model.EpisodeState{
				GUID:    psEp.GUID,
				FeedURL: clean, // canonical URL, matches lib.Podcasts entries
			}

			if rssItem, ok := rssIdx[psEp.GUID]; ok {
				ep.Title = rssItem.Title
				ep.PubDate = rssItem.PubDate
				ep.Duration = rssItem.Duration
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
	}

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
