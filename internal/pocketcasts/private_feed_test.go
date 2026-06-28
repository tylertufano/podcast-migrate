package pocketcasts_test

// Tests for IsPrivate=true podcast handling in doWriteSubscriptions:
//   - Private feeds use feed URL (not iTunes ID) for resolution
//   - Successful resolution → subscribe normally
//   - Failed resolution → write to skipped-feeds OPML

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/pocketcasts"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// privateFeedTestServer builds a minimal httptest.Server for private-feed tests.
// It records subscribe calls and maps feed URLs to PC UUIDs via add_feed_url.
// A /podcasts/show handler returns a different UUID ("wrong-pod") so that if
// the iTunes ID fast path is incorrectly used, the wrong subscription fires.
func privateFeedTestServer(t *testing.T, feedURLToUUID map[string]string, subscribeCalls *[]string) func() {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/user/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"test-token","uuid":"u1"}`))
	})

	mux.HandleFunc("/user/podcast/list", func(w http.ResponseWriter, _ *http.Request) {
		// No existing subscriptions — everything in the source library is new.
		payload := map[string]any{"podcasts": []map[string]any{}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	mux.HandleFunc("/user/podcast/subscribe", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UUID string `json:"uuid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*subscribeCalls = append(*subscribeCalls, body.UUID)
		w.WriteHeader(http.StatusOK)
	})

	// feed URL → PC UUID resolution (add_feed_url).
	mux.HandleFunc("/author/add_feed_url", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		feedURL, _ := body["url"].(string)
		uuid := feedURLToUUID[feedURL]
		w.Header().Set("Content-Type", "application/json")
		if uuid == "" {
			_, _ = w.Write([]byte(`{"status":"error","message":"feed not found"}`))
			return
		}
		resp := map[string]any{
			"status": "ok",
			"result": map[string]any{"podcast": map[string]any{"uuid": uuid}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// iTunes ID → PC UUID (podcasts/show). Returns "wrong-pod" so tests can
	// detect if the iTunes-ID fast path is incorrectly taken.
	mux.HandleFunc("/podcasts/show", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"status": "ok",
			"result": map[string]any{"podcast": map[string]any{"uuid": "wrong-pod"}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv := httptest.NewServer(mux)
	pocketcasts.SetBaseURLForTest(srv.URL)
	pocketcasts.SetRefreshURLForTest(srv.URL)
	pocketcasts.SetPollIntervalForTest(0)
	return func() {
		srv.Close()
		pocketcasts.SetBaseURLForTest("https://api.pocketcasts.com")
		pocketcasts.SetRefreshURLForTest("https://refresh.pocketcasts.com")
		pocketcasts.SetPollIntervalForTest(2 * time.Second)
	}
}

// TestProvider_SetLibrary_PrivateFeed_SkipsITunesIDFastPath verifies that when
// IsPrivate=true, the provider resolves via feed URL (add_feed_url) rather than
// the iTunes ID fast path, even when an iTunes ID is present.
//
// The /podcasts/show handler returns "wrong-pod"; the feed URL maps to "correct-pod".
// Correct behaviour: subscribeCalls contains only "correct-pod".
func TestProvider_SetLibrary_PrivateFeed_SkipsITunesIDFastPath(t *testing.T) {
	var subscribeCalls []string
	restore := privateFeedTestServer(t,
		map[string]string{"https://feeds.example.com/private": "correct-pod"},
		&subscribeCalls,
	)
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{
				FeedURL:   "https://feeds.example.com/private",
				Title:     "Private Show",
				ITunesID:  "999888777", // present — but should be ignored when IsPrivate
				IsPrivate: true,
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{OnlySubscriptions: true, RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}

	if len(subscribeCalls) != 1 {
		t.Fatalf("expected 1 subscribe call, got %d: %v", len(subscribeCalls), subscribeCalls)
	}
	if subscribeCalls[0] == "wrong-pod" {
		t.Error("provider used iTunes ID fast path (got wrong-pod); should have used feed URL")
	}
	if subscribeCalls[0] != "correct-pod" {
		t.Errorf("subscribe UUID = %q, want correct-pod", subscribeCalls[0])
	}
}

// TestProvider_SetLibrary_PrivateFeed_ResolvesSuccessfully verifies that
// IsPrivate=true podcasts are subscribed normally when add_feed_url succeeds.
func TestProvider_SetLibrary_PrivateFeed_ResolvesSuccessfully(t *testing.T) {
	var subscribeCalls []string
	restore := privateFeedTestServer(t,
		map[string]string{"https://kvs.example.com/subscriber-feed": "sub-pod"},
		&subscribeCalls,
	)
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{
				FeedURL:   "https://kvs.example.com/subscriber-feed",
				Title:     "Subscriber Show",
				IsPrivate: true,
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{OnlySubscriptions: true, RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}

	if len(subscribeCalls) != 1 {
		t.Fatalf("expected 1 subscribe call, got %d", len(subscribeCalls))
	}
	if subscribeCalls[0] != "sub-pod" {
		t.Errorf("subscribe UUID = %q, want sub-pod", subscribeCalls[0])
	}
}

// TestProvider_SetLibrary_PrivateFeed_ResolveFails_WritesSkippedOPML verifies
// that when add_feed_url fails for an IsPrivate=true feed, the podcast is
// collected in the skipped-feeds OPML rather than silently dropped.
func TestProvider_SetLibrary_PrivateFeed_ResolveFails_WritesSkippedOPML(t *testing.T) {
	var subscribeCalls []string
	// No feedURLToUUID entries → add_feed_url returns "error" for all URLs.
	restore := privateFeedTestServer(t, nil, &subscribeCalls)
	defer restore()

	skippedPath := filepath.Join(t.TempDir(), "skipped.opml")

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{
				FeedURL:   "https://kvs.example.com/auth-gated",
				Title:     "Auth Gated Show",
				IsPrivate: true,
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	p.SetSkippedOPMLPath(skippedPath)
	opts := provider.WriteOptions{OnlySubscriptions: true, RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}

	// No subscription should have been attempted.
	if len(subscribeCalls) != 0 {
		t.Errorf("expected 0 subscribe calls for failed resolution, got %d: %v", len(subscribeCalls), subscribeCalls)
	}

	// Skipped OPML must exist and contain the feed URL.
	data, err := os.ReadFile(skippedPath)
	if err != nil {
		t.Fatalf("skipped OPML not written: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "https://kvs.example.com/auth-gated") {
		t.Errorf("skipped OPML does not contain feed URL; content:\n%s", content)
	}
	if !strings.Contains(content, "Auth Gated Show") {
		t.Errorf("skipped OPML does not contain podcast title; content:\n%s", content)
	}
}

// TestProvider_SetLibrary_PrivateFeed_MixedSuccess verifies that when a source
// library has both resolvable and unresolvable private feeds, the resolvable
// one is subscribed and the unresolvable one ends up in the skipped OPML.
func TestProvider_SetLibrary_PrivateFeed_MixedSuccess(t *testing.T) {
	var subscribeCalls []string
	restore := privateFeedTestServer(t,
		map[string]string{
			"https://kvs.example.com/good-private": "good-pod",
			// bad-private URL intentionally omitted → add_feed_url returns error
		},
		&subscribeCalls,
	)
	defer restore()

	skippedPath := filepath.Join(t.TempDir(), "skipped.opml")

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://kvs.example.com/good-private", Title: "Good Private", IsPrivate: true},
			{FeedURL: "https://kvs.example.com/bad-private", Title: "Bad Private", IsPrivate: true},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	p.SetSkippedOPMLPath(skippedPath)
	opts := provider.WriteOptions{OnlySubscriptions: true, RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}

	if len(subscribeCalls) != 1 || subscribeCalls[0] != "good-pod" {
		t.Errorf("subscribe calls = %v, want [good-pod]", subscribeCalls)
	}

	data, err := os.ReadFile(skippedPath)
	if err != nil {
		t.Fatalf("skipped OPML not written: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "Good Private") {
		t.Error("skipped OPML should not contain successfully subscribed feed")
	}
	if !strings.Contains(content, "Bad Private") {
		t.Errorf("skipped OPML should contain unresolvable feed; content:\n%s", content)
	}
}

// TestProvider_SetLibrary_PublicFeed_ResolveFails_DoesNotWriteSkippedOPML verifies
// that a failed resolution for a public (IsPrivate=false) feed is logged as a
// warning but does NOT go into the skipped OPML, matching pre-existing behaviour.
func TestProvider_SetLibrary_PublicFeed_ResolveFails_DoesNotWriteSkippedOPML(t *testing.T) {
	var subscribeCalls []string
	// No feedURLToUUID → add_feed_url returns error for all URLs.
	restore := privateFeedTestServer(t, nil, &subscribeCalls)
	defer restore()

	skippedPath := filepath.Join(t.TempDir(), "skipped.opml")

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{
				FeedURL:   "https://feeds.example.com/unknown-public",
				Title:     "Unknown Public Show",
				ITunesID:  "", // no iTunes ID so fast path is not taken
				IsPrivate: false,
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	p.SetSkippedOPMLPath(skippedPath)
	opts := provider.WriteOptions{OnlySubscriptions: true, RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}

	// The file should NOT exist: public feed failures are warnings, not OPML entries.
	if _, err := os.Stat(skippedPath); err == nil {
		data, _ := os.ReadFile(skippedPath)
		t.Errorf("skipped OPML should not be written for public feed failures; content:\n%s", data)
	}
}
