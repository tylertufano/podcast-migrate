package pocketcasts_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/pocketcasts"
)

// newTestServer returns a test server with the given handler and sets the
// package base URL to point at it. The caller must call restore() when done.
func newTestServer(t *testing.T, handler http.Handler) (client *http.Client, restore func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	pocketcasts.SetBaseURLForTest(srv.URL)
	return srv.Client(), func() {
		srv.Close()
		pocketcasts.SetBaseURLForTest("https://play.pocketcasts.com")
	}
}

// loginServer returns a handler that serves a successful login (sets a cookie)
// and delegates all other requests to next.
func loginServer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/sign_in" {
			http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "testsession", Path: "/"})
			w.WriteHeader(http.StatusOK)
			return
		}
		if next != nil {
			next.ServeHTTP(w, r)
		}
	})
}

// authedClient returns an http.Client that has already authenticated against
// the currently-configured test server (pcBaseURL must already be set via
// newTestServer before calling this). The /users/sign_in handler must be
// registered on the active test server.
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
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Login: expected POST, got %s", r.Method)
		}
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok123", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	_, restore := newTestServer(t, mux)
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
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// No cookie — invalid credentials: response contains error text
		_, _ = w.Write([]byte("Invalid email or password"))
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	_, err := pocketcasts.Login(ctx, "bad@example.com", "wrong")
	if err == nil {
		t.Fatal("Login with invalid credentials: expected error, got nil")
	}
}

func TestLogin_NoCookie_ReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		// Return 200 but no cookie — simulates a broken login flow.
		w.WriteHeader(http.StatusOK)
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	_, err := pocketcasts.Login(ctx, "user@example.com", "pass")
	if err == nil {
		t.Fatal("Login with no cookie: expected error, got nil")
	}
}

// ---- FetchSubscribedPodcasts ----

func TestFetchSubscribedPodcasts_ReturnsPodcasts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/podcasts/all.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("FetchSubscribedPodcasts: expected POST, got %s", r.Method)
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
	_, restore := newTestServer(t, mux)
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
		t.Errorf("pods[0].UUID = %q, want %q", pods[0].UUID, "pod1")
	}
	if pods[0].URL != "https://feeds.example.com/test" {
		t.Errorf("pods[0].URL = %q, want RSS feed URL", pods[0].URL)
	}
}

func TestFetchSubscribedPodcasts_Empty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/podcasts/all.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"podcasts":[]}`))
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	pods, err := pocketcasts.FetchSubscribedPodcasts(ctx, client)
	if err != nil {
		t.Fatalf("FetchSubscribedPodcasts empty: %v", err)
	}
	if len(pods) != 0 {
		t.Errorf("got %d podcasts, want 0", len(pods))
	}
}

func TestFetchSubscribedPodcasts_RateLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/podcasts/all.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	_, err := pocketcasts.FetchSubscribedPodcasts(ctx, client)
	var rl *pocketcasts.RateLimitError
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
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/episodes/in_progress_episodes.json", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{
			"episodes": []map[string]any{
				{
					"uuid":           "ep-uuid-1",
					"podcast_uuid":   "pod1",
					"title":          "Episode One",
					"url":            "https://cdn.example.com/ep1.mp3",
					"published_at":   "2024-03-15 10:00:00",
					"duration":       3600,
					"playing_status": 2,
					"played_up_to":   900,
					"starred":        false,
					"is_deleted":     false,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	eps, err := pocketcasts.FetchInProgressEpisodes(ctx, client)
	if err != nil {
		t.Fatalf("FetchInProgressEpisodes: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d episodes, want 1", len(eps))
	}
	ep := eps[0]
	if ep.UUID != "ep-uuid-1" {
		t.Errorf("UUID = %q, want %q", ep.UUID, "ep-uuid-1")
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

// ---- FetchPodcastEpisodes ----

func TestFetchPodcastEpisodes_SinglePage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/episodes/find_by_podcast.json", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if r.FormValue("uuid") != "pod-abc" {
			t.Errorf("uuid = %q, want %q", r.FormValue("uuid"), "pod-abc")
		}
		if r.FormValue("page") != "1" {
			t.Errorf("page = %q, want %q", r.FormValue("page"), "1")
		}
		payload := map[string]any{
			"result": map[string]any{
				"total": 2,
				"episodes": []map[string]any{
					{"uuid": "ep1", "podcast_uuid": "pod-abc", "title": "Ep 1",
						"published_at": "2024-01-10 08:00:00", "duration": 1800,
						"playing_status": 3, "played_up_to": 1800},
					{"uuid": "ep2", "podcast_uuid": "pod-abc", "title": "Ep 2",
						"published_at": "2024-01-03 08:00:00", "duration": 2400,
						"playing_status": 0, "played_up_to": 0},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	eps, total, err := pocketcasts.FetchPodcastEpisodes(ctx, client, "pod-abc", 1)
	if err != nil {
		t.Fatalf("FetchPodcastEpisodes: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(eps) != 2 {
		t.Fatalf("len(eps) = %d, want 2", len(eps))
	}
	if eps[0].UUID != "ep1" {
		t.Errorf("eps[0].UUID = %q, want ep1", eps[0].UUID)
	}
}

// ---- UpdateEpisodeProgress ----

func TestUpdateEpisodeProgress_Played(t *testing.T) {
	var gotUUID, gotPodUUID string
	var gotStatus, gotPlayedUpTo, gotDuration int

	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/episodes/update_episode_position.json", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UUID          string `json:"uuid"`
			PodcastUUID   string `json:"podcast_uuid"`
			PlayingStatus int    `json:"playing_status"`
			PlayedUpTo    int    `json:"played_up_to"`
			Duration      int    `json:"duration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		gotUUID = body.UUID
		gotPodUUID = body.PodcastUUID
		gotStatus = body.PlayingStatus
		gotPlayedUpTo = body.PlayedUpTo
		gotDuration = body.Duration
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	err := pocketcasts.UpdateEpisodeProgress(ctx, client, "ep-uuid", "pod-uuid",
		pocketcasts.PlayingPlayed, 3600, 3600)
	if err != nil {
		t.Fatalf("UpdateEpisodeProgress: %v", err)
	}
	if gotUUID != "ep-uuid" {
		t.Errorf("uuid = %q, want ep-uuid", gotUUID)
	}
	if gotPodUUID != "pod-uuid" {
		t.Errorf("podcast_uuid = %q, want pod-uuid", gotPodUUID)
	}
	if gotStatus != pocketcasts.PlayingPlayed {
		t.Errorf("playing_status = %d, want %d (played)", gotStatus, pocketcasts.PlayingPlayed)
	}
	if gotPlayedUpTo != 3600 {
		t.Errorf("played_up_to = %d, want 3600", gotPlayedUpTo)
	}
	if gotDuration != 3600 {
		t.Errorf("duration = %d, want 3600", gotDuration)
	}
}

func TestUpdateEpisodeProgress_InProgress(t *testing.T) {
	var gotStatus, gotPlayedUpTo int

	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/episodes/update_episode_position.json", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PlayingStatus int `json:"playing_status"`
			PlayedUpTo    int `json:"played_up_to"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotStatus = body.PlayingStatus
		gotPlayedUpTo = body.PlayedUpTo
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	err := pocketcasts.UpdateEpisodeProgress(ctx, client, "ep-uuid", "pod-uuid",
		pocketcasts.PlayingInProgress, 450, 1800)
	if err != nil {
		t.Fatalf("UpdateEpisodeProgress in-progress: %v", err)
	}
	if gotStatus != pocketcasts.PlayingInProgress {
		t.Errorf("playing_status = %d, want %d", gotStatus, pocketcasts.PlayingInProgress)
	}
	if gotPlayedUpTo != 450 {
		t.Errorf("played_up_to = %d, want 450", gotPlayedUpTo)
	}
}

func TestUpdateEpisodeProgress_RateLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/episodes/update_episode_position.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	err := pocketcasts.UpdateEpisodeProgress(ctx, client, "e", "p", pocketcasts.PlayingPlayed, 100, 100)
	var rl *pocketcasts.RateLimitError
	if !isRateLimitError(err, &rl) {
		t.Fatalf("expected RateLimitError, got: %v", err)
	}
	if rl.Wait != 60*time.Second {
		t.Errorf("Wait = %v, want 60s", rl.Wait)
	}
}

func TestUpdateEpisodeProgress_TransientError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/episodes/update_episode_position.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	err := pocketcasts.UpdateEpisodeProgress(ctx, client, "e", "p", pocketcasts.PlayingPlayed, 100, 100)
	var te *pocketcasts.TransientError
	if !isTransientError(err, &te) {
		t.Fatalf("expected TransientError for 5xx, got: %v", err)
	}
}

func TestUpdateEpisodeProgress_ServerErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/web/episodes/update_episode_position.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","message":"something went wrong"}`))
	})
	_, restore := newTestServer(t, mux)
	defer restore()

	ctx := context.Background()
	client := authedClient(t)

	err := pocketcasts.UpdateEpisodeProgress(ctx, client, "e", "p", pocketcasts.PlayingPlayed, 100, 100)
	if err == nil {
		t.Fatal("expected error for status=error response, got nil")
	}
}

// ---- APIEpisode.ParsePublishedAt ----

func TestAPIEpisode_ParsePublishedAt_Valid(t *testing.T) {
	ep := pocketcasts.APIEpisode{PublishedAt: "2024-06-15 14:30:00"}
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

// ---- helpers ----

func isRateLimitError(err error, target **pocketcasts.RateLimitError) bool {
	return errors.As(err, target)
}

func isTransientError(err error, target **pocketcasts.TransientError) bool {
	return errors.As(err, target)
}
