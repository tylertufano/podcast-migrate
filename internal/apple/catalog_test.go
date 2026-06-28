package apple

// White-box tests for CatalogClient — the iTunes Search → amp-api cascade.
// Uses httptest servers so no real network calls are made.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// itunesSearchResponse builds a minimal iTunes Search JSON payload.
func itunesSearchResponse(results []map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"results": results})
	return b
}

// episodePageResponse builds a minimal amp-api episodes page response.
func episodePageResponse(episodes []map[string]any, total int) []byte {
	b, _ := json.Marshal(map[string]any{
		"data": episodes,
		"meta": map[string]any{"total": total},
	})
	return b
}

// TestSearchITunes_FeedURLMatch verifies the primary feed URL matching path.
func TestSearchITunes_FeedURLMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(itunesSearchResponse([]map[string]any{
			{
				"collectionId":   int64(12345),
				"feedUrl":        "https://feeds.example.com/show",
				"collectionName": "Example Show",
			},
		}))
	}))
	defer srv.Close()

	c := NewCatalogClient("token")
	// Point the client at the test server by temporarily monkey-patching the
	// HTTP transport to redirect all requests.
	c.httpClient = &http.Client{
		Transport: rewriteHostTransport(srv.URL),
	}

	id, found, err := c.searchITunes(context.Background(),
		"https://feeds.example.com/show", "Example Show")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true via feed URL match")
	}
	if id != 12345 {
		t.Errorf("collectionId: got %d, want 12345", id)
	}
}

// TestSearchITunes_TitleFallback verifies that a title-exact match is used when
// the feed URL in the catalog differs from the source app's stored URL.
// This reproduces the "The Retrievals" failure reported by the user.
func TestSearchITunes_TitleFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return a result whose feedUrl doesn't match what the source app has,
		// but whose collectionName is an exact title match.
		w.Write(itunesSearchResponse([]map[string]any{
			{
				"collectionId":   int64(1691599042),
				"feedUrl":        "https://feeds.simplecast.com/SD1pPTeV", // Apple's URL
				"collectionName": "The Retrievals",
			},
		}))
	}))
	defer srv.Close()

	c := NewCatalogClient("token")
	c.httpClient = &http.Client{Transport: rewriteHostTransport(srv.URL)}

	// Source app (Overcast) has a different feed URL for the same podcast.
	id, found, err := c.searchITunes(context.Background(),
		"https://feeds.overcast.fm/alternate-retrievals-url", "The Retrievals")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true via title fallback when feed URL mismatches")
	}
	if id != 1691599042 {
		t.Errorf("collectionId: got %d, want 1691599042", id)
	}
}

// TestSearchITunes_TitleFallback_CaseInsensitive verifies the fallback is
// case-insensitive (source "the retrievals" matches catalog "The Retrievals").
func TestSearchITunes_TitleFallback_CaseInsensitive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(itunesSearchResponse([]map[string]any{
			{
				"collectionId":   int64(42),
				"feedUrl":        "https://feeds.example.com/canonical",
				"collectionName": "My Podcast Show",
			},
		}))
	}))
	defer srv.Close()

	c := NewCatalogClient("token")
	c.httpClient = &http.Client{Transport: rewriteHostTransport(srv.URL)}

	id, found, err := c.searchITunes(context.Background(),
		"https://feeds.example.com/old-url", "my podcast show")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true via case-insensitive title fallback")
	}
	if id != 42 {
		t.Errorf("collectionId: got %d, want 42", id)
	}
}

// TestSearchITunes_NotFound verifies that a podcast is not found when neither
// the feed URL nor the title match any search result.
func TestSearchITunes_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(itunesSearchResponse([]map[string]any{
			{
				"collectionId":   int64(99),
				"feedUrl":        "https://feeds.example.com/other",
				"collectionName": "A Different Show",
			},
		}))
	}))
	defer srv.Close()

	c := NewCatalogClient("token")
	c.httpClient = &http.Client{Transport: rewriteHostTransport(srv.URL)}

	_, found, err := c.searchITunes(context.Background(),
		"https://feeds.example.com/my-show", "My Show")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected found=false — neither feed URL nor title matched")
	}
}

// TestFindEpisode_TitleFallbackEndToEnd exercises the full FindEpisode path with
// a mocked iTunes Search that returns a mismatched feed URL, triggering the
// title fallback, then verifies the correct episode is resolved via the catalog.
func TestFindEpisode_TitleFallbackEndToEnd(t *testing.T) {
	mux := http.NewServeMux()

	// iTunes Search — returns a feed URL that doesn't match the source.
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(itunesSearchResponse([]map[string]any{
			{
				"collectionId":   int64(1691599042),
				"feedUrl":        "https://feeds.simplecast.com/SD1pPTeV",
				"collectionName": "The Retrievals",
			},
		}))
	})

	// amp-api episode page — returns a single episode matching our target.
	pubDate := time.Date(2023, 8, 3, 9, 0, 0, 0, time.UTC)
	mux.HandleFunc("/v1/catalog/us/podcasts/1691599042/episodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(episodePageResponse([]map[string]any{
			{
				"id":   "1000716654719",
				"type": "podcast-episodes",
				"attributes": map[string]any{
					"name":            "S01 Episode 5: The Outcomes",
					"releaseDateTime": pubDate.Format(time.RFC3339),
					"guid":            "guid-outcomes",
				},
			},
		}, 1))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewCatalogClient("token")
	c.httpClient = &http.Client{Transport: rewriteHostTransport(srv.URL)}

	ep := model.EpisodeState{
		FeedURL:   "https://feeds.overcast.fm/different-retrievals-url",
		Title:     "S01 Episode 5: The Outcomes",
		PubDate:   pubDate,
		PlayState: model.PlayStatePlayed,
	}
	feedToTitle := map[string]string{
		ep.FeedURL: "the retrievals",
	}

	appleID, result, err := c.FindEpisode(context.Background(), ep, feedToTitle, 72*time.Hour, false)
	if err != nil {
		t.Fatalf("FindEpisode error: %v", err)
	}
	if result != CatalogFound {
		t.Fatalf("FindEpisode result: got %v, want CatalogFound", result)
	}
	if appleID != 1000716654719 {
		t.Errorf("appleID: got %d, want 1000716654719", appleID)
	}
}

// TestSearchITunes_PlusTitleFallback verifies that when the source app has a paid
// feed (e.g. "Fresh Air Plus" from NPR Plus) the Apple catalog is still found by
// stripping the " Plus" suffix and matching the public podcast title.
func TestSearchITunes_PlusTitleFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// iTunes catalog only knows the public show title "Fresh Air".
		w.Write(itunesSearchResponse([]map[string]any{
			{
				"collectionId":   int64(381444908),
				"feedUrl":        "https://feeds.npr.org/381444908/podcast.xml",
				"collectionName": "Fresh Air",
			},
		}))
	}))
	defer srv.Close()

	c := NewCatalogClient("token")
	c.httpClient = &http.Client{Transport: rewriteHostTransport(srv.URL)}

	// Source (Overcast) stored the NPR Plus feed URL and podcast title "Fresh Air Plus".
	id, found, err := c.searchITunes(context.Background(),
		"https://feeds.npr.org/plus/fresh-air", "Fresh Air Plus")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true via Plus-normalised title fallback")
	}
	if id != 381444908 {
		t.Errorf("collectionId: got %d, want 381444908", id)
	}
}

// TestSearchITunes_PlusTitleFallback_PlusSymbol verifies the "+" suffix variant
// (e.g. "Planet Money+") is also stripped when searching the iTunes catalog.
func TestSearchITunes_PlusTitleFallback_PlusSymbol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(itunesSearchResponse([]map[string]any{
			{
				"collectionId":   int64(290783428),
				"feedUrl":        "https://feeds.npr.org/510289/podcast.xml",
				"collectionName": "Planet Money",
			},
		}))
	}))
	defer srv.Close()

	c := NewCatalogClient("token")
	c.httpClient = &http.Client{Transport: rewriteHostTransport(srv.URL)}

	id, found, err := c.searchITunes(context.Background(),
		"https://feeds.npr.org/plus/planet-money", "Planet Money+")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true via Plus-symbol title fallback")
	}
	if id != 290783428 {
		t.Errorf("collectionId: got %d, want 290783428", id)
	}
}

// TestPaginateEpisodes_FindsEpisodeOnSecondPage verifies that paginateEpisodes
// loops past the first page when total > pageSize and the target episode is on
// page 2. The test server returns 100 dummy episodes (total=200) for offset=0
// and one real episode for offset=100, so the loop must make two requests.
func TestPaginateEpisodes_FindsEpisodeOnSecondPage(t *testing.T) {
	targetDate := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	const collectionID = int64(111222333)

	calls := 0
	mux := http.NewServeMux()

	// iTunes Search — returns a matching collection ID.
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(itunesSearchResponse([]map[string]any{
			{
				"collectionId":   collectionID,
				"feedUrl":        "https://feeds.example.com/paginated",
				"collectionName": "Paginated Show",
			},
		}))
	})

	mux.HandleFunc("/v1/catalog/us/podcasts/111222333/episodes", func(w http.ResponseWriter, r *http.Request) {
		calls++
		offset := 0
		if o := r.URL.Query().Get("offset"); o != "" {
			fmt.Sscan(o, &offset) //nolint:errcheck
		}
		w.Header().Set("Content-Type", "application/json")

		if offset == 0 {
			// Page 1: 100 dummy episodes; total = 200 (signals a second page).
			eps := make([]map[string]any, 100)
			for i := range eps {
				eps[i] = map[string]any{
					"id":   fmt.Sprintf("%d", 9000000+i),
					"type": "podcast-episodes",
					"attributes": map[string]any{
						"name":            fmt.Sprintf("Old Episode %d", i),
						"releaseDateTime": time.Date(2020, 1, i+1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
					},
				}
			}
			w.Write(episodePageResponse(eps, 200))
		} else {
			// Page 2: the target episode plus one filler.
			w.Write(episodePageResponse([]map[string]any{
				{
					"id":   "9999999",
					"type": "podcast-episodes",
					"attributes": map[string]any{
						"name":            "Target Episode",
						"releaseDateTime": targetDate.Format(time.RFC3339),
					},
				},
			}, 200))
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewCatalogClient("token")
	c.httpClient = &http.Client{Transport: rewriteHostTransport(srv.URL)}

	ep := model.EpisodeState{
		FeedURL:   "https://feeds.example.com/paginated",
		Title:     "Target Episode",
		PubDate:   targetDate,
		PlayState: model.PlayStatePlayed,
	}
	feedToTitle := map[string]string{ep.FeedURL: "paginated show"}

	appleID, result, err := c.FindEpisode(context.Background(), ep, feedToTitle, 72*time.Hour, false)
	if err != nil {
		t.Fatalf("FindEpisode error: %v", err)
	}
	if result != CatalogFound {
		t.Fatalf("result = %v, want CatalogFound", result)
	}
	if appleID != 9999999 {
		t.Errorf("appleID = %d, want 9999999", appleID)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 episode-page requests (pagination), got %d", calls)
	}
}

// TestSearchITunes_PlusTitleFallback_NotUsedForExactMatch verifies that the
// Plus fallback is not attempted when the feed URL already matched directly.
func TestSearchITunes_PlusTitleFallback_NotUsedForExactMatch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write(itunesSearchResponse([]map[string]any{
			{
				"collectionId":   int64(12345),
				"feedUrl":        "https://feeds.example.com/show",
				"collectionName": "Example Show",
			},
		}))
	}))
	defer srv.Close()

	c := NewCatalogClient("token")
	c.httpClient = &http.Client{Transport: rewriteHostTransport(srv.URL)}

	id, found, err := c.searchITunes(context.Background(),
		"https://feeds.example.com/show", "Example Show")
	if err != nil || !found || id != 12345 {
		t.Fatalf("basic match failed: err=%v found=%v id=%d", err, found, id)
	}
	// Only one HTTP call (no retry from fallback paths).
	if calls != 1 {
		t.Errorf("expected 1 HTTP call (feed URL matched directly), got %d", calls)
	}
}

// rewriteHostTransport returns an http.RoundTripper that redirects all requests
// to baseURL, preserving the path and query. Used to point clients at httptest servers.
type rewriteHost struct {
	base string
}

func rewriteHostTransport(base string) http.RoundTripper {
	return &rewriteHost{base: strings.TrimRight(base, "/")}
}

func (r *rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone request and rewrite host.
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	// Strip the original host and use the test server.
	base := strings.TrimRight(r.base, "/")
	// base is already "http://host:port"
	baseIdx := strings.Index(base, "://")
	if baseIdx >= 0 {
		hostPort := base[baseIdx+3:]
		clone.URL.Host = hostPort
	}
	clone.Host = clone.URL.Host
	return http.DefaultTransport.RoundTrip(clone)
}
