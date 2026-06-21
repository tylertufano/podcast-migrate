package apple

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tyler/podcast-migrate/internal/httputil"
	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/model"
)

// catalogEntry holds an Apple catalog episode ID and its publication date.
// The pub date is kept so that title-keyed lookups can apply date tolerance.
type catalogEntry struct {
	id      int64
	pubDate time.Time
}

// CatalogClient resolves Overcast episode metadata to Apple catalog episode IDs.
//
// Resolution is a two-step process:
//  1. iTunes Search API (itunes.apple.com/search) — find the Apple podcast
//     collectionId by searching with the podcast title and confirming the feed URL.
//  2. amp-api.podcasts.apple.com catalog — paginate through all episodes for the
//     podcast, building an in-memory index keyed by the same strategies used by
//     the SQLite path (feeddate, feedtitle, poddate, podtitle).
//
// Results are cached in memory so each podcast is queried at most once per run.
// Only the Bearer token is required — the media-user-token is not needed for
// catalog reads.
type CatalogClient struct {
	bearerToken string
	httpClient  *http.Client

	// podcastIDCache: normalized feedURL → Apple podcast collectionId.
	// A value of -1 means the podcast was searched and not found in the catalog.
	podMu          sync.Mutex
	podcastIDCache map[string]int64

	// episodeIndexCache: collectionId → episode lookup index.
	epMu              sync.Mutex
	episodeIndexCache map[int64]map[string]catalogEntry
}

// NewCatalogClient creates a CatalogClient. bearerToken is the same app-level
// Apple Podcasts JWT used for the playback-positions write endpoint.
func NewCatalogClient(bearerToken string) *CatalogClient {
	return &CatalogClient{
		bearerToken:       bearerToken,
		httpClient:        &http.Client{Timeout: 30 * time.Second},
		podcastIDCache:    make(map[string]int64),
		episodeIndexCache: make(map[int64]map[string]catalogEntry),
	}
}

// FindEpisodeResult indicates whether and how an episode was located.
type FindEpisodeResult int

const (
	// CatalogFound means the episode was matched and appleID is valid.
	CatalogFound FindEpisodeResult = iota
	// CatalogPodcastNotInCatalog means no Apple catalog entry exists for this
	// podcast's feed URL. The episode cannot be marked via the web API.
	CatalogPodcastNotInCatalog
	// CatalogEpisodeNotMatched means the podcast is in the catalog but the
	// episode couldn't be matched by any of the four lookup strategies.
	CatalogEpisodeNotMatched
)

// FindEpisode looks up the Apple catalog episode ID for ep.
//
// Implemented strategies (migrate.MatchStrategy), in priority order:
//   - MatchByFeedDate  — feed URL + exact pub date.
//   - MatchByFeedTitle — feed URL + episode title, date-tolerance guard applied.
//   - MatchByPodDate   — podcast title + exact pub date (cross-feed); skipped when
//     strictFeedMatch is true.
//   - MatchByPodTitle  — podcast title + episode title, date-tolerance guard applied
//     (cross-feed); skipped when strictFeedMatch is true.
//
// Absent strategies and rationale:
//   - MatchByGUID: the Apple catalog API (amp-api.podcasts.apple.com) does not
//     expose episode GUIDs in its episode list response.
//   - MatchByTitleDate: the catalog has per-podcast episode lists so a podcast-title
//     anchor (MatchByPodDate/MatchByPodTitle) is both cheaper and higher-precision
//     than a title-only cross-feed lookup.
//
// feedToTitle maps feed URL → lower-cased podcast title (built from the merged
// library). tolerance is the maximum pub-date gap allowed for title-keyed
// matches; pass 0 to disable the guard. strictFeedMatch limits matching to
// MatchByFeedDate and MatchByFeedTitle only.
//
// Returns the Apple episode ID, a FindEpisodeResult status, and any error.
func (c *CatalogClient) FindEpisode(
	ctx context.Context,
	ep model.EpisodeState,
	feedToTitle map[string]string,
	tolerance time.Duration,
	strictFeedMatch bool,
) (appleID int64, result FindEpisodeResult, err error) {
	if ep.FeedURL == "" {
		return 0, CatalogPodcastNotInCatalog, nil
	}

	podcastTitle := feedToTitle[ep.FeedURL]

	// Step 1: resolve the Apple podcast collectionId for this feed.
	collectionID, found, err := c.lookupPodcastID(ctx, ep.FeedURL, podcastTitle)
	if err != nil {
		return 0, CatalogPodcastNotInCatalog, err
	}
	if !found {
		return 0, CatalogPodcastNotInCatalog, nil
	}

	// Step 2: build or retrieve the cached episode index.
	index, err := c.getOrBuildEpisodeIndex(ctx, collectionID, ep.FeedURL, podcastTitle)
	if err != nil {
		return 0, CatalogEpisodeNotMatched, err
	}

	// Step 3: cascade match — mirrors findInAppleIndex.
	normFeed := migrate.NormalizeFeedURL(ep.FeedURL)
	podTitleLower := strings.ToLower(strings.TrimSpace(podcastTitle))

	// Strategy 1: feeddate — same feed URL, exact pub date.
	if !ep.PubDate.IsZero() {
		if e, ok := index[feedDateKey(normFeed, ep.PubDate)]; ok {
			return e.id, CatalogFound, nil
		}
	}

	// Strategy 2: feedtitle — same feed URL, same episode title (date-tolerant).
	if ep.Title != "" {
		if e, ok := index[feedTitleKey(normFeed, ep.Title)]; ok {
			if withinDateTolerance(ep.PubDate, e.pubDate, tolerance) {
				return e.id, CatalogFound, nil
			}
		}
	}

	// Strategies 3 and 4 are cross-feed fallbacks that match by podcast title
	// rather than feed URL. Skipped when strictFeedMatch is enabled.
	if !strictFeedMatch {
		// Strategy 3: poddate — same podcast title, exact pub date (cross-feed).
		if podTitleLower != "" && !ep.PubDate.IsZero() {
			if e, ok := index[podDateKey(podTitleLower, ep.PubDate)]; ok {
				return e.id, CatalogFound, nil
			}
		}

		// Strategy 4: podtitle — same podcast title, same episode title (cross-feed).
		if podTitleLower != "" && ep.Title != "" {
			if e, ok := index[podTitleKey(podTitleLower, ep.Title)]; ok {
				if withinDateTolerance(ep.PubDate, e.pubDate, tolerance) {
					return e.id, CatalogFound, nil
				}
			}
		}
	}

	return 0, CatalogEpisodeNotMatched, nil
}

// ---------------------------------------------------------------------------
// Podcast ID lookup (iTunes Search API)
// ---------------------------------------------------------------------------

// lookupPodcastID returns the Apple catalog collectionId for the given feed URL.
// Uses the iTunes Search API with the podcast title as the search term,
// then matches by feed URL for disambiguation.
func (c *CatalogClient) lookupPodcastID(ctx context.Context, feedURL, podcastTitle string) (int64, bool, error) {
	normFeed := migrate.NormalizeFeedURL(feedURL)

	c.podMu.Lock()
	if cached, ok := c.podcastIDCache[normFeed]; ok {
		c.podMu.Unlock()
		if cached == -1 {
			return 0, false, nil
		}
		return cached, true, nil
	}
	c.podMu.Unlock()

	id, found, err := c.searchITunes(ctx, feedURL, podcastTitle)
	if err != nil {
		return 0, false, err
	}

	c.podMu.Lock()
	if found {
		c.podcastIDCache[normFeed] = id
	} else {
		c.podcastIDCache[normFeed] = -1
	}
	c.podMu.Unlock()

	return id, found, nil
}

// searchITunes calls the iTunes Search API to find the Apple podcast collectionId
// for the given feed URL. It searches by podcast title and matches results by feedUrl.
func (c *CatalogClient) searchITunes(ctx context.Context, feedURL, podcastTitle string) (int64, bool, error) {
	normFeed := migrate.NormalizeFeedURL(feedURL)

	q := url.Values{}
	q.Set("media", "podcast")
	q.Set("entity", "podcast")
	q.Set("term", podcastTitle)
	q.Set("limit", "10")

	searchURL := "https://itunes.apple.com/search?" + q.Encode()

	var result struct {
		Results []struct {
			CollectionId   int64  `json:"collectionId"`
			FeedUrl        string `json:"feedUrl"`
			CollectionName string `json:"collectionName"`
		} `json:"results"`
	}

	if err := httputil.RetryFunc(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
		if err != nil {
			return fmt.Errorf("catalog: iTunes search: %w", err)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return httputil.NewTransientError(fmt.Errorf("catalog: iTunes search: %w", err))
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			return &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 30*time.Second)}
		}
		if resp.StatusCode >= 500 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			return httputil.NewTransientError(fmt.Errorf("catalog: iTunes search: HTTP %d: %s", resp.StatusCode, body))
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			return fmt.Errorf("catalog: iTunes search: HTTP %d: %s", resp.StatusCode, body)
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return fmt.Errorf("catalog: iTunes search: decode: %w", err)
		}
		return nil
	}, httputil.RetryOptions{}); err != nil {
		return 0, false, err
	}

	// Primary: match by normalized feed URL.
	for _, r := range result.Results {
		if migrate.NormalizeFeedURL(r.FeedUrl) == normFeed {
			return r.CollectionId, true, nil
		}
	}

	// Fallback 1: exact podcast title match when no feed URL matched.
	// The source app may store a redirected or alternate feed URL that differs
	// from what the iTunes catalog has. Searching by title already scoped results,
	// so an exact case-insensitive name match is reliable.
	// Mirrors the title-fallback in overcast.SearchPodcastITunesID.
	titleNorm := strings.ToLower(strings.TrimSpace(podcastTitle))
	if titleNorm != "" {
		for _, r := range result.Results {
			if strings.ToLower(strings.TrimSpace(r.CollectionName)) == titleNorm {
				fmt.Printf("  note: iTunes feed URL mismatch for %q — matched by podcast title (catalog feedUrl: %s)\n",
					podcastTitle, r.FeedUrl)
				return r.CollectionId, true, nil
			}
		}

		// Fallback 2: Plus-normalised title match.
		// When the source has a paid feed (e.g. "Fresh Air Plus" from NPR Plus) but
		// the Apple catalog only knows the public title ("Fresh Air"), strip the paid
		// suffix and try again. NormalizePlusTitle lowercases, so we compare directly.
		baseTitleNorm := model.NormalizePlusTitle(podcastTitle)
		if baseTitleNorm != titleNorm {
			for _, r := range result.Results {
				if strings.ToLower(strings.TrimSpace(r.CollectionName)) == baseTitleNorm {
					fmt.Printf("  note: iTunes feed URL mismatch for %q — matched via Plus-normalized title %q (catalog feedUrl: %s)\n",
						podcastTitle, baseTitleNorm, r.FeedUrl)
					return r.CollectionId, true, nil
				}
			}
		}
	}

	return 0, false, nil
}

// ---------------------------------------------------------------------------
// Episode index (amp-api catalog)
// ---------------------------------------------------------------------------

// getOrBuildEpisodeIndex returns the cached episode index for the given podcast,
// building it by paginating the amp-api catalog if not yet cached.
//
// feedURL and podcastTitle are used to build index keys that match the Overcast
// source data (feeddate/feedtitle use feedURL; poddate/podtitle use podcastTitle).
func (c *CatalogClient) getOrBuildEpisodeIndex(
	ctx context.Context,
	collectionID int64,
	feedURL, podcastTitle string,
) (map[string]catalogEntry, error) {
	c.epMu.Lock()
	if idx, ok := c.episodeIndexCache[collectionID]; ok {
		c.epMu.Unlock()
		return idx, nil
	}
	c.epMu.Unlock()

	idx, err := c.paginateEpisodes(ctx, collectionID, feedURL, podcastTitle)
	if err != nil {
		return nil, err
	}

	c.epMu.Lock()
	c.episodeIndexCache[collectionID] = idx
	c.epMu.Unlock()

	return idx, nil
}

// episodeRaw is a single episode from the amp-api catalog.
type episodeRaw struct {
	id      int64
	title   string
	pubDate time.Time
	guid    string
}

// paginateEpisodes fetches all episodes for a podcast from amp-api and returns
// an index keyed by the same four strategies used by the SQLite path.
func (c *CatalogClient) paginateEpisodes(
	ctx context.Context,
	collectionID int64,
	feedURL, podcastTitle string,
) (map[string]catalogEntry, error) {
	normFeed := migrate.NormalizeFeedURL(feedURL)
	podTitleLower := strings.ToLower(strings.TrimSpace(podcastTitle))

	index := make(map[string]catalogEntry)
	offset := 0
	const pageSize = 100
	total := -1

	for {
		page, pageTotal, err := c.fetchEpisodePage(ctx, collectionID, offset, pageSize)
		if err != nil {
			return nil, err
		}
		if total == -1 {
			total = pageTotal
		}

		for _, ep := range page {
			if ep.id == 0 {
				continue
			}
			entry := catalogEntry{id: ep.id, pubDate: ep.pubDate}

			// Feed URL-based keys (primary).
			if !ep.pubDate.IsZero() {
				key := feedDateKey(normFeed, ep.pubDate)
				if _, exists := index[key]; !exists {
					index[key] = entry
				}
			}
			if ep.title != "" {
				key := feedTitleKey(normFeed, ep.title)
				if _, exists := index[key]; !exists {
					index[key] = entry
				}
			}

			// Podcast-title-based keys (secondary, cross-feed matching).
			if podTitleLower != "" {
				if !ep.pubDate.IsZero() {
					key := podDateKey(podTitleLower, ep.pubDate)
					if _, exists := index[key]; !exists {
						index[key] = entry
					}
				}
				if ep.title != "" {
					key := podTitleKey(podTitleLower, ep.title)
					if _, exists := index[key]; !exists {
						index[key] = entry
					}
				}
			}
		}

		offset += len(page)
		if len(page) == 0 || (total > 0 && offset >= total) {
			break
		}
	}

	return index, nil
}

// fetchEpisodePage fetches one page of episodes from the amp-api catalog.
// Returns the episodes, the total episode count from meta.total, and any error.
func (c *CatalogClient) fetchEpisodePage(
	ctx context.Context,
	collectionID int64,
	offset, limit int,
) ([]episodeRaw, int, error) {
	u := fmt.Sprintf(
		"https://amp-api.podcasts.apple.com/v1/catalog/us/podcasts/%d/episodes?limit=%d&offset=%d&l=en-US",
		collectionID, limit, offset,
	)
	pageNum := offset / limit

	var result struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Name            string `json:"name"`
				ReleaseDateTime string `json:"releaseDateTime"`
				Guid            string `json:"guid"`
			} `json:"attributes"`
		} `json:"data"`
		Meta struct {
			Total int `json:"total"`
		} `json:"meta"`
	}
	notFound := false

	if err := httputil.RetryFunc(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return fmt.Errorf("catalog: fetch episodes: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)
		req.Header.Set("Origin", "https://podcasts.apple.com")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return httputil.NewTransientError(fmt.Errorf("catalog: fetch episodes page %d: %w", pageNum, err))
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			// Apple returns 404 when the offset is past the end of the episode list
			// (code 40403 "No related resources"). Treat as end-of-results.
			notFound = true
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 30*time.Second)}
		}
		if resp.StatusCode >= 500 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return httputil.NewTransientError(fmt.Errorf("catalog: fetch episodes page %d: HTTP %d: %s", pageNum, resp.StatusCode, body))
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return fmt.Errorf("catalog: fetch episodes page %d: HTTP %d: %s", pageNum, resp.StatusCode, body)
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return fmt.Errorf("catalog: fetch episodes page %d: decode: %w", pageNum, err)
		}
		return nil
	}, httputil.RetryOptions{}); err != nil {
		return nil, 0, err
	}

	if notFound {
		return nil, 0, nil
	}

	episodes := make([]episodeRaw, 0, len(result.Data))
	for _, d := range result.Data {
		id, _ := strconv.ParseInt(d.ID, 10, 64)

		var pubDate time.Time
		if d.Attributes.ReleaseDateTime != "" {
			if t, err := time.Parse(time.RFC3339, d.Attributes.ReleaseDateTime); err == nil {
				pubDate = t
			}
		}

		episodes = append(episodes, episodeRaw{
			id:      id,
			title:   d.Attributes.Name,
			pubDate: pubDate,
			guid:    d.Attributes.Guid,
		})
	}

	return episodes, result.Meta.Total, nil
}
