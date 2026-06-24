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

	// Build the set of feed URLs we need to fetch RSS for.
	feedURLs := make([]string, 0, len(kw.subscriptions))
	for _, sub := range kw.subscriptions {
		if sub.Subscribed == 1 && sub.FeedURL != "" {
			feedURLs = append(feedURLs, sub.FeedURL)
		}
	}

	// Fetch RSS feeds concurrently to get episode metadata (title, pubDate, duration).
	rssFeeds := r.fetchRSSFeeds(ctx, feedURLs)

	// Build the library.
	lib := &model.Library{}

	// Podcasts from KVS subscription list; enrich with RSS channel metadata.
	for _, sub := range kw.subscriptions {
		if sub.Subscribed != 1 || sub.FeedURL == "" {
			continue
		}
		pod := model.Podcast{
			FeedURL: sub.FeedURL,
			Title:   sub.Title,
		}
		if feed, ok := rssFeeds[sub.FeedURL]; ok {
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
	}

	// Episodes: iterate over all per-feed playState entries, resolve UPP state,
	// and fill in metadata from RSS.
	var matched int
	for feedURL, psFeed := range kw.podcastsFeeds {
		rssFeed := rssFeeds[feedURL] // may be zero value if fetch failed
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

			// Decode UPP play state for this episode.
			compressed, hasUPP := kw.serverRawValues[psEp.MetadataIdentifier]
			if !hasUPP {
				continue // no play state record → skip (not played/in-progress)
			}
			uppData, err := decodeUPPEntry(ctx, compressed)
			if err != nil {
				continue
			}

			// Apply --since filter on UPP timestamp.
			if !r.sinceTime.IsZero() {
				ts := coreDataEpoch.Add(time.Duration(uppData.TimestampSec * float64(time.Second)))
				if ts.Before(r.sinceTime) {
					continue
				}
			}

			// Only include episodes with meaningful play state.
			if !uppData.HasBeenPlayed && uppData.BookmarkTimeSec == 0 {
				continue
			}

			ep := model.EpisodeState{
				GUID:    psEp.GUID,
				FeedURL: feedURL,
			}

			// Enrich with RSS metadata.
			if rssItem, ok := rssIdx[psEp.GUID]; ok {
				ep.Title = rssItem.Title
				ep.PubDate = rssItem.PubDate
				ep.Duration = rssItem.Duration
			}

			// Apply play state.
			if uppData.HasBeenPlayed {
				ep.PlayState = model.PlayStatePlayed
			} else if uppData.BookmarkTimeSec > 0 {
				ep.PlayState = model.PlayStateInProgress
				ep.PlayPosition = time.Duration(uppData.BookmarkTimeSec * float64(time.Second))
			}

			// LastPlayed from KVS timestamp (best proxy available without SQLite).
			if uppData.TimestampSec > 0 {
				ep.LastPlayed = coreDataEpoch.Add(time.Duration(uppData.TimestampSec * float64(time.Second)))
			}

			lib.Episodes = append(lib.Episodes, ep)
			matched++
		}
	}

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

// fetchRSSFeeds concurrently fetches the RSS feeds for the given URLs and
// returns a map of feedURL→rssFeed. Failures are silently skipped so that
// KVS data can still be used for that feed (episodes will lack title/pubDate).
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
