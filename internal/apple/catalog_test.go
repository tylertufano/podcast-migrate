package apple

// White-box tests for CatalogClient — the iTunes Search → amp-api cascade.
// Uses httptest servers so no real network calls are made.

import (
	"context"
	"encoding/json"
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
