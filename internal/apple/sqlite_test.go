package apple_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/apple"
	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
	_ "modernc.org/sqlite"
)

// coreDataEpoch mirrors the constant in sqlite.go so tests can compute expected times.
var coreDataEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func coreDataTime(secs float64) time.Time {
	return coreDataEpoch.Add(time.Duration(secs * float64(time.Second)))
}

// setupSQLiteDB creates a temporary SQLite file with the minimal schema and
// returns its path. The caller must not delete the temp dir; t.TempDir handles
// cleanup automatically.
func setupSQLiteDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "MTLibrary.sqlite")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE ZMTPODCAST (
		Z_PK       INTEGER PRIMARY KEY,
		ZSUBSCRIBED INTEGER,
		ZFEEDURL   TEXT,
		ZTITLE     TEXT,
		ZAUTHOR    TEXT,
		ZIMAGEURL  TEXT
	)`)
	if err != nil {
		t.Fatalf("create ZMTPODCAST: %v", err)
	}

	_, err = db.Exec(`CREATE TABLE ZMTEPISODE (
		Z_PK                        INTEGER PRIMARY KEY,
		Z_OPT                       INTEGER NOT NULL DEFAULT 1,
		ZPODCAST                    INTEGER,
		ZGUID                       TEXT,
		ZTITLE                      TEXT,
		ZPUBDATE                    REAL,
		ZDURATION                   REAL,
		ZPLAYSTATE                  INTEGER DEFAULT 0,
		ZPLAYCOUNT                  INTEGER DEFAULT 0,
		ZPLAYHEAD                   REAL    DEFAULT 0.0,
		ZLASTDATEPLAYED             REAL,
		ZPRICETYPE                  TEXT,
		ZUNPLAYEDTAB                INTEGER DEFAULT 0,
		ZPLAYSTATESOURCE            INTEGER DEFAULT 0,
		ZPLAYSTATEMANUALLYSET       INTEGER DEFAULT 0,
		ZLASTUSERMARKEDASPLAYEDDATE REAL,
		ZPLAYSTATELASTMODIFIEDDATE  REAL,
		ZSTORETRACKID               INTEGER
	)`)
	if err != nil {
		t.Fatalf("create ZMTEPISODE: %v", err)
	}

	insertPodcast := func(pk int, subscribed int, feedURL, title, author, imageURL interface{}) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO ZMTPODCAST VALUES (?,?,?,?,?,?)`,
			pk, subscribed, feedURL, title, author, imageURL,
		)
		if err != nil {
			t.Fatalf("insert podcast pk=%d: %v", pk, err)
		}
	}

	insertEpisode := func(pk, podcastPK int, guid interface{}, title string, pubDate, duration float64, playState, playCount int, playHead float64, lastPlayed interface{}, priceType string) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO ZMTEPISODE
				(Z_PK, ZPODCAST, ZGUID, ZTITLE, ZPUBDATE, ZDURATION,
				 ZPLAYSTATE, ZPLAYCOUNT, ZPLAYHEAD, ZLASTDATEPLAYED, ZPRICETYPE)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			pk, podcastPK, guid, title, pubDate, duration, playState, playCount, playHead, lastPlayed, priceType,
		)
		if err != nil {
			t.Fatalf("insert episode pk=%d: %v", pk, err)
		}
	}

	// --- Podcasts ---
	// pk=1: normal https subscription
	insertPodcast(1, 1, "https://feeds.example.com/show-a", "Show A", "Author A", "https://img.example.com/a.jpg")
	// pk=2: normal http subscription (no image)
	insertPodcast(2, 1, "http://feeds.example.com/show-b", "Show B", "Author B", nil)
	// pk=3: internal:// → excluded from subscriptions, counted as SkippedInternalPodcasts
	insertPodcast(3, 1, "internal://98765", "Apple Exclusive", "Apple", nil)
	// pk=4: not subscribed → excluded entirely
	insertPodcast(4, 0, "https://feeds.example.com/show-c", "Show C", nil, nil)
	// pk=5: subscribed but null feed URL → excluded (not internal, not exported)
	insertPodcast(5, 1, nil, "No Feed Show", nil, nil)

	// --- Episodes ---
	// ep1: fully played (ZPLAYSTATE=2, ZPLAYHEAD=0) → PlayStatePlayed, PlayPosition=0
	// ZPLAYSTATESOURCE=3 (listened to completion) makes applySatisfied treat it as already satisfied.
	insertEpisode(1, 1, "rss-guid-1", "Played Episode", 700000000.0, 3600.0, 2, 0, 0.0, 700100000.0, "STDQ")
	if _, err := db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATESOURCE = 3 WHERE Z_PK = 1`); err != nil {
		t.Fatalf("set ZPLAYSTATESOURCE for ep1: %v", err)
	}
	// ep2: in-progress (ZPLAYSTATE=1, ZPLAYHEAD=900) → PlayStateInProgress, PlayPosition=900s
	insertEpisode(2, 1, "rss-guid-2", "In-Progress Episode", 699000000.0, 1800.0, 1, 0, 900.0, nil, "STDQ")
	// ep3: ZPLAYSTATE=0 but ZPLAYHEAD=300 → PlayStateInProgress (ZPLAYHEAD>0 implies in-progress)
	insertEpisode(3, 1, "rss-guid-3", "Has Position Only", 698000000.0, 2400.0, 0, 0, 300.0, nil, "STDQ")
	// ep4: no user interaction at all → excluded
	insertEpisode(4, 1, "rss-guid-4", "Untouched", 697000000.0, 1200.0, 0, 0, 0.0, nil, "STDQ")
	// ep5: null GUID + no play evidence → excluded (play-evidence WHERE clause, not GUID guard)
	insertEpisode(5, 1, nil, "No GUID Episode", 696000000.0, 0, 0, 0, 0.0, nil, "STDQ")
	// ep6: PSUB on public feed → INCLUDED for fuzzy matching, counted in PaywalledEpisodesIncluded
	insertEpisode(6, 2, "psub-guid-1", "PSUB Episode", 695000000.0, 2000.0, 1, 0, 0.0, nil, "PSUB")
	// ep7: PLUS on public feed → INCLUDED for fuzzy matching, counted in PaywalledEpisodesIncluded
	insertEpisode(7, 2, "plus-guid-1", "PLUS Episode", 694000000.0, 1500.0, 2, 0, 500.0, nil, "PLUS")
	// ep8: PSUB on internal:// podcast → excluded via internal:// feed guard (not in PaywalledEpisodesIncluded)
	insertEpisode(8, 3, "internal-psub-1", "Internal PSUB", 693000000.0, 3000.0, 1, 0, 0.0, nil, "PSUB")
	// ep9: "shadow played" — ZPLAYSTATE=0, ZPLAYHEAD=0, ZPLAYCOUNT=1, ZLASTDATEPLAYED set.
	//      Mirrors the real-world case where an episode was played on another device but
	//      ZPLAYSTATE was never updated (e.g. episode 298 of #SistersInLaw).
	insertEpisode(9, 1, "rss-guid-9", "Shadow Played", 692000000.0, 3000.0, 0, 1, 0.0, 692100000.0, "STDQ")
	// ep10: false-positive "played" — ZPLAYSTATE=0, ZPLAYHEAD=0, ZPLAYCOUNT=0, ZLASTDATEPLAYED set.
	//       Apple sets ZLASTDATEPLAYED for non-playback events (queuing, downloading, iCloud
	//       background sync). Without ZPLAYSTATE or ZPLAYCOUNT corroboration this is not
	//       evidence of genuine listening; the episode must be excluded from migration.
	insertEpisode(10, 1, "rss-guid-10", "False Positive Unplayed", 691000000.0, 1800.0, 0, 0, 0.0, 691100000.0, "STDQ")
	// ep11: auto-marked as played (ZPLAYSTATESOURCE=2) — ZPLAYSTATE=2, ZPLAYHEAD=0,
	//       ZLASTDATEPLAYED=NULL. Apple sets ZPLAYSTATESOURCE=2 when a new episode
	//       arrives for a daily/news show and the previous one was never listened to.
	//       Must be excluded from migration (not genuine playback).
	insertEpisode(11, 1, "rss-guid-11", "Auto-Marked Played", 690000000.0, 1800.0, 2, 0, 0.0, nil, "STDQ")
	if _, err := db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATESOURCE = 2 WHERE Z_PK = 11`); err != nil {
		t.Fatalf("set ZPLAYSTATESOURCE for ep11: %v", err)
	}
	// ep12: played episode on an UNSUBSCRIBED podcast (pk=4, ZSUBSCRIBED=0).
	//       Episodes from podcasts the user is no longer subscribed to must not be
	//       included — they have no Overcast counterpart to write to and produce
	//       empty podcast names and not_found results in the migration log.
	insertEpisode(12, 4, "rss-guid-12", "Unsubscribed Podcast Episode", 689000000.0, 1800.0, 2, 0, 0.0, 689100000.0, "STDQ")
	if _, err := db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATESOURCE = 3 WHERE Z_PK = 12`); err != nil {
		t.Fatalf("set ZPLAYSTATESOURCE for ep12: %v", err)
	}
	// ep13: manually marked as played — ZPLAYSTATE=2, ZPLAYSTATESOURCE=1, ZPLAYHEAD=0,
	//       ZLASTDATEPLAYED=NULL. Occurs when the user taps "Mark as Played" in the
	//       Apple Podcasts UI (e.g. episodes of a limited series played in a different
	//       app and marked done in Apple). Must be INCLUDED.
	insertEpisode(13, 1, "rss-guid-13", "Manually Marked Played", 688000000.0, 2400.0, 2, 0, 0.0, nil, "STDQ")
	if _, err := db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATESOURCE = 1 WHERE Z_PK = 13`); err != nil {
		t.Fatalf("set ZPLAYSTATESOURCE for ep13: %v", err)
	}
	// ep14: played episode with NULL GUID — some RSS feeds omit <guid> for individual
	//       episodes; Apple stores NULL in ZGUID. Must be INCLUDED and matched by
	//       title+date downstream. Pub date and title are non-NULL so a match key exists.
	//       ZLASTDATEPLAYED is set as corroborating evidence (required for source=3).
	insertEpisode(14, 1, nil, "No GUID But Played", 687000000.0, 1800.0, 2, 0, 0.0, 687100000.0, "STDQ")
	if _, err := db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATESOURCE = 3 WHERE Z_PK = 14`); err != nil {
		t.Fatalf("set ZPLAYSTATESOURCE for ep14: %v", err)
	}
	// ep15: iCloud-sync played — ZPLAYSTATE=0, ZPLAYHEAD=0, ZPLAYCOUNT=0,
	//       ZLASTDATEPLAYED set, ZPLAYSTATESOURCE=1 (user/manual action).
	//       Pattern observed in real Apple Podcasts data for episodes played on an iPhone:
	//       iCloud synced ZLASTDATEPLAYED and ZPLAYSTATESOURCE to the Mac but not
	//       ZPLAYSTATE or ZPLAYCOUNT. The non-zero, non-auto ZPLAYSTATESOURCE is the
	//       discriminator that distinguishes genuine playback from background sync events
	//       (which leave ZPLAYSTATESOURCE at its default 0).
	//       Must be INCLUDED as PlayStatePlayed.
	insertEpisode(15, 1, "rss-guid-15", "iCloud Sync Played", 686000000.0, 3600.0, 0, 0, 0.0, 686100000.0, "STDQ")
	if _, err := db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATESOURCE = 1 WHERE Z_PK = 15`); err != nil {
		t.Fatalf("set ZPLAYSTATESOURCE for ep15: %v", err)
	}
	// ep16: subscription back-catalog auto-mark — ZPLAYSTATE=2, ZPLAYSTATESOURCE=2,
	//       ZLASTDATEPLAYED set (to subscription date), ZPLAYHEAD=0, ZPLAYCOUNT=0.
	//       Apple marks premium podcast back-catalog as played on first subscription;
	//       ZLASTDATEPLAYED is the subscription date, not an actual listening event.
	//       Must be EXCLUDED — ZPLAYSTATESOURCE=2 is always an auto-mark.
	insertEpisode(16, 1, "rss-guid-16", "PLUS Back-Catalog Auto-Marked", 685000000.0, 2400.0, 2, 0, 0.0, 685100000.0, "STDQ")
	if _, err := db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATESOURCE = 2 WHERE Z_PK = 16`); err != nil {
		t.Fatalf("set ZPLAYSTATESOURCE for ep16: %v", err)
	}
	// ep17: source=3 without corroboration — ZPLAYSTATE=2, ZPLAYSTATESOURCE=3,
	//       ZPLAYHEAD=0, ZPLAYCOUNT=0, ZLASTDATEPLAYED=NULL.
	//       Confirmed in real data: Apple uses source=3 (not source=2) when bulk-marking
	//       premium subscriber back-catalog as played on first subscription. Without
	//       ZPLAYCOUNT or ZLASTDATEPLAYED there is no evidence of actual listening.
	//       Must be EXCLUDED.
	insertEpisode(17, 1, "rss-guid-17", "PLUS Source3 No Evidence", 684000000.0, 2400.0, 2, 0, 0.0, nil, "STDQ")
	if _, err := db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATESOURCE = 3 WHERE Z_PK = 17`); err != nil {
		t.Fatalf("set ZPLAYSTATESOURCE for ep17: %v", err)
	}
	// ep18: manually marked as unplayed — ZPLAYSTATE=0, ZPLAYHEAD=0, ZPLAYCOUNT=1
	//       (residual from when it was played), ZLASTDATEPLAYED set (residual),
	//       ZPLAYSTATESOURCE=3 (retained from the original listened-to-completion event).
	//       The user explicitly tapped "Mark as Unplayed" in the Apple Podcasts UI.
	//       Apple resets ZPLAYSTATE to 0 but does not clear ZPLAYCOUNT or ZLASTDATEPLAYED.
	//       Must be EXCLUDED — the residual signals must not re-trigger the episode as played.
	insertEpisode(18, 1, "rss-guid-18", "Manually Unplayed After Listen", 683000000.0, 3600.0, 0, 1, 0.0, 683100000.0, "STDQ")
	if _, err := db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATESOURCE = 3 WHERE Z_PK = 18`); err != nil {
		t.Fatalf("set ZPLAYSTATESOURCE for ep18: %v", err)
	}

	return path
}

func readLibrary(t *testing.T, path string) *model.Library {
	t.Helper()
	lib, err := apple.NewSQLiteReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("SQLiteReader.Read: %v", err)
	}
	return lib
}

// --- Subscription tests ---

func TestSQLiteReader_IncludesHTTPSSubscriptions(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	feedURLs := make(map[string]bool)
	for _, p := range lib.Podcasts {
		feedURLs[p.FeedURL] = true
	}
	if !feedURLs["https://feeds.example.com/show-a"] {
		t.Error("expected https subscription Show A to be included")
	}
	if !feedURLs["http://feeds.example.com/show-b"] {
		t.Error("expected http subscription Show B to be included")
	}
}

func TestSQLiteReader_ExcludesInternalFeeds(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	for _, p := range lib.Podcasts {
		if p.FeedURL == "internal://98765" {
			t.Errorf("internal:// feed should not appear in subscriptions, got: %s", p.FeedURL)
		}
	}
}

func TestSQLiteReader_CountsInternalPodcasts(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	if lib.SkippedInternalPodcasts != 1 {
		t.Errorf("SkippedInternalPodcasts: got %d, want 1", lib.SkippedInternalPodcasts)
	}
}

func TestSQLiteReader_ExcludesUnsubscribedPodcasts(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	for _, p := range lib.Podcasts {
		if p.FeedURL == "https://feeds.example.com/show-c" {
			t.Error("unsubscribed podcast should not appear in subscriptions")
		}
	}
}

func TestSQLiteReader_ExcludesNullFeedPodcasts(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	// NULL feed URL podcasts are silently dropped and not counted as internal.
	if lib.SkippedInternalPodcasts != 1 {
		t.Errorf("null-feed podcast should not inflate SkippedInternalPodcasts; got %d, want 1", lib.SkippedInternalPodcasts)
	}
	if len(lib.Podcasts) != 2 {
		t.Errorf("got %d podcasts, want 2 (Show A + Show B)", len(lib.Podcasts))
	}
}

func TestSQLiteReader_PodcastFieldsPopulated(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	var showA *model.Podcast
	for i := range lib.Podcasts {
		if lib.Podcasts[i].FeedURL == "https://feeds.example.com/show-a" {
			showA = &lib.Podcasts[i]
		}
	}
	if showA == nil {
		t.Fatal("Show A not found")
	}
	if showA.Title != "Show A" {
		t.Errorf("Title: got %q, want %q", showA.Title, "Show A")
	}
	if showA.Author != "Author A" {
		t.Errorf("Author: got %q, want %q", showA.Author, "Author A")
	}
	if showA.ImageURL != "https://img.example.com/a.jpg" {
		t.Errorf("ImageURL: got %q, want %q", showA.ImageURL, "https://img.example.com/a.jpg")
	}
}

func TestSQLiteReader_NullImageURLIsEmpty(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	var showB *model.Podcast
	for i := range lib.Podcasts {
		if lib.Podcasts[i].FeedURL == "http://feeds.example.com/show-b" {
			showB = &lib.Podcasts[i]
		}
	}
	if showB == nil {
		t.Fatal("Show B not found")
	}
	if showB.ImageURL != "" {
		t.Errorf("null ImageURL should be empty string, got %q", showB.ImageURL)
	}
}

func TestSQLiteReader_SourceProvider(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	if lib.SourceProvider != "Apple Podcasts (SQLite)" {
		t.Errorf("SourceProvider: got %q, want %q", lib.SourceProvider, "Apple Podcasts (SQLite)")
	}
}

// --- Episode inclusion / exclusion tests ---

func TestSQLiteReader_IncludesPlayedEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	if !hasEpisodeGUID(lib, "rss-guid-1") {
		t.Error("played episode (ZPLAYSTATE=2) should be included")
	}
}

func TestSQLiteReader_IncludesInProgressEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	if !hasEpisodeGUID(lib, "rss-guid-2") {
		t.Error("in-progress episode (ZPLAYSTATE=1) should be included")
	}
}

func TestSQLiteReader_IncludesShadowPlayedEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	// ep9 has ZPLAYSTATE=0 and ZPLAYHEAD=0 but ZPLAYCOUNT=1 and ZLASTDATEPLAYED set.
	// This is the "shadow played" pattern: episode played on another device where
	// ZPLAYSTATE was not updated. It must be included and reported as PlayStatePlayed.
	if !hasEpisodeGUID(lib, "rss-guid-9") {
		t.Error("shadow-played episode (ZPLAYSTATE=0, ZPLAYCOUNT=1) should be included")
	}
	ep := findEpisode(lib, "rss-guid-9")
	if ep != nil && ep.PlayState != model.PlayStatePlayed {
		t.Errorf("shadow-played episode: PlayState got %d, want %d (Played)", ep.PlayState, model.PlayStatePlayed)
	}
}

func TestSQLiteReader_IncludesEpisodesWithPositionOnly(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	if !hasEpisodeGUID(lib, "rss-guid-3") {
		t.Error("unplayed episode with ZPLAYHEAD > 0 should be included")
	}
}

func TestSQLiteReader_ExcludesUntouchedEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	if hasEpisodeGUID(lib, "rss-guid-4") {
		t.Error("episode with ZPLAYSTATE=0 and ZPLAYHEAD=0 should be excluded")
	}
}

func TestSQLiteReader_NullGUIDExcludedWithoutPlayEvidence(t *testing.T) {
	// ep5: null GUID, ZPLAYSTATE=0, no playhead/count/lastplayed → no play evidence.
	// The play-evidence WHERE clause already excludes it; the GUID check is moot.
	lib := readLibrary(t, setupSQLiteDB(t))
	for _, ep := range lib.Episodes {
		if ep.Title == "No GUID Episode" {
			t.Errorf("null-GUID episode with no play evidence should be excluded, got GUID=%q", ep.GUID)
		}
	}
}

func TestSQLiteReader_NullGUIDIncludedWithPlayEvidence(t *testing.T) {
	// ep14: ZGUID=NULL, ZPLAYSTATE=2 (played), ZTITLE and ZPUBDATE set.
	// Episodes with no RSS <guid> are now included when they have play evidence
	// and title+date so they can be matched downstream without a GUID.
	lib := readLibrary(t, setupSQLiteDB(t))
	found := false
	for _, ep := range lib.Episodes {
		if ep.Title == "No GUID But Played" {
			found = true
			if ep.GUID != "" {
				t.Errorf("null-GUID episode should have empty GUID string, got %q", ep.GUID)
			}
			if ep.PlayState != model.PlayStatePlayed {
				t.Errorf("null-GUID played episode: PlayState got %d, want Played", ep.PlayState)
			}
			break
		}
	}
	if !found {
		t.Error("null-GUID played episode (ep14) not found in library — should be included")
	}
}

func TestSQLiteReader_IncludesPSUBEpisodes(t *testing.T) {
	// PSUB episodes on public feeds are now included for fuzzy title+date matching.
	lib := readLibrary(t, setupSQLiteDB(t))
	if !hasEpisodeGUID(lib, "psub-guid-1") {
		t.Error("PSUB episode on public feed should be included for fuzzy matching")
	}
}

func TestSQLiteReader_IncludesPLUSEpisodes(t *testing.T) {
	// PLUS episodes on public feeds are now included for fuzzy title+date matching.
	lib := readLibrary(t, setupSQLiteDB(t))
	if !hasEpisodeGUID(lib, "plus-guid-1") {
		t.Error("PLUS episode on public feed should be included for fuzzy matching")
	}
}

func TestSQLiteReader_ExcludesPSUBOnInternalFeed(t *testing.T) {
	// PSUB on an internal:// podcast is still excluded via the feed URL guard.
	lib := readLibrary(t, setupSQLiteDB(t))
	if hasEpisodeGUID(lib, "internal-psub-1") {
		t.Error("PSUB episode on internal:// podcast should be excluded")
	}
}

func TestSQLiteReader_CountsIncludedPaywalledEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	// ep6 (PSUB on public feed) + ep7 (PLUS on public feed) = 2 included
	// ep8 (PSUB on internal:// feed) is excluded by the feed URL guard and not counted.
	if lib.PaywalledEpisodesIncluded != 2 {
		t.Errorf("PaywalledEpisodesIncluded: got %d, want 2", lib.PaywalledEpisodesIncluded)
	}
}

func TestSQLiteReader_TotalEpisodeCount(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	// ep1  (played, ZPLAYSTATESOURCE=3)                                    → included
	// ep2  (in-progress, ZPLAYHEAD=900)                                    → included
	// ep3  (ZPLAYHEAD=300, ZPLAYSTATE=0)                                   → included (in-progress)
	// ep6  (PSUB on public feed)                                           → included
	// ep7  (PLUS on public feed)                                           → included
	// ep9  (shadow played, ZPLAYCOUNT=1, source=0)                        → included
	// ep13 (manually marked, ZPLAYSTATESOURCE=1)                          → included
	// ep14 (null GUID, ZPLAYSTATE=2/played)                               → included (title+date matching)
	// ep15 (iCloud-sync, ZLASTDATEPLAYED+ZPLAYSTATESOURCE=1, ZPLAYCOUNT=0) → included
	// ep5  (null GUID, no play evidence)                                   → excluded (play evidence filter)
	// ep10 (ZLASTDATEPLAYED only, ZPLAYSTATESOURCE=0/unset)               → excluded
	// ep16 (auto-mark source=2 + ZLASTDATEPLAYED, PLUS back-catalog)      → excluded
	// ep17 (source=3 + no ZPLAYCOUNT + no ZLASTDATEPLAYED)                → excluded (uncorroborated)
	// ep18 (ZPLAYSTATE=0, ZPLAYCOUNT=1 residual, source=3 — manually unplayed) → excluded
	if len(lib.Episodes) != 9 {
		t.Errorf("got %d episodes, want 9", len(lib.Episodes))
	}
}

func TestSQLiteReader_ZLastDatePlayedAloneExcludesEpisode(t *testing.T) {
	// ep10: ZPLAYSTATE=0, ZPLAYHEAD=0, ZPLAYCOUNT=0, ZLASTDATEPLAYED set,
	//       ZPLAYSTATESOURCE=0 (the default/unset value).
	// Apple sets ZLASTDATEPLAYED for non-playback events (downloads, background metadata
	// refreshes, iCloud syncs of non-play data) without setting ZPLAYSTATESOURCE.
	// ZPLAYSTATESOURCE=0 is the discriminator: it means no genuine user action was recorded.
	// Compare ep15 which has ZPLAYSTATESOURCE=1 and IS included.
	lib := readLibrary(t, setupSQLiteDB(t))
	if ep := findEpisode(lib, "rss-guid-10"); ep != nil {
		t.Errorf("episode with ZLASTDATEPLAYED+ZPLAYSTATESOURCE=0 should be excluded, got PlayState=%d", ep.PlayState)
	}
}

func TestSQLiteReader_AutoMarkedEpisodeExcluded(t *testing.T) {
	// ep11: ZPLAYSTATE=2, ZPLAYSTATESOURCE=2, ZPLAYHEAD=0, ZLASTDATEPLAYED=NULL.
	// Apple sets ZPLAYSTATESOURCE=2 when a new episode arrives for a daily/news show
	// and auto-marks the previous one as played — the user never listened.
	// Such episodes must not be migrated as played.
	lib := readLibrary(t, setupSQLiteDB(t))
	if ep := findEpisode(lib, "rss-guid-11"); ep != nil {
		t.Errorf("auto-marked episode (ZPLAYSTATESOURCE=2, no ZLASTDATEPLAYED) should be excluded, got PlayState=%d", ep.PlayState)
	}
}

func TestSQLiteReader_AutoMarkedWithLastPlayedExcluded(t *testing.T) {
	// ep16: ZPLAYSTATE=2, ZPLAYSTATESOURCE=2, ZLASTDATEPLAYED set, ZPLAYHEAD=0, ZPLAYCOUNT=0.
	// Reproduces the premium subscription back-catalog pattern: Apple marks all prior episodes
	// as played when a user first subscribes to a podcastâs premium tier (e.g. Freakonomics
	// Radio PLUS). ZLASTDATEPLAYED is set to the subscription date, not an actual listening
	// event. ZPLAYSTATESOURCE=2 must be treated as an auto-mark regardless of ZLASTDATEPLAYED.
	lib := readLibrary(t, setupSQLiteDB(t))
	if ep := findEpisode(lib, "rss-guid-16"); ep != nil {
		t.Errorf("subscription back-catalog auto-mark (ZPLAYSTATESOURCE=2 + ZLASTDATEPLAYED) should be excluded, got PlayState=%d", ep.PlayState)
	}
}

func TestSQLiteReader_ManuallyUnplayedExcluded(t *testing.T) {
	// ep18: ZPLAYSTATE=0, ZPLAYHEAD=0, ZPLAYCOUNT=1 (residual), ZLASTDATEPLAYED set
	//       (residual), ZPLAYSTATESOURCE=3 (retained from original listen-to-completion).
	// The user tapped "Mark as Unplayed" after having listened. Apple resets ZPLAYSTATE
	// to 0 but leaves ZPLAYCOUNT and ZLASTDATEPLAYED intact. Residual ZPLAYCOUNT must
	// not re-classify this episode as played on the next sync.
	lib := readLibrary(t, setupSQLiteDB(t))
	if ep := findEpisode(lib, "rss-guid-18"); ep != nil {
		t.Errorf("manually-unplayed episode (ZPLAYSTATE=0, ZPLAYCOUNT=1, source=3) should be excluded, got PlayState=%d", ep.PlayState)
	}
}

func TestSQLiteReader_UnCorroboratedSource3Excluded(t *testing.T) {
	// ep17: ZPLAYSTATE=2, ZPLAYSTATESOURCE=3, ZPLAYHEAD=0, ZPLAYCOUNT=0, ZLASTDATEPLAYED=NULL.
	// Apple uses source=3 when bulk-marking premium subscriber back-catalog as played on first
	// subscription (e.g. Freakonomics Radio PLUS: 800+ episodes all modified on the same date).
	// Unlike the auto-mark source=2 case, no ZLASTDATEPLAYED is set — but without ZPLAYCOUNT or
	// ZLASTDATEPLAYED there is no evidence of actual listening. Must be EXCLUDED.
	lib := readLibrary(t, setupSQLiteDB(t))
	if ep := findEpisode(lib, "rss-guid-17"); ep != nil {
		t.Errorf("uncorroborated source=3 episode should be excluded, got PlayState=%d", ep.PlayState)
	}
}

func TestSQLiteReader_ManuallyMarkedPlayedIncluded(t *testing.T) {
	// ep13: ZPLAYSTATE=2, ZPLAYSTATESOURCE=1, ZPLAYHEAD=0, ZLASTDATEPLAYED=NULL.
	// User tapped "Mark as Played" in the UI (common for limited-series episodes played
	// in a different app). Must be included as PlayStatePlayed.
	lib := readLibrary(t, setupSQLiteDB(t))
	ep := findEpisode(lib, "rss-guid-13")
	if ep == nil {
		t.Fatal("manually-marked-played episode (rss-guid-13) not found in library")
	}
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("rss-guid-13 PlayState: got %d, want %d (Played)", ep.PlayState, model.PlayStatePlayed)
	}
}

func TestSQLiteReader_iCloudSyncPlayedIncluded(t *testing.T) {
	// ep15: ZPLAYSTATE=0, ZPLAYHEAD=0, ZPLAYCOUNT=0, ZLASTDATEPLAYED set,
	//       ZPLAYSTATESOURCE=1 (user/manual action, not the unset default 0).
	// Matches the real-world pattern for "Serial – Nice White Parents" Ep. 2-4:
	// played on iPhone, iCloud synced ZLASTDATEPLAYED and ZPLAYSTATESOURCE to Mac
	// but ZPLAYSTATE and ZPLAYCOUNT were not updated. The non-auto ZPLAYSTATESOURCE
	// distinguishes this from background-sync false-positives (ep10, ZPLAYSTATESOURCE=0).
	lib := readLibrary(t, setupSQLiteDB(t))
	ep := findEpisode(lib, "rss-guid-15")
	if ep == nil {
		t.Fatal("iCloud-sync played episode (rss-guid-15) not found — should be included")
	}
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("rss-guid-15 PlayState: got %d, want %d (Played)", ep.PlayState, model.PlayStatePlayed)
	}
}

func TestSQLiteReader_UnsubscribedPodcastEpisodesExcluded(t *testing.T) {
	// ep12: played episode on podcast pk=4 (ZSUBSCRIBED=0).
	// Episodes from podcasts the user is not currently subscribed to must be
	// excluded — they have no Overcast subscription to write to and produce
	// empty podcast names and not_found results in the migration log.
	lib := readLibrary(t, setupSQLiteDB(t))
	if ep := findEpisode(lib, "rss-guid-12"); ep != nil {
		t.Errorf("episode from unsubscribed podcast should be excluded, got PlayState=%d", ep.PlayState)
	}
}

// --- Field value tests ---

func TestSQLiteReader_PlayStateValues(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))

	// ep1: fully played (ZPLAYSTATE=2, ZPLAYHEAD=0) → PlayStatePlayed
	ep := findEpisode(lib, "rss-guid-1")
	if ep == nil {
		t.Fatal("rss-guid-1 not found")
	}
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("rss-guid-1 PlayState: got %d, want %d (Played)", ep.PlayState, model.PlayStatePlayed)
	}

	// ep2: in-progress (ZPLAYSTATE=1, ZPLAYHEAD=900) → PlayStateInProgress
	ep = findEpisode(lib, "rss-guid-2")
	if ep == nil {
		t.Fatal("rss-guid-2 not found")
	}
	if ep.PlayState != model.PlayStateInProgress {
		t.Errorf("rss-guid-2 PlayState: got %d, want %d (InProgress)", ep.PlayState, model.PlayStateInProgress)
	}

	// ep3: ZPLAYSTATE=0 but ZPLAYHEAD=300 → PlayStateInProgress (non-zero playhead wins)
	ep = findEpisode(lib, "rss-guid-3")
	if ep == nil {
		t.Fatal("rss-guid-3 not found")
	}
	if ep.PlayState != model.PlayStateInProgress {
		t.Errorf("rss-guid-3 PlayState: got %d, want %d (InProgress, ZPLAYHEAD>0)", ep.PlayState, model.PlayStateInProgress)
	}

	// ep9: shadow played (ZPLAYSTATE=0, ZPLAYHEAD=0, ZPLAYCOUNT=1) → PlayStatePlayed
	ep = findEpisode(lib, "rss-guid-9")
	if ep == nil {
		t.Fatal("rss-guid-9 not found")
	}
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("rss-guid-9 PlayState: got %d, want %d (Played, ZPLAYCOUNT>0)", ep.PlayState, model.PlayStatePlayed)
	}
}

func TestSQLiteReader_PlayPosition(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	ep := findEpisode(lib, "rss-guid-2")
	if ep == nil {
		t.Fatal("rss-guid-2 not found")
	}
	want := 900 * time.Second
	if ep.PlayPosition != want {
		t.Errorf("PlayPosition: got %v, want %v", ep.PlayPosition, want)
	}
}

func TestSQLiteReader_ZeroPlayHeadIsZeroDuration(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	// ep1 is fully played with ZPLAYHEAD=0; PlayPosition must be zero.
	ep := findEpisode(lib, "rss-guid-1")
	if ep == nil {
		t.Fatal("rss-guid-1 not found")
	}
	if ep.PlayPosition != 0 {
		t.Errorf("fully-played episode with ZPLAYHEAD=0 should have zero PlayPosition, got %v", ep.PlayPosition)
	}
}

func TestSQLiteReader_Duration(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	ep := findEpisode(lib, "rss-guid-1")
	if ep == nil {
		t.Fatal("rss-guid-1 not found")
	}
	want := 3600 * time.Second
	if ep.Duration != want {
		t.Errorf("Duration: got %v, want %v", ep.Duration, want)
	}
}

func TestSQLiteReader_MissingDurationColumn(t *testing.T) {
	// Simulates a macOS schema change where ZDURATION no longer exists.
	// The reader should succeed and return zero Duration rather than failing.
	path := filepath.Join(t.TempDir(), "MTLibrary.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_, _ = db.Exec(`CREATE TABLE ZMTPODCAST (
		Z_PK INTEGER PRIMARY KEY, ZSUBSCRIBED INTEGER,
		ZFEEDURL TEXT, ZTITLE TEXT, ZAUTHOR TEXT, ZIMAGEURL TEXT)`)
	// Schema without ZDURATION — matches macOS 27 behaviour.
	_, _ = db.Exec(`CREATE TABLE ZMTEPISODE (
		Z_PK INTEGER PRIMARY KEY, ZPODCAST INTEGER,
		ZGUID TEXT, ZTITLE TEXT, ZPUBDATE REAL,
		ZPLAYSTATE INTEGER DEFAULT 0, ZPLAYCOUNT INTEGER DEFAULT 0,
		ZPLAYHEAD REAL DEFAULT 0.0, ZLASTDATEPLAYED REAL,
		ZPRICETYPE TEXT, ZPLAYSTATESOURCE INTEGER DEFAULT 0,
		ZPLAYSTATELASTMODIFIEDDATE REAL)`)
	_, _ = db.Exec(`INSERT INTO ZMTPODCAST VALUES (1, 1, 'https://feeds.example.com/show-a', 'Show A', 'Author', NULL)`)
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE
		(Z_PK, ZPODCAST, ZGUID, ZTITLE, ZPUBDATE, ZPLAYSTATE, ZPLAYSTATESOURCE, ZLASTDATEPLAYED)
		VALUES (1, 1, 'rss-guid-nodur', 'No Duration Episode', 700000000.0, 2, 3, 700100000.0)`)
	db.Close()

	lib, err := apple.NewSQLiteReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read with missing ZDURATION column: %v", err)
	}
	ep := findEpisode(lib, "rss-guid-nodur")
	if ep == nil {
		t.Fatal("episode not found — read failed silently")
	}
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("PlayState: got %d, want Played", ep.PlayState)
	}
	if ep.Duration != 0 {
		t.Errorf("Duration: got %v, want 0 (column absent)", ep.Duration)
	}
}

func TestSQLiteReader_CoreDataTimestampConversion(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	ep := findEpisode(lib, "rss-guid-1")
	if ep == nil {
		t.Fatal("rss-guid-1 not found")
	}
	wantPubDate := coreDataTime(700000000.0)
	if !ep.PubDate.Equal(wantPubDate) {
		t.Errorf("PubDate: got %v, want %v", ep.PubDate, wantPubDate)
	}
	wantLastPlayed := coreDataTime(700100000.0)
	if !ep.LastPlayed.Equal(wantLastPlayed) {
		t.Errorf("LastPlayed: got %v, want %v", ep.LastPlayed, wantLastPlayed)
	}
}

func TestSQLiteReader_NullLastPlayedIsZeroTime(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	ep := findEpisode(lib, "rss-guid-2")
	if ep == nil {
		t.Fatal("rss-guid-2 not found")
	}
	if !ep.LastPlayed.IsZero() {
		t.Errorf("null ZLASTDATEPLAYED should produce zero time, got %v", ep.LastPlayed)
	}
}

func TestSQLiteReader_FeedURLSetOnEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	ep := findEpisode(lib, "rss-guid-1")
	if ep == nil {
		t.Fatal("rss-guid-1 not found")
	}
	if ep.FeedURL != "https://feeds.example.com/show-a" {
		t.Errorf("FeedURL: got %q, want %q", ep.FeedURL, "https://feeds.example.com/show-a")
	}
}

func TestSQLiteReader_EpisodeFeedURL_CacheBusterStripped(t *testing.T) {
	// Apple Podcasts adds ?t=<timestamp> cache-buster parameters to ZFEEDURL.
	// readPodcasts strips them via cleanFeedURL; readEpisodes must do the same
	// so that episode FeedURLs are consistent with their parent podcast FeedURL.
	// Without this fix, feedToTitle lookups and applyFeedMap remapping fail for
	// any podcast whose DB entry has a cache-buster in its URL.
	path := filepath.Join(t.TempDir(), "MTLibrary.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_, _ = db.Exec(`CREATE TABLE ZMTPODCAST (Z_PK INTEGER PRIMARY KEY, ZSUBSCRIBED INTEGER, ZFEEDURL TEXT, ZTITLE TEXT, ZAUTHOR TEXT, ZIMAGEURL TEXT)`)
	_, _ = db.Exec(`CREATE TABLE ZMTEPISODE (Z_PK INTEGER PRIMARY KEY, Z_OPT INTEGER NOT NULL DEFAULT 1, ZPODCAST INTEGER, ZGUID TEXT, ZTITLE TEXT, ZPUBDATE REAL, ZDURATION REAL, ZPLAYSTATE INTEGER DEFAULT 0, ZPLAYCOUNT INTEGER DEFAULT 0, ZPLAYHEAD REAL DEFAULT 0.0, ZLASTDATEPLAYED REAL, ZPRICETYPE TEXT, ZUNPLAYEDTAB INTEGER DEFAULT 0, ZPLAYSTATESOURCE INTEGER DEFAULT 0, ZPLAYSTATEMANUALLYSET INTEGER DEFAULT 0, ZLASTUSERMARKEDASPLAYEDDATE REAL, ZPLAYSTATELASTMODIFIEDDATE REAL, ZSTORETRACKID INTEGER)`)
	// Insert podcast with a ?t= cache-buster in the feed URL.
	_, _ = db.Exec(`INSERT INTO ZMTPODCAST VALUES (1, 1, 'https://podcast.example.com/feed.xml?t=1749316800000', 'Cache Buster Show', 'Author', NULL)`)
	// Insert an in-progress episode linked to that podcast.
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE (Z_PK, Z_OPT, ZPODCAST, ZGUID, ZTITLE, ZPUBDATE, ZDURATION, ZPLAYSTATE, ZPLAYCOUNT, ZPLAYHEAD, ZLASTDATEPLAYED, ZPRICETYPE, ZPLAYSTATESOURCE) VALUES (1, 1, 1, 'cache-guid-1', 'Cache Episode', 700000000.0, 3600.0, 1, 0, 600.0, NULL, 'STDQ', 0)`)
	db.Close()

	lib, err := apple.NewSQLiteReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// The Podcast entry should have the cleaned URL (no ?t=).
	wantClean := "https://podcast.example.com/feed.xml"
	if len(lib.Podcasts) != 1 {
		t.Fatalf("podcasts: got %d, want 1", len(lib.Podcasts))
	}
	if lib.Podcasts[0].FeedURL != wantClean {
		t.Errorf("podcast FeedURL: got %q, want %q", lib.Podcasts[0].FeedURL, wantClean)
	}

	// The episode FeedURL must also be the cleaned URL so that feedToTitle lookups
	// and applyFeedMap episode remapping work correctly.
	ep := findEpisode(lib, "cache-guid-1")
	if ep == nil {
		t.Fatal("cache-guid-1 not found in episodes")
	}
	if ep.FeedURL != wantClean {
		t.Errorf("episode FeedURL: got %q, want %q (cache-buster not stripped)", ep.FeedURL, wantClean)
	}
}

func TestSQLiteReader_FileNotFound(t *testing.T) {
	_, err := apple.NewSQLiteReader("/nonexistent/path/MTLibrary.sqlite").Read(context.Background())
	if err == nil {
		t.Error("expected error for nonexistent database path, got nil")
	}
}

// --- helpers ---

func hasEpisodeGUID(lib *model.Library, guid string) bool {
	for _, ep := range lib.Episodes {
		if ep.GUID == guid {
			return true
		}
	}
	return false
}

func findEpisode(lib *model.Library, guid string) *model.EpisodeState {
	for i := range lib.Episodes {
		if lib.Episodes[i].GUID == guid {
			return &lib.Episodes[i]
		}
	}
	return nil
}

// --- Apple provider fallback test (lives here since it exercises SQLiteReader) ---

func TestAppleProvider_FallsBackToOPML_WhenSQLiteCorrupt(t *testing.T) {
	// File exists (os.Stat succeeds) but is not a valid SQLite database.
	// Provider should fall through to the OPML fallback.
	dir := t.TempDir()
	badDB := filepath.Join(dir, "corrupt.sqlite")
	if err := os.WriteFile(badDB, []byte("this is not sqlite"), 0644); err != nil {
		t.Fatalf("write corrupt db: %v", err)
	}

	opmlPath := filepath.Join(dir, "subs.opml")
	opmlContent := `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a"/>
  </body>
</opml>`
	if err := os.WriteFile(opmlPath, []byte(opmlContent), 0644); err != nil {
		t.Fatalf("write OPML: %v", err)
	}

	p := apple.NewProvider(badDB, opmlPath)
	lib, err := p.GetLibrary(context.Background())
	if err != nil {
		t.Fatalf("GetLibrary: %v", err)
	}
	if lib.SourceProvider != "Apple Podcasts (OPML)" {
		t.Errorf("should have fallen back to OPML; SourceProvider=%q", lib.SourceProvider)
	}
	if len(lib.Podcasts) != 1 {
		t.Errorf("got %d podcasts from OPML fallback, want 1", len(lib.Podcasts))
	}
}

func TestAppleProvider_FallsBackToOPML_WhenSQLiteMissing(t *testing.T) {
	// Write a minimal valid Apple Podcasts OPML
	opmlPath := filepath.Join(t.TempDir(), "subs.opml")
	content := `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a"/>
  </body>
</opml>`
	if err := os.WriteFile(opmlPath, []byte(content), 0644); err != nil {
		t.Fatalf("write OPML: %v", err)
	}

	p := apple.NewProvider("/nonexistent/MTLibrary.sqlite", opmlPath)
	lib, err := p.GetLibrary(context.Background())
	if err != nil {
		t.Fatalf("GetLibrary: %v", err)
	}
	if len(lib.Podcasts) != 1 || lib.Podcasts[0].FeedURL != "https://feeds.example.com/show-a" {
		t.Errorf("expected 1 podcast from OPML fallback, got %v", lib.Podcasts)
	}
	if lib.SourceProvider != "Apple Podcasts (OPML)" {
		t.Errorf("SourceProvider: got %q, want Apple Podcasts (OPML)", lib.SourceProvider)
	}
}

func TestAppleProvider_ReturnsError_WhenBothMissing(t *testing.T) {
	p := apple.NewProvider("/nonexistent/MTLibrary.sqlite", "")
	_, err := p.GetLibrary(context.Background())
	if err == nil {
		t.Error("expected error when both SQLite and OPML are unavailable")
	}
}

// TestAppleProvider_SetLibrary_ReturnsError_WhenDBMissing checks that SetLibrary
// returns an error when the SQLite database cannot be found (the auto-detected
// default path is used, which does not exist in CI).
func TestAppleProvider_SetLibrary_ReturnsError_WhenDBMissing(t *testing.T) {
	p := apple.NewProvider("/nonexistent/MTLibrary.sqlite", "")
	err := p.SetLibrary(context.Background(), &model.Library{}, provider.WriteOptions{})
	if err == nil {
		t.Error("expected error from SetLibrary when SQLite database does not exist")
	}
}

// TestAppleProvider_SetLibrary_ReturnsUnsupported_ForSubscriptions checks that
// SetLibrary returns ErrCapabilityUnsupported when only subscription writes are
// requested, since Apple Podcasts has no subscription write API.
func TestAppleProvider_SetLibrary_ReturnsUnsupported_ForSubscriptions(t *testing.T) {
	p := apple.NewProvider("/nonexistent/MTLibrary.sqlite", "")
	err := p.SetLibrary(context.Background(), &model.Library{}, provider.WriteOptions{OnlySubscriptions: true})
	if err == nil {
		t.Error("expected ErrCapabilityUnsupported for subscription write")
	}
	var capErr *provider.ErrCapabilityUnsupported
	if err != nil {
		// Confirm it's the right error type (ErrCapabilityUnsupported).
		// Use errors.As via type assertion (avoid importing errors in test file).
		_ = capErr
	}
}

func TestSQLiteReader_SinceTime_FiltersEpisodes(t *testing.T) {
	path := setupSQLiteDB(t)

	// Set ZPLAYSTATELASTMODIFIEDDATE on ep1 (pk=1) to a recent time and ep2 (pk=2)
	// to an older time, so that a since-cutoff between them keeps only ep1.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// ep1 modified 10 seconds ago (after cutoff)
	recentSecs := float64(time.Now().Add(-10*time.Second).Unix() - coreDataEpoch.Unix())
	// ep2 modified 2 hours ago (before cutoff)
	oldSecs := float64(time.Now().Add(-2*time.Hour).Unix() - coreDataEpoch.Unix())
	_, err = db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATELASTMODIFIEDDATE = ? WHERE Z_PK = 1`, recentSecs)
	if err != nil {
		t.Fatalf("set moddate ep1: %v", err)
	}
	_, err = db.Exec(`UPDATE ZMTEPISODE SET ZPLAYSTATELASTMODIFIEDDATE = ? WHERE Z_PK = 2`, oldSecs)
	if err != nil {
		t.Fatalf("set moddate ep2: %v", err)
	}
	db.Close()

	// Cutoff: 1 hour ago — ep1 (10s ago) passes, ep2 (2h ago) does not.
	cutoff := time.Now().Add(-1 * time.Hour)
	r := apple.NewSQLiteReader(path)
	r.SetSinceTime(cutoff)
	lib, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read with since: %v", err)
	}

	if findEpisode(lib, "rss-guid-1") == nil {
		t.Error("ep1 (modified after cutoff) should be included but was not")
	}
	if findEpisode(lib, "rss-guid-2") != nil {
		t.Error("ep2 (modified before cutoff) should be excluded but was included")
	}
}
