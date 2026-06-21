package pocketcasts_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/httputil"
	"github.com/tyler/podcast-migrate/internal/pocketcasts"
	"google.golang.org/protobuf/encoding/protowire"
)

// newTestServer creates an httptest.Server with the given mux, points
// pcBaseURL, pcCacheURL, and pcRefreshURL at it, and returns a restore func.
// The caller must defer the restore func to reset the URLs and close the server.
func newTestServer(t *testing.T, mux *http.ServeMux) func() {
	t.Helper()
	srv := httptest.NewServer(mux)
	pocketcasts.SetBaseURLForTest(srv.URL)
	pocketcasts.SetCacheURLForTest(srv.URL)
	pocketcasts.SetRefreshURLForTest(srv.URL)
	return func() {
		srv.Close()
		pocketcasts.SetBaseURLForTest("https://api.pocketcasts.com")
		pocketcasts.SetCacheURLForTest("https://cache.pocketcasts.com")
		pocketcasts.SetRefreshURLForTest("https://refresh.pocketcasts.com")
	}
}

// loginHandler is an http.HandlerFunc that simulates a successful Pocket Casts
// login by returning a JSON Bearer token response.
func loginHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"token":"test-token","uuid":"test-uuid"}`))
}

// authedClient calls Login against the currently-active test server and returns
// the resulting *http.Client (which injects "Authorization: Bearer test-token"
// on every request). The /user/login route must be registered on the active
// test server before calling this.
func authedClient(t *testing.T) *http.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := pocketcasts.Login(ctx, "user@example.com", "password")
	if err != nil {
		t.Fatalf("authedClient Login: %v", err)
	}
	return client
}

// ---- Login ----

func TestLogin_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Login: expected POST, got %s", r.Method)
		}
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
			Scope    string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Scope != "mobile" {
			t.Errorf("Login: scope = %q, want mobile", body.Scope)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"tok123","uuid":"user-1"}`))
	})
	restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client, err := pocketcasts.Login(ctx, "user@example.com", "pass")
	if err != nil {
		t.Fatalf("Login: unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("Login: returned nil client")
	}
}

func TestLogin_InvalidCredentials(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid credentials"}`))
	})
	restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	_, err := pocketcasts.Login(ctx, "bad@example.com", "wrong")
	if err == nil {
		t.Fatal("Login with invalid credentials: expected error, got nil")
	}
}

func TestLogin_NoToken_ReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 but no token field — simulates a broken login response.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uuid":"user-1"}`))
	})
	restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	_, err := pocketcasts.Login(ctx, "user@example.com", "pass")
	if err == nil {
		t.Fatal("Login with no token: expected error, got nil")
	}
}

// ---- FetchSubscribedPodcasts ----

func TestFetchSubscribedPodcasts_ReturnsPodcasts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/podcast/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("FetchSubscribedPodcasts: expected POST, got %s", r.Method)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if v, _ := body["v"].(float64); v != 1 {
			t.Errorf("FetchSubscribedPodcasts: body[v] = %v, want 1", body["v"])
		}
		payload := map[string]any{
			"podcasts": []map[string]any{
				{"uuid": "pod1", "title": "Test Podcast", "author": "Author", "url": "https://feeds.example.com/test"},
				{"uuid": "pod2", "title": "Another Show", "author": "Someone", "url": "https://feeds.example.com/another"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	pods, err := pocketcasts.FetchSubscribedPodcasts(ctx, client)
	if err != nil {
		t.Fatalf("FetchSubscribedPodcasts: %v", err)
	}
	if len(pods) != 2 {
		t.Fatalf("FetchSubscribedPodcasts: got %d podcasts, want 2", len(pods))
	}
	if pods[0].UUID != "pod1" {
		t.Errorf("pods[0].UUID = %q, want pod1", pods[0].UUID)
	}
	if pods[0].URL != "https://feeds.example.com/test" {
		t.Errorf("pods[0].URL = %q, want RSS feed URL", pods[0].URL)
	}
}

func TestFetchSubscribedPodcasts_Empty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/podcast/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"podcasts":[]}`))
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	pods, err := pocketcasts.FetchSubscribedPodcasts(context.Background(), client)
	if err != nil {
		t.Fatalf("FetchSubscribedPodcasts empty: %v", err)
	}
	if len(pods) != 0 {
		t.Errorf("got %d podcasts, want 0", len(pods))
	}
}

func TestFetchSubscribedPodcasts_RateLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/podcast/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	_, err := pocketcasts.FetchSubscribedPodcasts(context.Background(), client)
	var rl *httputil.RateLimitError
	if !isRateLimitError(err, &rl) {
		t.Fatalf("expected RateLimitError, got: %v", err)
	}
	if rl.Wait != 30*time.Second {
		t.Errorf("RateLimitError.Wait = %v, want 30s", rl.Wait)
	}
}

// ---- FetchInProgressEpisodes ----

func TestFetchInProgressEpisodes_ReturnsEpisodes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/in_progress", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("FetchInProgressEpisodes: expected POST, got %s", r.Method)
		}
		payload := map[string]any{
			"episodes": []map[string]any{
				{
					"uuid":          "ep-uuid-1",
					"podcastUuid":   "pod1",
					"title":         "Episode One",
					"url":           "https://cdn.example.com/ep1.mp3",
					"published":     "2024-03-15T10:00:00Z",
					"duration":      3600,
					"playingStatus": 2,
					"playedUpTo":    900,
					"starred":       false,
					"isDeleted":     false,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	eps, err := pocketcasts.FetchInProgressEpisodes(context.Background(), client)
	if err != nil {
		t.Fatalf("FetchInProgressEpisodes: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d episodes, want 1", len(eps))
	}
	ep := eps[0]
	if ep.UUID != "ep-uuid-1" {
		t.Errorf("UUID = %q, want ep-uuid-1", ep.UUID)
	}
	if ep.PlayedUpTo != 900 {
		t.Errorf("PlayedUpTo = %d, want 900", ep.PlayedUpTo)
	}
	if ep.PlayingStatus != 2 {
		t.Errorf("PlayingStatus = %d, want 2 (in-progress)", ep.PlayingStatus)
	}
	pub := ep.ParsePublishedAt()
	want := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	if !pub.Equal(want) {
		t.Errorf("ParsePublishedAt() = %v, want %v", pub, want)
	}
}

// ---- FetchPlayedEpisodes ----

func TestFetchPlayedEpisodes_ReturnsEpisodes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("FetchPlayedEpisodes: expected POST, got %s", r.Method)
		}
		// NOTE: /user/history returns camelCase field names, NOT the snake_case
		// used by /user/in_progress.  Keep this test payload exactly as the
		// real endpoint sends it so we catch any future regression.
		payload := map[string]any{
			"episodes": []map[string]any{
				{
					"uuid":          "ep-played-1",
					"podcastUuid":   "pod1",           // camelCase
					"title":         "Episode Played",
					"url":           "https://cdn.example.com/ep_played.mp3",
					"published":     "2024-04-10T10:00:00Z", // "published", not "published_at"
					"duration":      1800,
					"playingStatus": 3,               // camelCase
					"playedUpTo":    1800,             // camelCase
					"starred":       false,
					"isDeleted":     false,            // camelCase
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	eps, err := pocketcasts.FetchPlayedEpisodes(context.Background(), client)
	if err != nil {
		t.Fatalf("FetchPlayedEpisodes: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d episodes, want 1", len(eps))
	}
	ep := eps[0]
	if ep.UUID != "ep-played-1" {
		t.Errorf("UUID = %q, want ep-played-1", ep.UUID)
	}
	// PodcastUUID is the most important field: it drives feedURL resolution in
	// Phase A2.  An empty value here means all history episodes get silently
	// skipped.  Verifying it catches the snake_case / camelCase JSON mismatch.
	if ep.PodcastUUID != "pod1" {
		t.Errorf("PodcastUUID = %q, want pod1 (camelCase podcastUuid mismatch?)", ep.PodcastUUID)
	}
	if ep.PublishedAt != "2024-04-10T10:00:00Z" {
		t.Errorf("PublishedAt = %q, want 2024-04-10T10:00:00Z (\"published\" field mismatch?)", ep.PublishedAt)
	}
	if ep.PlayingStatus != 3 {
		t.Errorf("PlayingStatus = %d, want 3 (played) — camelCase playingStatus mismatch?", ep.PlayingStatus)
	}
	if ep.PlayedUpTo != 1800 {
		t.Errorf("PlayedUpTo = %d, want 1800 — camelCase playedUpTo mismatch?", ep.PlayedUpTo)
	}
}

// ---- FetchPodcastEpisodes ----

func TestFetchPodcastEpisodes_SinglePage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	// Cache CDN endpoint: GET /podcast/full/{uuid}/{page}/3/1000
	mux.HandleFunc("/podcast/full/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("FetchPodcastEpisodes: expected GET, got %s", r.Method)
		}
		// /podcast/full/pod-abc/0/3/1000
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/podcast/full/"), "/")
		if len(parts) < 1 || parts[0] != "pod-abc" {
			t.Errorf("unexpected path UUID in %s", r.URL.Path)
		}
		payload := map[string]any{
			"podcast": map[string]any{
				"uuid": "pod-abc",
				"episodes": []map[string]any{
					{"uuid": "ep1", "title": "Ep 1", "published": "2024-01-10T08:00:00Z", "duration": 1800},
					{"uuid": "ep2", "title": "Ep 2", "published": "2024-01-03T08:00:00Z", "duration": 2400},
				},
			},
			"has_more_episodes": false,
			"episode_count":     2,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	eps, hasMore, err := pocketcasts.FetchPodcastEpisodes(context.Background(), client, "pod-abc", 0)
	if err != nil {
		t.Fatalf("FetchPodcastEpisodes: %v", err)
	}
	if hasMore {
		t.Errorf("hasMore = true, want false")
	}
	if len(eps) != 2 {
		t.Fatalf("len(eps) = %d, want 2", len(eps))
	}
	if eps[0].UUID != "ep1" {
		t.Errorf("eps[0].UUID = %q, want ep1", eps[0].UUID)
	}
	// PodcastUUID is injected from the request parameter, not the response body.
	if eps[0].PodcastUUID != "pod-abc" {
		t.Errorf("eps[0].PodcastUUID = %q, want pod-abc", eps[0].PodcastUUID)
	}
	// Cache CDN has no play state — expect unplayed.
	if eps[0].PlayingStatus != pocketcasts.PlayingUnplayed {
		t.Errorf("eps[0].PlayingStatus = %d, want %d (unplayed)", eps[0].PlayingStatus, pocketcasts.PlayingUnplayed)
	}
}

func TestFetchPodcastEpisodes_HasMore(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/podcast/full/", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{
			"podcast": map[string]any{
				"uuid": "pod-xyz",
				"episodes": []map[string]any{
					{"uuid": "ep1", "title": "Ep 1", "published": "2024-01-10T08:00:00Z", "duration": 1800},
				},
			},
			"has_more_episodes": true, // more pages exist
			"episode_count":     500,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	_, hasMore, err := pocketcasts.FetchPodcastEpisodes(context.Background(), client, "pod-xyz", 0)
	if err != nil {
		t.Fatalf("FetchPodcastEpisodes: %v", err)
	}
	if !hasMore {
		t.Error("hasMore = false, want true")
	}
}

// ---- UpdateEpisodeProgress ----

func TestUpdateEpisodeProgress_Played(t *testing.T) {
	var gotUUID, gotPodcast string
	var gotStatus, gotPosition, gotDuration int

	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/sync/update_episode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("UpdateEpisodeProgress: expected POST, got %s", r.Method)
		}
		var body struct {
			UUID     string `json:"uuid"`
			Podcast  string `json:"podcast"`
			Status   int    `json:"status"`
			Position int    `json:"position"`
			Duration int    `json:"duration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		gotUUID = body.UUID
		gotPodcast = body.Podcast
		gotStatus = body.Status
		gotPosition = body.Position
		gotDuration = body.Duration
		w.WriteHeader(http.StatusOK)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	err := pocketcasts.UpdateEpisodeProgress(context.Background(), client, "ep-uuid", "pod-uuid",
		pocketcasts.PlayingPlayed, 3600, 3600)
	if err != nil {
		t.Fatalf("UpdateEpisodeProgress: %v", err)
	}
	if gotUUID != "ep-uuid" {
		t.Errorf("uuid = %q, want ep-uuid", gotUUID)
	}
	if gotPodcast != "pod-uuid" {
		t.Errorf("podcast = %q, want pod-uuid", gotPodcast)
	}
	if gotStatus != pocketcasts.PlayingPlayed {
		t.Errorf("status = %d, want %d (played)", gotStatus, pocketcasts.PlayingPlayed)
	}
	if gotPosition != 3600 {
		t.Errorf("position = %d, want 3600", gotPosition)
	}
	if gotDuration != 3600 {
		t.Errorf("duration = %d, want 3600", gotDuration)
	}
}

func TestUpdateEpisodeProgress_InProgress(t *testing.T) {
	var gotStatus, gotPosition, gotDuration int

	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/sync/update_episode", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status   int `json:"status"`
			Position int `json:"position"`
			Duration int `json:"duration"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotStatus = body.Status
		gotPosition = body.Position
		gotDuration = body.Duration
		w.WriteHeader(http.StatusOK)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	err := pocketcasts.UpdateEpisodeProgress(context.Background(), client, "ep-uuid", "pod-uuid",
		pocketcasts.PlayingInProgress, 450, 1800)
	if err != nil {
		t.Fatalf("UpdateEpisodeProgress in-progress: %v", err)
	}
	if gotStatus != pocketcasts.PlayingInProgress {
		t.Errorf("status = %d, want %d (in-progress)", gotStatus, pocketcasts.PlayingInProgress)
	}
	if gotPosition != 450 {
		t.Errorf("position = %d, want 450", gotPosition)
	}
	if gotDuration != 1800 {
		t.Errorf("duration = %d, want 1800", gotDuration)
	}
}

func TestUpdateEpisodeProgress_RateLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/sync/update_episode", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	err := pocketcasts.UpdateEpisodeProgress(context.Background(), client, "e", "p", pocketcasts.PlayingPlayed, 100, 100)
	var rl *httputil.RateLimitError
	if !isRateLimitError(err, &rl) {
		t.Fatalf("expected RateLimitError, got: %v", err)
	}
	if rl.Wait != 60*time.Second {
		t.Errorf("Wait = %v, want 60s", rl.Wait)
	}
}

func TestUpdateEpisodeProgress_TransientError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/sync/update_episode", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	err := pocketcasts.UpdateEpisodeProgress(context.Background(), client, "e", "p", pocketcasts.PlayingPlayed, 100, 100)
	var te *httputil.TransientError
	if !isTransientError(err, &te) {
		t.Fatalf("expected TransientError for 5xx, got: %v", err)
	}
}

func TestUpdateEpisodeProgress_BadRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/sync/update_episode", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"invalid episode uuid"}`))
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	err := pocketcasts.UpdateEpisodeProgress(context.Background(), client, "bad", "pod", pocketcasts.PlayingPlayed, 0, 0)
	if err == nil {
		t.Fatal("expected error for 400 response, got nil")
	}
	// 400 is a permanent error, not transient.
	var te *httputil.TransientError
	if errors.As(err, &te) {
		t.Error("400 should not be wrapped as TransientError")
	}
}

// ---- APIEpisode.ParsePublishedAt ----

func TestAPIEpisode_ParsePublishedAt_Valid(t *testing.T) {
	ep := pocketcasts.APIEpisode{PublishedAt: "2024-06-15T14:30:00Z"}
	got := ep.ParsePublishedAt()
	want := time.Date(2024, 6, 15, 14, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParsePublishedAt() = %v, want %v", got, want)
	}
}

func TestAPIEpisode_ParsePublishedAt_Empty(t *testing.T) {
	ep := pocketcasts.APIEpisode{PublishedAt: ""}
	got := ep.ParsePublishedAt()
	if !got.IsZero() {
		t.Errorf("ParsePublishedAt() empty = %v, want zero time", got)
	}
}

func TestAPIEpisode_ParsePublishedAt_Invalid(t *testing.T) {
	ep := pocketcasts.APIEpisode{PublishedAt: "not-a-date"}
	got := ep.ParsePublishedAt()
	if !got.IsZero() {
		t.Errorf("ParsePublishedAt() invalid = %v, want zero time", got)
	}
}

// ---- ResolveFeedToPodcastUUID ----

func TestResolveFeedToPodcastUUID_ImmediateOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/author/add_feed_url", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["url"] != "https://feeds.example.com/mypodcast" {
			t.Errorf("url = %v, want feed URL", body["url"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","result":{"podcast":{"uuid":"pc-uuid-123","title":"My Podcast"}}}`))
	})
	restore := newTestServer(t, mux)
	defer restore()

	uuid, err := pocketcasts.ResolveFeedToPodcastUUID(context.Background(), "https://feeds.example.com/mypodcast")
	if err != nil {
		t.Fatalf("ResolveFeedToPodcastUUID: %v", err)
	}
	if uuid != "pc-uuid-123" {
		t.Errorf("uuid = %q, want pc-uuid-123", uuid)
	}
}

func TestResolveFeedToPodcastUUID_PollThenOK(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/author/add_feed_url", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"status":"poll","poll_uuid":"poll-token-abc"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["poll_uuid"] != "poll-token-abc" {
			t.Errorf("call %d: poll_uuid = %v, want poll-token-abc", calls, body["poll_uuid"])
		}
		_, _ = w.Write([]byte(`{"status":"ok","result":{"podcast":{"uuid":"pc-uuid-456"}}}`))
	})
	restore := newTestServer(t, mux)
	defer restore()
	// Set poll interval to 0 so the test doesn't sleep 2 seconds between attempts.
	pocketcasts.SetPollIntervalForTest(0)
	t.Cleanup(func() { pocketcasts.SetPollIntervalForTest(2 * time.Second) })

	uuid, err := pocketcasts.ResolveFeedToPodcastUUID(context.Background(), "https://feeds.example.com/slow")
	if err != nil {
		t.Fatalf("ResolveFeedToPodcastUUID poll: %v", err)
	}
	if uuid != "pc-uuid-456" {
		t.Errorf("uuid = %q, want pc-uuid-456", uuid)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one poll + one ok)", calls)
	}
}

func TestResolveFeedToPodcastUUID_ErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/author/add_feed_url", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error"}`))
	})
	restore := newTestServer(t, mux)
	defer restore()

	_, err := pocketcasts.ResolveFeedToPodcastUUID(context.Background(), "https://feeds.example.com/bad")
	if err == nil {
		t.Fatal("expected error for status=error, got nil")
	}
}

// ---- SubscribePodcast ----

func TestSubscribePodcast_Success(t *testing.T) {
	var gotUUID string
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/podcast/subscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var body struct {
			UUID string `json:"uuid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotUUID = body.UUID
		w.WriteHeader(http.StatusOK)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	err := pocketcasts.SubscribePodcast(context.Background(), client, "pc-uuid-789")
	if err != nil {
		t.Fatalf("SubscribePodcast: %v", err)
	}
	if gotUUID != "pc-uuid-789" {
		t.Errorf("uuid = %q, want pc-uuid-789", gotUUID)
	}
}

func TestSubscribePodcast_RateLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/podcast/subscribe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	err := pocketcasts.SubscribePodcast(context.Background(), client, "uuid")
	var rl *httputil.RateLimitError
	if !isRateLimitError(err, &rl) {
		t.Fatalf("expected RateLimitError, got: %v", err)
	}
	if rl.Wait != 45*time.Second {
		t.Errorf("Wait = %v, want 45s", rl.Wait)
	}
}

// ---- FetchSyncUpdate ----

func TestFetchSyncUpdate_ReturnsEpisodes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/sync/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("Content-Type = %q, want application/octet-stream", ct)
		}
		resp := buildWebTestSyncResponse(42000, []pocketcasts.SyncEpisodeState{
			{EpisodeUUID: "ep-1", PodcastUUID: "pod-a", PlayingStatus: pocketcasts.PlayingPlayed, PlayedUpTo: 3600},
			{EpisodeUUID: "ep-2", PodcastUUID: "pod-a", PlayingStatus: pocketcasts.PlayingInProgress, PlayedUpTo: 900},
		})
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(resp)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	eps, lastMod, err := pocketcasts.FetchSyncUpdate(context.Background(), client, 0)
	if err != nil {
		t.Fatalf("FetchSyncUpdate: %v", err)
	}

	if lastMod != 42000 {
		t.Errorf("lastModified = %d, want 42000", lastMod)
	}
	if len(eps) != 2 {
		t.Fatalf("len(episodes) = %d, want 2", len(eps))
	}

	ep1 := eps[0]
	if ep1.EpisodeUUID != "ep-1" {
		t.Errorf("ep[0].EpisodeUUID = %q, want ep-1", ep1.EpisodeUUID)
	}
	if ep1.PodcastUUID != "pod-a" {
		t.Errorf("ep[0].PodcastUUID = %q, want pod-a", ep1.PodcastUUID)
	}
	if ep1.PlayingStatus != pocketcasts.PlayingPlayed {
		t.Errorf("ep[0].PlayingStatus = %d, want %d (played)", ep1.PlayingStatus, pocketcasts.PlayingPlayed)
	}
	if ep1.PlayedUpTo != 3600 {
		t.Errorf("ep[0].PlayedUpTo = %d, want 3600", ep1.PlayedUpTo)
	}

	ep2 := eps[1]
	if ep2.PlayingStatus != pocketcasts.PlayingInProgress {
		t.Errorf("ep[1].PlayingStatus = %d, want %d (in-progress)", ep2.PlayingStatus, pocketcasts.PlayingInProgress)
	}
	if ep2.PlayedUpTo != 900 {
		t.Errorf("ep[1].PlayedUpTo = %d, want 900", ep2.PlayedUpTo)
	}
}

func TestFetchSyncUpdate_EmptyResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/sync/update", func(w http.ResponseWriter, r *http.Request) {
		resp := buildWebTestSyncResponse(0, nil)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(resp)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	eps, _, err := pocketcasts.FetchSyncUpdate(context.Background(), client, 0)
	if err != nil {
		t.Fatalf("FetchSyncUpdate: %v", err)
	}
	if len(eps) != 0 {
		t.Errorf("len(episodes) = %d, want 0", len(eps))
	}
}

func TestFetchSyncUpdate_ServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/sync/update", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	_, _, err := pocketcasts.FetchSyncUpdate(context.Background(), client, 0)
	if err == nil {
		t.Fatal("expected error from 500 response, got nil")
	}
}

func TestFetchSyncUpdate_DeltaSync_SendsLastModified(t *testing.T) {
	var receivedLastMod int64
	mux := http.NewServeMux()
	mux.HandleFunc("/user/login", loginHandler)
	mux.HandleFunc("/user/sync/update", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Parse field 2 (last_modified) from the request body.
		for len(body) > 0 {
			num, typ, n := protowire.ConsumeTag(body)
			body = body[n:]
			if typ == protowire.VarintType {
				v, n2 := protowire.ConsumeVarint(body)
				body = body[n2:]
				if num == 2 {
					receivedLastMod = int64(v)
				}
			} else {
				_, n2 := protowire.ConsumeBytes(body)
				if n2 < 0 {
					break
				}
				body = body[n2:]
			}
		}
		resp := buildWebTestSyncResponse(99999, nil)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(resp)
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := authedClient(t)
	_, _, err := pocketcasts.FetchSyncUpdate(context.Background(), client, 12345)
	if err != nil {
		t.Fatalf("FetchSyncUpdate: %v", err)
	}
	if receivedLastMod != 12345 {
		t.Errorf("last_modified in request = %d, want 12345", receivedLastMod)
	}
}

// buildWebTestSyncResponse builds a minimal SyncUpdateResponse binary for use
// in web tests. Mirrors the helper in provider_test.go but scoped locally to
// avoid cross-file helper duplication.
func buildWebTestSyncResponse(lastModified int64, episodes []pocketcasts.SyncEpisodeState) []byte {
	var b []byte
	if lastModified != 0 {
		b = protowire.AppendTag(b, 1, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(lastModified))
	}
	for _, ep := range episodes {
		epMsg := buildWebTestSyncUserEpisode(ep)
		var rec []byte
		rec = protowire.AppendTag(rec, 2, protowire.BytesType)
		rec = protowire.AppendBytes(rec, epMsg)
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, rec)
	}
	return b
}

func buildWebTestSyncUserEpisode(ep pocketcasts.SyncEpisodeState) []byte {
	var b []byte
	if ep.EpisodeUUID != "" {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, []byte(ep.EpisodeUUID))
	}
	if ep.PodcastUUID != "" {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, []byte(ep.PodcastUUID))
	}
	if ep.PlayingStatus != 0 {
		var w []byte
		w = protowire.AppendTag(w, 1, protowire.VarintType)
		w = protowire.AppendVarint(w, uint64(ep.PlayingStatus))
		b = protowire.AppendTag(b, 7, protowire.BytesType)
		b = protowire.AppendBytes(b, w)
	}
	if ep.PlayedUpTo != 0 {
		var w []byte
		w = protowire.AppendTag(w, 1, protowire.VarintType)
		w = protowire.AppendVarint(w, uint64(ep.PlayedUpTo))
		b = protowire.AppendTag(b, 9, protowire.BytesType)
		b = protowire.AppendBytes(b, w)
	}
	return b
}

// ---- helpers ----

func isRateLimitError(err error, target **httputil.RateLimitError) bool {
	return errors.As(err, target)
}

func isTransientError(err error, target **httputil.TransientError) bool {
	return errors.As(err, target)
}
