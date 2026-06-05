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
	n := augmentIndexFromPodcastPages(
		context.Background(), nil,
		&model.Library{},
		nil, // empty
		map[string]overcastIndexEntry{},
		0, map[string]string{},
		false, false, 0, false,
	)
	if n != 0 {
		t.Errorf("empty episodes: got %d, want 0", n)
	}
}

func TestAugment_AllUnplayed_ReturnsZero(t *testing.T) {
	eps := []model.EpisodeState{
		{FeedURL: "https://feeds.example.com/s", PubDate: time.Now(), PlayState: model.PlayStateUnplayed},
	}
	n := augmentIndexFromPodcastPages(
		context.Background(), nil,
		&model.Library{},
		eps,
		map[string]overcastIndexEntry{},
		0,
		map[string]string{"https://feeds.example.com/s": "test show"},
		false, false, 0, false,
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

	n := augmentIndexFromPodcastPages(
		context.Background(), nil,
		&model.Library{},
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{feedURL: "fresh air"},
		false, false, 0, false,
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

	n := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{appleEp},
		map[string]overcastIndexEntry{},
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		true, // strictFeedMatch
		false, 0, false,
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

	n := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{appleEp},
		map[string]overcastIndexEntry{},
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false,
		true, // subscribedOnly
		0, false,
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
	n := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, 0, false,
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
	n := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{appleEp},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, 0, false,
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

	augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{ep1, ep2},
		map[string]overcastIndexEntry{},
		0,
		map[string]string{feed1: "fresh air", feed2: "fresh air plus"},
		false, false, 0, false,
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
	n := augmentIndexFromPodcastPages(
		context.Background(), srv.Client(),
		&model.Library{},
		[]model.EpisodeState{epOffByTwoDays},
		index,
		0,
		map[string]string{appleEp.FeedURL: "fresh air"},
		false, false, 0, false,
	)
	if n != 1 {
		t.Errorf("title fallback: got %d entries added, want 1", n)
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
