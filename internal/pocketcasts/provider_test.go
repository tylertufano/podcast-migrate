package pocketcasts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/pocketcasts"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// fullTestServer builds an httptest.Server that handles all endpoints used by
// the Provider. It serves two subscribed podcasts and a configurable set of
// in-progress and per-podcast episodes.
type testServerConfig struct {
	// inProgressEpisodes returned by in_progress_episodes.json.
	inProgressEpisodes []map[string]any
	// podcastEpisodes returned by find_by_podcast.json (keyed by podcast UUID).
	podcastEpisodes map[string][]map[string]any
	// updateCalls captures the JSON bodies sent to update_episode_position.json.
	updateCalls *[]map[string]any
}

func newFullTestServer(t *testing.T, cfg testServerConfig) func() {
	t.Helper()
	if cfg.updateCalls == nil {
		empty := []map[string]any{}
		cfg.updateCalls = &empty
	}

	mux := http.NewServeMux()

	// Login.
	mux.HandleFunc("/users/sign_in", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_pocketcasts_session", Value: "tok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})

	// Subscribed podcasts.
	mux.HandleFunc("/web/podcasts/all.json", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{
			"podcasts": []map[string]any{
				{"uuid": "pod1", "title": "Alpha Show", "author": "AuthorA",
					"url": "https://feeds.example.com/alpha"},
				{"uuid": "pod2", "title": "Beta Show", "author": "AuthorB",
					"url": "https://feeds.example.com/beta"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	// In-progress episodes.
	mux.HandleFunc("/web/episodes/in_progress_episodes.json", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{"episodes": cfg.inProgressEpisodes}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	// Per-podcast episode list.
	mux.HandleFunc("/web/episodes/find_by_podcast.json", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		podUUID := r.FormValue("uuid")
		eps := cfg.podcastEpisodes[podUUID]
		payload := map[string]any{
			"result": map[string]any{
				"total":    len(eps),
				"episodes": eps,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	// Update play state.
	mux.HandleFunc("/web/episodes/update_episode_position.json", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*cfg.updateCalls = append(*cfg.updateCalls, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := httptest.NewServer(mux)
	pocketcasts.SetBaseURLForTest(srv.URL)
	return func() {
		srv.Close()
		pocketcasts.SetBaseURLForTest("https://play.pocketcasts.com")
	}
}

// ---- GetLibrary ----

func TestProvider_GetLibrary_ReturnsPodcastsAndInProgress(t *testing.T) {
	restore := newFullTestServer(t, testServerConfig{
		inProgressEpisodes: []map[string]any{
			{
				"uuid":           "ep-ip-1",
				"podcast_uuid":   "pod1",
				"title":          "Ep In Progress",
				"published_at":   "2024-05-01 09:00:00",
				"duration":       3600,
				"playing_status": 2,
				"played_up_to":   600,
				"is_deleted":     false,
			},
		},
	})
	defer restore()

	p := pocketcasts.NewProvider("user@example.com", "pass")
	lib, err := p.GetLibrary(context.Background())
	if err != nil {
		t.Fatalf("GetLibrary: %v", err)
	}

	// Two subscribed podcasts.
	if len(lib.Podcasts) != 2 {
		t.Errorf("Podcasts: got %d, want 2", len(lib.Podcasts))
	}
	// One in-progress episode.
	if len(lib.Episodes) != 1 {
		t.Fatalf("Episodes: got %d, want 1", len(lib.Episodes))
	}
	ep := lib.Episodes[0]
	if ep.FeedURL != "https://feeds.example.com/alpha" {
		t.Errorf("FeedURL = %q, want alpha feed URL", ep.FeedURL)
	}
	if ep.PlayState != model.PlayStateInProgress {
		t.Errorf("PlayState = %v, want InProgress", ep.PlayState)
	}
	if ep.PlayPosition != 600*time.Second {
		t.Errorf("PlayPosition = %v, want 600s", ep.PlayPosition)
	}
}

func TestProvider_GetLibrary_SkipsDeletedEpisodes(t *testing.T) {
	restore := newFullTestServer(t, testServerConfig{
		inProgressEpisodes: []map[string]any{
			{"uuid": "ep-del", "podcast_uuid": "pod1", "title": "Deleted",
				"published_at": "2024-01-01 00:00:00", "duration": 1000,
				"playing_status": 2, "played_up_to": 100, "is_deleted": true},
		},
	})
	defer restore()

	p := pocketcasts.NewProvider("user@example.com", "pass")
	lib, err := p.GetLibrary(context.Background())
	if err != nil {
		t.Fatalf("GetLibrary: %v", err)
	}
	if len(lib.Episodes) != 0 {
		t.Errorf("deleted episode should be excluded; got %d episodes", len(lib.Episodes))
	}
}

// ---- SetLibrary (dry-run) ----

func TestProvider_SetLibrary_DryRun_ReportsWouldUpdate(t *testing.T) {
	var updateCalls []map[string]any
	restore := newFullTestServer(t, testServerConfig{
		inProgressEpisodes: []map[string]any{
			{
				"uuid":           "ep-pc-1",
				"podcast_uuid":   "pod1",
				"title":          "Episode One",
				"published_at":   "2024-04-10 08:00:00",
				"duration":       1800,
				"playing_status": 2,
				"played_up_to":   300,
				"is_deleted":     false,
			},
		},
		updateCalls: &updateCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/alpha",
				Title:     "Episode One",
				PubDate:   time.Date(2024, 4, 10, 8, 0, 0, 0, time.UTC),
				PlayState: model.PlayStatePlayed,
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{
		DryRun:       true,
		RequestDelay: time.Millisecond,
	}
	err := p.SetLibrary(context.Background(), lib, opts)
	if err != nil {
		t.Fatalf("SetLibrary dry-run: %v", err)
	}
	// Dry-run must not call the update endpoint.
	if len(updateCalls) != 0 {
		t.Errorf("dry-run should not call update endpoint; got %d calls", len(updateCalls))
	}
}

// ---- SetLibrary (live write) ----

func TestProvider_SetLibrary_WritesPlayedEpisode(t *testing.T) {
	var updateCalls []map[string]any
	restore := newFullTestServer(t, testServerConfig{
		inProgressEpisodes: []map[string]any{
			{
				"uuid":           "ep-pc-1",
				"podcast_uuid":   "pod1",
				"title":          "Episode One",
				"published_at":   "2024-04-10 08:00:00",
				"duration":       3600,
				"playing_status": 2, // in-progress in PC
				"played_up_to":   500,
				"is_deleted":     false,
			},
		},
		updateCalls: &updateCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/alpha",
				Title:     "Episode One",
				PubDate:   time.Date(2024, 4, 10, 8, 0, 0, 0, time.UTC),
				Duration:  3600 * time.Second,
				PlayState: model.PlayStatePlayed,
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}

	if len(updateCalls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(updateCalls))
	}
	call := updateCalls[0]
	if call["uuid"] != "ep-pc-1" {
		t.Errorf("uuid = %v, want ep-pc-1", call["uuid"])
	}
	if call["podcast_uuid"] != "pod1" {
		t.Errorf("podcast_uuid = %v, want pod1", call["podcast_uuid"])
	}
	if int(call["playing_status"].(float64)) != pocketcasts.PlayingPlayed {
		t.Errorf("playing_status = %v, want %d (played)", call["playing_status"], pocketcasts.PlayingPlayed)
	}
}

func TestProvider_SetLibrary_SkipsAlreadyPlayed(t *testing.T) {
	var updateCalls []map[string]any
	restore := newFullTestServer(t, testServerConfig{
		inProgressEpisodes: []map[string]any{
			{
				"uuid":           "ep-pc-1",
				"podcast_uuid":   "pod1",
				"title":          "Episode One",
				"published_at":   "2024-04-10 08:00:00",
				"duration":       3600,
				"playing_status": 3, // already played in PC
				"played_up_to":   3600,
				"is_deleted":     false,
			},
		},
		updateCalls: &updateCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/alpha",
				Title:     "Episode One",
				PubDate:   time.Date(2024, 4, 10, 8, 0, 0, 0, time.UTC),
				PlayState: model.PlayStatePlayed,
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}
	if len(updateCalls) != 0 {
		t.Errorf("already-played episode should be skipped; got %d update call(s)", len(updateCalls))
	}
}

func TestProvider_SetLibrary_PhaseB_FetchesPerPodcastEpisodes(t *testing.T) {
	// Source episode is NOT in the in-progress list (never opened in PC).
	// Phase B must fetch the per-podcast episode list and find it there.
	var updateCalls []map[string]any
	restore := newFullTestServer(t, testServerConfig{
		inProgressEpisodes: []map[string]any{}, // nothing in-progress
		podcastEpisodes: map[string][]map[string]any{
			"pod2": {
				{
					"uuid":           "ep-pod2-1",
					"podcast_uuid":   "pod2",
					"title":          "Beta Episode",
					"published_at":   "2024-02-20 12:00:00",
					"duration":       2700,
					"playing_status": 0,
					"played_up_to":   0,
					"is_deleted":     false,
				},
			},
		},
		updateCalls: &updateCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/beta", Title: "Beta Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/beta",
				Title:     "Beta Episode",
				PubDate:   time.Date(2024, 2, 20, 12, 0, 0, 0, time.UTC),
				Duration:  2700 * time.Second,
				PlayState: model.PlayStatePlayed,
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary Phase B: %v", err)
	}
	if len(updateCalls) != 1 {
		t.Fatalf("Phase B: expected 1 update call, got %d", len(updateCalls))
	}
	if updateCalls[0]["uuid"] != "ep-pod2-1" {
		t.Errorf("Phase B: uuid = %v, want ep-pod2-1", updateCalls[0]["uuid"])
	}
}

func TestProvider_SetLibrary_NotFound_LoggedCorrectly(t *testing.T) {
	restore := newFullTestServer(t, testServerConfig{
		inProgressEpisodes: []map[string]any{},
		// No per-podcast episodes either — the episode cannot be found.
	})
	defer restore()

	var logBuf bytes.Buffer
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/alpha",
				Title:     "Missing Episode",
				PubDate:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				PlayState: model.PlayStatePlayed,
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{
		RequestDelay: time.Millisecond,
		LogWriter:    &logBuf,
	}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary not-found: %v", err)
	}

	log := logBuf.String()
	if !strings.Contains(log, "not_found") {
		t.Errorf("log should contain 'not_found'; got:\n%s", log)
	}
	if !strings.Contains(log, "Missing Episode") {
		t.Errorf("log should contain episode title; got:\n%s", log)
	}
}

func TestProvider_SetLibrary_SkipsFromDestinationEpisodes(t *testing.T) {
	var updateCalls []map[string]any
	restore := newFullTestServer(t, testServerConfig{
		inProgressEpisodes: []map[string]any{
			{"uuid": "ep-pc-1", "podcast_uuid": "pod1", "title": "From PC",
				"published_at": "2024-03-01 00:00:00", "duration": 100,
				"playing_status": 2, "played_up_to": 50, "is_deleted": false},
		},
		updateCalls: &updateCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:         "https://feeds.example.com/alpha",
				Title:           "From PC",
				PubDate:         time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
				PlayState:       model.PlayStateInProgress,
				PlayPosition:    50 * time.Second,
				FromDestination: true, // originated from PC itself
			},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary FromDestination: %v", err)
	}
	if len(updateCalls) != 0 {
		t.Errorf("FromDestination episode should be skipped; got %d update call(s)", len(updateCalls))
	}
}

func TestProvider_SetLibrary_OnlySubscriptions_ReturnsError(t *testing.T) {
	restore := newFullTestServer(t, testServerConfig{})
	defer restore()

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{OnlySubscriptions: true}
	err := p.SetLibrary(context.Background(), &model.Library{}, opts)
	if err == nil {
		t.Fatal("expected error for OnlySubscriptions on Pocket Casts provider, got nil")
	}
}

func TestProvider_Capabilities(t *testing.T) {
	p := pocketcasts.NewProvider("user@example.com", "pass")
	caps := p.Capabilities()
	if !caps.ReadSubscriptions {
		t.Error("ReadSubscriptions should be true")
	}
	if !caps.ReadPlayState {
		t.Error("ReadPlayState should be true")
	}
	if !caps.WritePlayState {
		t.Error("WritePlayState should be true")
	}
	if caps.WriteSubscriptions {
		t.Error("WriteSubscriptions should be false (Phase 1 limitation)")
	}
}

func TestProvider_Name(t *testing.T) {
	p := pocketcasts.NewProvider("u", "p")
	if p.Name() != "Pocket Casts" {
		t.Errorf("Name() = %q, want %q", p.Name(), "Pocket Casts")
	}
}
