package overcast

// White-box tests for augmentIndexFromPodcastPages.
// Uses an httptest.Server and sets overcastBaseURL to the server URL so all
// HTTP calls go to the mock instead of the real Overcast servers.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// setupAugmentServer registers handlers on a new httptest.Server, sets
// overcastBaseURL to the server URL, and redirects the episode ID cache to a
// temp directory. Both are restored via t.Cleanup.
func setupAugmentServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}
	srv := httptest.NewServer(mux)
	prevBase := overcastBaseURL
	overcastBaseURL = srv.URL
	// Redirect episode ID cache away from the real user cache directory.
	cachePath := filepath.Join(t.TempDir(), "test-episode-ids.json")
	setEpisodeCachePathForTest(cachePath)
	t.Cleanup(func() {
		srv.Close()
		overcastBaseURL = prevBase
		setEpisodeCachePathForTest("")
	})
	return srv
}

// Shared mock HTML strings ─────────────────────────────────────────────────

const (
	// /podcasts page with one subscribed podcast "Fresh Air".
	augPodcastsPageFreshAir = `<!DOCTYPE html><html><body>
<a class="feedcell" href="/itunes12345/fresh-air">
  <div class="titlestack"><div class="title">Fresh Air</div></div>
</a>
</body></html>`

	// /podcasts page with no subscribed podcasts.
	augPodcastsPageEmpty = `<!DOCTYPE html><html><body><p>No podcasts</p></body></html>`

	// Listing page for /itunes12345/fresh-air — episode on 2024-06-15, no NumericID.
	// Requires a separate /+HASH1 fetch to resolve the numeric ID.
	augListingPage = `<!DOCTYPE html><html><body>
<a class="extendedepisodecell" href="/+HASH1">
  <div>Test Episode<span class="caption2">Jun 15, 2024 • 45 min</span></div>
</a>
</body></html>`

	// Listing page where the episode cell carries data-item-id — no episode page fetch needed.
	augListingPageWithNumericID = `<!DOCTYPE html><html><body>
<a class="extendedepisodecell" data-item-id="9999" href="/+HASH1">
  <div>Test Episode<span class="caption2">Jun 15, 2024 • 45 min</span></div>
</a>
</body></html>`

	// Episode player page at /+HASH1 — carries data-item-id for the extended-match worker.
	augEpisodePage = `<html><body><div id="audioplayer" data-item-id="9999"></div></body></html>`
)

// appleEp is a played Apple episode on 2024-06-15 used across augment tests.
var appleEp = model.EpisodeState{
	FeedURL:   "https://feeds.npr.org/381444908/podcast.xml",
	Title:     "Test Episode",
	PubDate:   time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
	PlayState: model.PlayStatePlayed,
}

// ── Early-exit tests (no HTTP) ────────────────────────────────────────────

func TestAugment_EmptyEpisodes_ReturnsZero(t *testing.T) {
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), nil,
		&model.Library{},
		nil, // empty
		map[string]overcastIndexEntry{},
		0, map[string]string{},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 0 {
		t.Errorf("empty episodes: got %d, want 0", n)
	}
}

func TestAugment_AllUnplayed_ReturnsZero(t *testing.T) {
	eps := []model.EpisodeState{
		{FeedURL: "https://feeds.example.com/s", PubDate: time.Now(), PlayState: model.PlayStateUnplayed},
	}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), nil,
		&model.Library{},
		eps,
		map[string]overcastIndexEntry{},
		0,
		map[string]string{"https://feeds.example.com/s": "test show"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 0 {
		t.Errorf("all-unplayed: got %d, want 0", n)
	}
}

func TestAugment_AllAlreadyIndexed_ReturnsZero(t *testing.T) {
	feedURL := appleEp.FeedURL
	normFeed := normalizeFeedURL(feedURL)
	key := "feeddate:" + normFeed + "|" + appleEp.PubDate.UTC().Format(time.RFC3339)
	index := map[string]overcastIndexEntry{key: {numericID: "already-there"}}

	n, _ := augmentIndexFromPodcastPages(
		context.Background(), nil,
		&model.Library{},
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{feedURL: "fresh air"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 0 {
		t.Errorf("all-indexed: got %d, want 0", n)
	}
}

// ── HTTP-dependent tests ──────────────────────────────────────────────────

func TestAugment_StrictFeedMatch_SkipsAllFeeds(t *testing.T) {
	// strictFeedMatch=true skips the cross-feed extended-matching loop.
	// /podcasts is still fetched (it runs before the loop), but no listing pages are hit.
	listingCalled := false
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageEmpty)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			listingCalled = true
			fmt.Fprint(w, augListingPage)
		},
	})

	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{appleEp},
		map[string]overcastIndexEntry{},
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		true, // strictFeedMatch
		false, false, 0, false, newTestCache(t),
	)
	if n != 0 {
		t.Errorf("strictFeedMatch: got %d, want 0", n)
	}
	if listingCalled {
		t.Error("listing page should not be fetched when strictFeedMatch=true")
	}
}

func TestAugment_SubscribedOnly_SkipsUnsubscribed(t *testing.T) {
	// /podcasts returns no matching podcast → the feed is not subscribed.
	// subscribedOnly=true → skip instead of search+subscribe.
	listingCalled := false
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageEmpty)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			listingCalled = true
			fmt.Fprint(w, augListingPage)
		},
	})

	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{appleEp},
		map[string]overcastIndexEntry{},
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false,
		true, // subscribedOnly
		false, 0, false, newTestCache(t),
	)
	if n != 0 {
		t.Errorf("subscribedOnly with no match: got %d, want 0", n)
	}
	if listingCalled {
		t.Error("listing page should not be fetched when subscribedOnly and not subscribed")
	}
}

func TestAugment_NormalPath_AddsToIndex(t *testing.T) {
	// Full happy path:
	//   /podcasts → "Fresh Air" at /itunes12345/fresh-air
	//   listing page → episode on 2024-06-15 (no NumericID)
	//   episode page /+HASH1 → data-item-id="9999"
	// Expected: 1 entry added to index.
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augListingPage)
		},
		"/+HASH1": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augEpisodePage)
		},
	})

	index := map[string]overcastIndexEntry{}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 1 {
		t.Errorf("normal path: got %d entries added, want 1", n)
	}

	normFeed := normalizeFeedURL(appleEp.FeedURL)
	key := "feeddate:" + normFeed + "|" + appleEp.PubDate.UTC().Format(time.RFC3339)
	entry, ok := index[key]
	if !ok {
		t.Fatalf("index missing expected key %q; keys: %v", key, indexEntryKeys(index))
	}
	if entry.numericID != "9999" {
		t.Errorf("numericID: got %q, want %q", entry.numericID, "9999")
	}
}

func TestAugment_NumericIDShortcut_AddsToIndex(t *testing.T) {
	// Listing page embeds data-item-id in the episode cell → direct insertion,
	// no /+HASH episode page fetch needed. This exercises the NumericID shortcut
	// in step 3 and validates the bug fix (returns added, not 0, when pending is empty).
	episodePageCalled := false
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augListingPageWithNumericID)
		},
		"/+HASH1": func(w http.ResponseWriter, r *http.Request) {
			episodePageCalled = true
			fmt.Fprint(w, augEpisodePage)
		},
	})

	index := map[string]overcastIndexEntry{}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 1 {
		t.Errorf("NumericID shortcut: got %d entries added, want 1", n)
	}
	if episodePageCalled {
		t.Error("episode player page should not be fetched when NumericID is in listing cell")
	}

	normFeed := normalizeFeedURL(appleEp.FeedURL)
	key := "feeddate:" + normFeed + "|" + appleEp.PubDate.UTC().Format(time.RFC3339)
	if entry, ok := index[key]; !ok || entry.numericID != "9999" {
		t.Errorf("index[%q]: got %+v (ok=%v), want numericID=9999", key, index[key], ok)
	}
}

func TestAugment_PodPageCacheDedup(t *testing.T) {
	// Two Apple feeds resolve to the same Overcast listing page via Plus-normalised
	// title matching: "fresh air" (direct) and "fresh air plus" (Plus-normalised to "fresh air").
	// The listing page must only be fetched once — the second feed serves from podPageCache.
	var listingFetchCount int32

	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&listingFetchCount, 1)
			fmt.Fprint(w, augListingPage)
		},
		"/+HASH1": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augEpisodePage)
		},
	})

	feed1 := "https://feeds.npr.org/381444908/podcast.xml"      // public "Fresh Air"
	feed2 := "https://feeds.npr.org/381444908/plus/podcast.xml" // "Fresh Air Plus"
	ep1 := model.EpisodeState{
		FeedURL:   feed1,
		Title:     "Ep A",
		PubDate:   time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC), // matches listing
		PlayState: model.PlayStatePlayed,
	}
	ep2 := model.EpisodeState{
		FeedURL:   feed2,
		Title:     "Ep B",
		PubDate:   time.Date(2024, 5, 10, 12, 0, 0, 0, time.UTC), // no listing match
		PlayState: model.PlayStatePlayed,
	}

	_, _ = augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{ep1, ep2},
		map[string]overcastIndexEntry{},
		0,
		map[string]string{feed1: "fresh air", feed2: "fresh air plus"},
		false, false, false, 0, false, newTestCache(t),
	)

	if count := atomic.LoadInt32(&listingFetchCount); count != 1 {
		t.Errorf("listing page fetched %d time(s), want exactly 1 (podPageCache should dedup)", count)
	}
}

func TestAugment_TitleFallback_MatchesWhenDateMisses(t *testing.T) {
	// Apple episode has a different pub date than what Overcast shows (e.g. timezone
	// difference in the RSS feed). Title-based fallback in step 3 should still match.
	//
	// Apple episode: 2024-06-16 (off by one day vs listing's 2024-06-15).
	// Listing shows: Jun 15, 2024 — date match fails.
	// Title match: "Test Episode" == listing cell title → match.
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augListingPage) // has "Jun 15, 2024" and title "Test Episode"
		},
		"/+HASH1": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augEpisodePage)
		},
	})

	epOffByTwoDays := model.EpisodeState{
		FeedURL:   appleEp.FeedURL,
		Title:     "Test Episode",
		PubDate:   time.Date(2024, 6, 17, 12, 0, 0, 0, time.UTC), // 2 days later — outside ±1 day tolerance
		PlayState: model.PlayStatePlayed,
	}

	index := map[string]overcastIndexEntry{}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{epOffByTwoDays},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 1 {
		t.Errorf("title fallback: got %d entries added, want 1", n)
	}
}

func TestAugment_OneDayOffSameTitle_Accepted(t *testing.T) {
	// ±1-day tolerance: Apple episode is published one day after the Overcast
	// listing date (timezone edge case — same episode, same title).
	// The title guard must NOT reject this match.
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			// listing has episode "Test Episode" on 2024-06-15
			fmt.Fprint(w, augListingPage)
		},
		"/+HASH1": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augEpisodePage)
		},
	})

	epOneDayLater := model.EpisodeState{
		FeedURL:   appleEp.FeedURL,
		Title:     "Test Episode", // same title as in augListingPage
		PubDate:   time.Date(2024, 6, 16, 12, 0, 0, 0, time.UTC), // +1 day vs listing's Jun 15
		PlayState: model.PlayStatePlayed,
	}

	index := map[string]overcastIndexEntry{}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{epOneDayLater},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 1 {
		t.Errorf("±1 day same title: got %d entries added, want 1 — should accept when titles match", n)
	}
}

func TestAugment_OneDayOffSeasonMarkerVariant_Accepted(t *testing.T) {
	// Feed variant: Apple stores the episode title without the "S01" marker that
	// Overcast shows in its listing cell. The ±1-day fuzzy-title guard must accept
	// this as the same episode rather than rejecting it as mismatched.
	//
	// Real-world pattern (Serial / The Retrievals):
	//   Apple (subscriber feed): "The Retrievals - Ep. 4" on 2023-08-17
	//   Overcast (public feed):  "The Retrievals S01 - Ep. 4" on 2023-08-17
	// Both normalise to "the retrievals ep 4" via FuzzyNormalizeTitle.
	const listingPageS01 = `<!DOCTYPE html><html><body>
<a class="extendedepisodecell" data-item-id="4444" href="/+HASH3">
  <div>The Retrievals S01 - Ep. 4<span class="caption2">Jun 16, 2024 • 40 min</span></div>
</a>
</body></html>`

	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			// listing shows "The Retrievals S01 - Ep. 4" on 2024-06-16
			fmt.Fprint(w, listingPageS01)
		},
	})

	// Apple stored the title without "S01", published one day earlier.
	epNoS01 := model.EpisodeState{
		FeedURL:   appleEp.FeedURL,
		Title:     "The Retrievals - Ep. 4",
		PubDate:   time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC), // one day before listing's Jun 16
		PlayState: model.PlayStatePlayed,
	}

	index := map[string]overcastIndexEntry{}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{epNoS01},
		index,
		0,
		// feedToTitle uses "fresh air" to match augPodcastsPageFreshAir →
		// /itunes12345/fresh-air, which serves listingPageS01 for this test.
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 1 {
		t.Errorf("±1 day season-marker variant: got %d entries added, want 1 — fuzzy title should accept", n)
	}
	// Confirm the correct numeric ID landed in the index.
	normFeed := normalizeFeedURL(appleEp.FeedURL)
	key := "feeddate:" + normFeed + "|" + epNoS01.PubDate.UTC().Format(time.RFC3339)
	if entry, ok := index[key]; !ok || entry.numericID != "4444" {
		t.Errorf("index[%q]: got %+v (ok=%v), want numericID=4444", key, index[key], ok)
	}
}

func TestAugment_OneDayOffDifferentTitle_Rejected(t *testing.T) {
	// Guard against the subscriber-feed false positive: an Apple subscriber
	// episode published one day before a *different* public episode should NOT
	// be matched via the ±1-day tolerance.
	//
	// Real-world example that triggered this bug:
	//   Apple (subscriber feed): "Pollercoaster: What the Primaries Tell Us…" on 2026-04-02
	//   Overcast (public feed):  "Bondi Gets the Boot" on 2026-04-03
	// The ±1-day window found "Bondi Gets the Boot" (date+1) and falsely marked
	// it as played in Overcast. The title guard prevents this.
	const listingPageDifferentTitle = `<!DOCTYPE html><html><body>
<a class="extendedepisodecell" data-item-id="7777" href="/+HASH2">
  <div>Bondi Gets the Boot<span class="caption2">Jun 16, 2024 • 30 min</span></div>
</a>
</body></html>`

	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			// listing has "Bondi Gets the Boot" on 2024-06-16
			fmt.Fprint(w, listingPageDifferentTitle)
		},
	})

	// Apple subscriber episode has a completely different title, published one day
	// before the public episode.
	epSubscriberExclusive := model.EpisodeState{
		FeedURL:   appleEp.FeedURL,
		Title:     "Pollercoaster: What the Primaries Tell Us About the Midterms, So Far",
		PubDate:   time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC), // 2024-06-15 vs listing's 2024-06-16
		PlayState: model.PlayStatePlayed,
	}

	index := map[string]overcastIndexEntry{}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{epSubscriberExclusive},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 0 {
		t.Errorf("±1 day different title: got %d entries added, want 0 — should reject when titles differ", n)
	}
	// Verify the wrong episode ID was not added to the index.
	normFeed := normalizeFeedURL(appleEp.FeedURL)
	key := "feeddate:" + normFeed + "|" + epSubscriberExclusive.PubDate.UTC().Format(time.RFC3339)
	if entry, ok := index[key]; ok {
		t.Errorf("index should not contain a match for the subscriber-exclusive episode; got numericID=%q", entry.numericID)
	}
}

// ── OPML play-state seeding tests ────────────────────────────────────────────
//
// These tests verify that augmentIndexFromPodcastPages uses the live OPML state
// (via buildOPMLNumericIDIndex) to seed currentState for augmented episodes, even
// when the episode ID cache is empty (first run). This fixes the bug where PC →
// Overcast migrations without a --overcast-match-opml file would show all augmented
// episodes as "unplayed" and trigger unnecessary writes.

func TestAugment_OPMLState_NumericIDShortcut_SeedsCurrentState(t *testing.T) {
	// The listing page provides data-item-id="9999" (no per-episode fetch).
	// The overcastLib has that same episode (GUID="9999") marked as played.
	// Expected: index entry has currentState=Played, so a "played" write is skipped.
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augListingPageWithNumericID) // data-item-id="9999"
		},
	})

	overcastLib := &model.Library{
		Episodes: []model.EpisodeState{
			{GUID: "9999", PlayState: model.PlayStatePlayed},
		},
	}

	index := map[string]overcastIndexEntry{}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		overcastLib,
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 1 {
		t.Fatalf("expected 1 entry added, got %d", n)
	}

	normFeed := normalizeFeedURL(appleEp.FeedURL)
	key := "feeddate:" + normFeed + "|" + appleEp.PubDate.UTC().Format(time.RFC3339)
	entry, ok := index[key]
	if !ok {
		t.Fatalf("index missing key %q", key)
	}
	if entry.currentState != model.PlayStatePlayed {
		t.Errorf("currentState: got %v, want PlayStatePlayed — OPML state should seed the index even on first run", entry.currentState)
	}
}

func TestAugment_OPMLState_CacheHitPath_SeedsCurrentState(t *testing.T) {
	// The episode URL is in the ID cache (cache hit path, Pass A) but the written
	// state in the cache is Unplayed (e.g. the cache entry was written before any
	// play-state write). The OPML has the same numeric ID as Played.
	// Expected: currentState=Played (OPML takes precedence over empty cache state).
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augListingPage) // no data-item-id → pending list
		},
	})

	// Pre-populate the cache: id="9999", no written state (writtenState=Unplayed).
	// The episode URL must use the test server's base URL since OvercastURL is
	// constructed as overcastBaseURL+hash, and overcastBaseURL is set to srv.URL in tests.
	episodeURL := srv.URL + "/+HASH1"
	cache := newTestCache(t)
	cache.set(episodeURL, "9999")

	overcastLib := &model.Library{
		Episodes: []model.EpisodeState{
			{GUID: "9999", PlayState: model.PlayStatePlayed},
		},
	}

	index := map[string]overcastIndexEntry{}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		overcastLib,
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, cache,
	)
	if n != 1 {
		t.Fatalf("expected 1 entry added, got %d", n)
	}

	normFeed := normalizeFeedURL(appleEp.FeedURL)
	key := "feeddate:" + normFeed + "|" + appleEp.PubDate.UTC().Format(time.RFC3339)
	entry, ok := index[key]
	if !ok {
		t.Fatalf("index missing key %q", key)
	}
	if entry.currentState != model.PlayStatePlayed {
		t.Errorf("currentState: got %v, want PlayStatePlayed — OPML state should seed cache-hit entries", entry.currentState)
	}
}

func TestAugment_OPMLState_WorkerPath_SeedsCurrentState(t *testing.T) {
	// Episode is not in the cache; worker fetches /+HASH1 and resolves numericID="9999".
	// The OPML has that episode marked as InProgress(30m).
	// Expected: currentState=InProgress, currentPos=30m.
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augListingPage) // no data-item-id
		},
		"/+HASH1": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augEpisodePage) // data-item-id="9999"
		},
	})

	wantPos := 30 * time.Minute
	overcastLib := &model.Library{
		Episodes: []model.EpisodeState{
			{GUID: "9999", PlayState: model.PlayStateInProgress, PlayPosition: wantPos},
		},
	}

	index := map[string]overcastIndexEntry{}
	n, _ := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		overcastLib,
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, newTestCache(t),
	)
	if n != 1 {
		t.Fatalf("expected 1 entry added, got %d", n)
	}

	normFeed := normalizeFeedURL(appleEp.FeedURL)
	key := "feeddate:" + normFeed + "|" + appleEp.PubDate.UTC().Format(time.RFC3339)
	entry, ok := index[key]
	if !ok {
		t.Fatalf("index missing key %q", key)
	}
	if entry.currentState != model.PlayStateInProgress {
		t.Errorf("currentState: got %v, want PlayStateInProgress", entry.currentState)
	}
	if entry.currentPos != wantPos {
		t.Errorf("currentPos: got %v, want %v", entry.currentPos, wantPos)
	}
}

func TestAugment_OPMLState_FurthestWins_CacheAhead(t *testing.T) {
	// The cache records a previous write of "played"; the OPML shows "unplayed"
	// (e.g. Overcast hasn't reflected the write yet, or the OPML entry is stale).
	// Expected: currentState=Played (cache wins because it's further).
	srv := setupAugmentServer(t, map[string]http.HandlerFunc{
		"/podcasts": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augPodcastsPageFreshAir)
		},
		"/itunes12345/fresh-air": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, augListingPage)
		},
	})

	// Cache has id="9999" with written state=Played. URL must use the test server's
	// base URL since OvercastURL is built as overcastBaseURL+hash in FetchPodcastEpisodes.
	episodeURL := srv.URL + "/+HASH1"
	cache := newTestCache(t)
	cache.set(episodeURL, "9999")
	cache.setWrittenState(episodeURL, model.PlayStatePlayed, 0)

	// OPML shows Unplayed (hasn't caught up yet).
	overcastLib := &model.Library{
		Episodes: []model.EpisodeState{
			{GUID: "9999", PlayState: model.PlayStateUnplayed},
		},
	}

	index := map[string]overcastIndexEntry{}
	_, _ = augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		overcastLib,
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, false, 0, false, cache,
	)

	normFeed := normalizeFeedURL(appleEp.FeedURL)
	key := "feeddate:" + normFeed + "|" + appleEp.PubDate.UTC().Format(time.RFC3339)
	entry, ok := index[key]
	if !ok {
		t.Fatalf("index missing key %q", key)
	}
	if entry.currentState != model.PlayStatePlayed {
		t.Errorf("currentState: got %v, want PlayStatePlayed — cache Played should beat OPML Unplayed", entry.currentState)
	}
}

// indexEntryKeys returns all keys in an index map for diagnostic messages.
func indexEntryKeys(m map[string]overcastIndexEntry) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
