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

// testServerConfig configures the full Pocket Casts test server used by provider tests.
type testServerConfig struct {
	// inProgressEpisodes returned by /user/in_progress.
	inProgressEpisodes []map[string]any
	// podcastEpisodes returned by the cache CDN /podcast/full/{uuid}/... (keyed by
	// podcast UUID). Each entry uses the cache episode format: uuid, title, url,
	// duration, published (ISO 8601). No play-state fields.
	podcastEpisodes map[string][]map[string]any
	// updateCalls captures the JSON bodies sent to /sync/update_episode.
	updateCalls *[]map[string]any
	// subscribeCalls captures the podcast UUIDs sent to /user/podcast/subscribe.
	subscribeCalls *[]string
	// feedURLToUUID maps an RSS feed URL to the Pocket Casts UUID returned by
	// /author/add_feed_url. If a feed URL is not in this map the handler returns
	// status "error".
	feedURLToUUID map[string]string
	// subscribedPodcasts overrides the default two-podcast subscription list
	// returned by /user/podcast/list. If nil the default alpha+beta list is used.
	subscribedPodcasts []map[string]any
}

// newFullTestServer builds an httptest.Server that handles all endpoints
// used by the Provider, pointing pcBaseURL, pcCacheURL, and pcRefreshURL at it.
// It serves two subscribed podcasts (alpha, beta) and a configurable set of
// in-progress episodes, per-podcast episode lists, subscribe calls, and
// feed-URL-to-UUID resolution.
func newFullTestServer(t *testing.T, cfg testServerConfig) func() {
	t.Helper()
	if cfg.updateCalls == nil {
		empty := []map[string]any{}
		cfg.updateCalls = &empty
	}
	if cfg.subscribeCalls == nil {
		empty := []string{}
		cfg.subscribeCalls = &empty
	}

	mux := http.NewServeMux()

	// Login — returns JSON Bearer token.
	mux.HandleFunc("/user/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"test-token","uuid":"test-uuid"}`))
	})

	// Subscribed podcasts — use override if provided, otherwise default alpha+beta.
	mux.HandleFunc("/user/podcast/list", func(w http.ResponseWriter, r *http.Request) {
		pods := cfg.subscribedPodcasts
		if pods == nil {
			pods = []map[string]any{
				{"uuid": "pod1", "title": "Alpha Show", "author": "AuthorA",
					"url": "https://feeds.example.com/alpha"},
				{"uuid": "pod2", "title": "Beta Show", "author": "AuthorB",
					"url": "https://feeds.example.com/beta"},
			}
		}
		payload := map[string]any{"podcasts": pods}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	// In-progress episodes.
	mux.HandleFunc("/user/in_progress", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{"episodes": cfg.inProgressEpisodes}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	// Cache CDN per-podcast episode metadata: GET /podcast/full/{uuid}/{page}/3/1000
	mux.HandleFunc("/podcast/full/", func(w http.ResponseWriter, r *http.Request) {
		// Extract podcast UUID from path: /podcast/full/{uuid}/{page}/3/1000
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/podcast/full/"), "/")
		podUUID := ""
		if len(parts) > 0 {
			podUUID = parts[0]
		}
		eps := cfg.podcastEpisodes[podUUID]
		payload := map[string]any{
			"podcast": map[string]any{
				"uuid":     podUUID,
				"episodes": eps,
			},
			"has_more_episodes": false,
			"episode_count":     len(eps),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	// Update play state.
	mux.HandleFunc("/sync/update_episode", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*cfg.updateCalls = append(*cfg.updateCalls, body)
		w.WriteHeader(http.StatusOK)
	})

	// Subscribe to a podcast by PC UUID.
	mux.HandleFunc("/user/podcast/subscribe", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UUID string `json:"uuid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*cfg.subscribeCalls = append(*cfg.subscribeCalls, body.UUID)
		w.WriteHeader(http.StatusOK)
	})

	// Resolve RSS feed URL → Pocket Casts UUID (public, no auth).
	mux.HandleFunc("/author/add_feed_url", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		feedURL, _ := body["url"].(string)
		uuid := ""
		if cfg.feedURLToUUID != nil {
			uuid = cfg.feedURLToUUID[feedURL]
		}
		w.Header().Set("Content-Type", "application/json")
		if uuid == "" {
			_, _ = w.Write([]byte(`{"status":"error","message":"feed not found"}`))
			return
		}
		resp := map[string]any{
			"status": "ok",
			"result": map[string]any{
				"podcast": map[string]any{"uuid": uuid},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv := httptest.NewServer(mux)
	pocketcasts.SetBaseURLForTest(srv.URL)
	pocketcasts.SetCacheURLForTest(srv.URL)
	pocketcasts.SetRefreshURLForTest(srv.URL)
	pocketcasts.SetPollIntervalForTest(0) // no sleep between feed-resolve poll attempts
	return func() {
		srv.Close()
		pocketcasts.SetBaseURLForTest("https://api.pocketcasts.com")
		pocketcasts.SetCacheURLForTest("https://cache.pocketcasts.com")
		pocketcasts.SetRefreshURLForTest("https://refresh.pocketcasts.com")
		pocketcasts.SetPollIntervalForTest(2 * time.Second)
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
				"published_at":   "2024-05-01T09:00:00Z",
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
				"published_at": "2024-01-01T00:00:00Z", "duration": 1000,
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
				"published_at":   "2024-04-10T08:00:00Z",
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
				"published_at":   "2024-04-10T08:00:00Z",
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
	if call["podcast"] != "pod1" {
		t.Errorf("podcast = %v, want pod1", call["podcast"])
	}
	if int(call["status"].(float64)) != pocketcasts.PlayingPlayed {
		t.Errorf("status = %v, want %d (played)", call["status"], pocketcasts.PlayingPlayed)
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
				"published_at":   "2024-04-10T08:00:00Z",
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
	// Phase B must fetch the per-podcast cache episode list and find it there.
	var updateCalls []map[string]any
	restore := newFullTestServer(t, testServerConfig{
		inProgressEpisodes: []map[string]any{}, // nothing in-progress
		podcastEpisodes: map[string][]map[string]any{
			// Cache CDN format: uuid, title, url, duration, published (no play state).
			"pod2": {
				{
					"uuid":      "ep-pod2-1",
					"title":     "Beta Episode",
					"url":       "https://cdn.example.com/beta-ep.mp3",
					"duration":  2700,
					"published": "2024-02-20T12:00:00Z",
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
				"published_at": "2024-03-01T00:00:00Z", "duration": 100,
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

func TestProvider_SetLibrary_OnlySubscriptions_Succeeds(t *testing.T) {
	// With an empty library there are no podcasts to subscribe to — the call
	// should return immediately with nil (no error).
	restore := newFullTestServer(t, testServerConfig{})
	defer restore()

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{OnlySubscriptions: true}
	if err := p.SetLibrary(context.Background(), &model.Library{}, opts); err != nil {
		t.Fatalf("SetLibrary OnlySubscriptions empty library: unexpected error: %v", err)
	}
}

func TestProvider_SetLibrary_SubscribesNewPodcast(t *testing.T) {
	// lib has a podcast (gamma) that is NOT in the server's existing subscriptions
	// (alpha + beta). The provider should resolve the feed URL to a PC UUID and
	// call /user/podcast/subscribe with that UUID.
	var subscribeCalls []string
	restore := newFullTestServer(t, testServerConfig{
		feedURLToUUID:  map[string]string{"https://feeds.example.com/gamma": "pod3"},
		subscribeCalls: &subscribeCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/gamma", Title: "Gamma Show"},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{
		OnlySubscriptions: true,
		RequestDelay:      time.Millisecond,
	}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary subscribe new: %v", err)
	}

	if len(subscribeCalls) != 1 {
		t.Fatalf("subscribe calls: got %d, want 1", len(subscribeCalls))
	}
	if subscribeCalls[0] != "pod3" {
		t.Errorf("subscribed UUID = %q, want pod3", subscribeCalls[0])
	}
}

func TestProvider_SetLibrary_AlreadySubscribedURLMismatch_SkipsSubscribe(t *testing.T) {
	// PC has "Gamma Show" subscribed under a DIFFERENT URL than the Apple source.
	// The resolve endpoint maps the Apple URL to the same PC UUID → already subscribed.
	// Expected: 0 /user/podcast/subscribe calls.
	var subscribeCalls []string
	restore := newFullTestServer(t, testServerConfig{
		subscribedPodcasts: []map[string]any{
			// PC stores the podcast under "gamma-pc" URL, not the Apple "gamma" URL.
			{"uuid": "pod3", "title": "Gamma Show", "author": "AuthorG",
				"url": "https://feeds.example.com/gamma-pc"},
		},
		// The refresh API resolves the Apple URL to the same UUID.
		feedURLToUUID:  map[string]string{"https://feeds.example.com/gamma": "pod3"},
		subscribeCalls: &subscribeCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/gamma", Title: "Gamma Show"},
		},
	}
	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{
		OnlySubscriptions: true,
		RequestDelay:      time.Millisecond,
	}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary URL-mismatch skip: %v", err)
	}
	if len(subscribeCalls) != 0 {
		t.Errorf("expected 0 subscribe calls (UUID already subscribed); got %d: %v", len(subscribeCalls), subscribeCalls)
	}
}

func TestProvider_SetLibrary_PhaseBURLMismatch_FindsEpisodes(t *testing.T) {
	// Apple source episode uses FeedURL "https://feeds.example.com/gamma".
	// PC subscription stores the same podcast under "https://feeds.example.com/gamma-pc".
	// Phase B must resolve the Apple URL to PC UUID and index the episode correctly.
	var updateCalls []map[string]any
	restore := newFullTestServer(t, testServerConfig{
		subscribedPodcasts: []map[string]any{
			{"uuid": "pod3", "title": "Gamma Show", "author": "AuthorG",
				"url": "https://feeds.example.com/gamma-pc"}, // PC's URL differs
		},
		// The cache CDN has episodes for pod3.
		podcastEpisodes: map[string][]map[string]any{
			"pod3": {
				{
					"uuid":      "ep-gamma-1",
					"title":     "Gamma Episode One",
					"url":       "https://cdn.example.com/gamma-ep1.mp3",
					"duration":  3000,
					"published": "2024-03-10T09:00:00Z",
				},
			},
		},
		// Refresh API resolves the Apple URL to the PC UUID.
		feedURLToUUID: map[string]string{"https://feeds.example.com/gamma": "pod3"},
		updateCalls:   &updateCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/gamma", Title: "Gamma Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/gamma", // Apple URL
				Title:     "Gamma Episode One",
				PubDate:   time.Date(2024, 3, 10, 9, 0, 0, 0, time.UTC),
				Duration:  3000 * time.Second,
				PlayState: model.PlayStatePlayed,
			},
		},
	}
	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{RequestDelay: time.Millisecond}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary Phase B URL mismatch: %v", err)
	}
	if len(updateCalls) != 1 {
		t.Fatalf("expected 1 update call; got %d", len(updateCalls))
	}
	if updateCalls[0]["uuid"] != "ep-gamma-1" {
		t.Errorf("update uuid = %v, want ep-gamma-1", updateCalls[0]["uuid"])
	}
}

func TestProvider_SetLibrary_PodcastFilter_LimitsSubscriptions(t *testing.T) {
	// Source has two unsubscribed podcasts (gamma, delta). --podcast "gamma"
	// should subscribe only to gamma, not delta.
	var subscribeCalls []string
	restore := newFullTestServer(t, testServerConfig{
		feedURLToUUID: map[string]string{
			"https://feeds.example.com/gamma": "pod3",
			"https://feeds.example.com/delta": "pod4",
		},
		subscribeCalls: &subscribeCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/gamma", Title: "Gamma Show"},
			{FeedURL: "https://feeds.example.com/delta", Title: "Delta Show"},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{
		OnlySubscriptions: true,
		RequestDelay:      time.Millisecond,
		PodcastFilter:     []string{"gamma"},
	}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary with podcast filter: %v", err)
	}

	if len(subscribeCalls) != 1 {
		t.Fatalf("subscribe calls: got %d, want 1 (only gamma)", len(subscribeCalls))
	}
	if subscribeCalls[0] != "pod3" {
		t.Errorf("subscribed UUID = %q, want pod3 (gamma)", subscribeCalls[0])
	}
}

func TestProvider_SetLibrary_SubscribedOnly_SkipsSubscribeStep(t *testing.T) {
	// With --subscribed-only, doWriteSubscriptions must not be called even when
	// the lib contains a podcast that is not yet in the PC account.
	var subscribeCalls []string
	restore := newFullTestServer(t, testServerConfig{
		feedURLToUUID:  map[string]string{"https://feeds.example.com/gamma": "pod3"},
		subscribeCalls: &subscribeCalls,
		// In-progress + podcastEpisodes empty: no play-state writes expected either.
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/gamma", Title: "Gamma Show"},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{
		SubscribedOnly: true,
		RequestDelay:   time.Millisecond,
	}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary SubscribedOnly: %v", err)
	}

	if len(subscribeCalls) != 0 {
		t.Errorf("--subscribed-only: expected 0 subscribe calls, got %d (%v)", len(subscribeCalls), subscribeCalls)
	}
}

func TestProvider_SetLibrary_SkipsAlreadySubscribed(t *testing.T) {
	// lib has a podcast (alpha) that IS already in the server's existing
	// subscriptions. The provider should not call /user/podcast/subscribe.
	var subscribeCalls []string
	restore := newFullTestServer(t, testServerConfig{
		subscribeCalls: &subscribeCalls,
	})
	defer restore()

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
	}

	p := pocketcasts.NewProvider("user@example.com", "pass")
	opts := provider.WriteOptions{
		OnlySubscriptions: true,
		RequestDelay:      time.Millisecond,
	}
	if err := p.SetLibrary(context.Background(), lib, opts); err != nil {
		t.Fatalf("SetLibrary skip already-subscribed: %v", err)
	}

	if len(subscribeCalls) != 0 {
		t.Errorf("subscribe calls: got %d, want 0 (alpha already subscribed)", len(subscribeCalls))
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
	if !caps.WriteSubscriptions {
		t.Error("WriteSubscriptions should be true")
	}
}

func TestProvider_Name(t *testing.T) {
	p := pocketcasts.NewProvider("u", "p")
	if p.Name() != "Pocket Casts" {
		t.Errorf("Name() = %q, want %q", p.Name(), "Pocket Casts")
	}
}
