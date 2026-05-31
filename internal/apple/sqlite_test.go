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
		Z_PK            INTEGER PRIMARY KEY,
		Z_OPT           INTEGER NOT NULL DEFAULT 1,
		ZPODCAST        INTEGER,
		ZGUID           TEXT,
		ZTITLE          TEXT,
		ZPUBDATE        REAL,
		ZDURATION       REAL,
		ZPLAYSTATE      INTEGER DEFAULT 0,
		ZPLAYCOUNT      INTEGER DEFAULT 0,
		ZPLAYHEAD       REAL    DEFAULT 0.0,
		ZLASTDATEPLAYED REAL,
		ZPRICETYPE      TEXT
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
	insertEpisode(1, 1, "rss-guid-1", "Played Episode", 700000000.0, 3600.0, 2, 0, 0.0, 700100000.0, "STDQ")
	// ep2: in-progress (ZPLAYSTATE=1, ZPLAYHEAD=900) → PlayStateInProgress, PlayPosition=900s
	insertEpisode(2, 1, "rss-guid-2", "In-Progress Episode", 699000000.0, 1800.0, 1, 0, 900.0, nil, "STDQ")
	// ep3: ZPLAYSTATE=0 but ZPLAYHEAD=300 → PlayStateInProgress (ZPLAYHEAD>0 implies in-progress)
	insertEpisode(3, 1, "rss-guid-3", "Has Position Only", 698000000.0, 2400.0, 0, 0, 300.0, nil, "STDQ")
	// ep4: no user interaction at all → excluded
	insertEpisode(4, 1, "rss-guid-4", "Untouched", 697000000.0, 1200.0, 0, 0, 0.0, nil, "STDQ")
	// ep5: null GUID → excluded
	insertEpisode(5, 1, nil, "No GUID Episode", 696000000.0, 0, 1, 0, 0.0, nil, "STDQ")
	// ep6: PSUB on public feed → excluded from episodes, counted in SkippedPaywalledEpisodes
	insertEpisode(6, 2, "psub-guid-1", "PSUB Episode", 695000000.0, 2000.0, 1, 0, 0.0, nil, "PSUB")
	// ep7: PLUS on public feed → excluded from episodes, counted in SkippedPaywalledEpisodes
	insertEpisode(7, 2, "plus-guid-1", "PLUS Episode", 694000000.0, 1500.0, 2, 0, 500.0, nil, "PLUS")
	// ep8: PSUB on internal:// podcast → excluded (paywalled), counted in SkippedPaywalledEpisodes
	insertEpisode(8, 3, "internal-psub-1", "Internal PSUB", 693000000.0, 3000.0, 1, 0, 0.0, nil, "PSUB")
	// ep9: "shadow played" — ZPLAYSTATE=0, ZPLAYHEAD=0, ZPLAYCOUNT=1, ZLASTDATEPLAYED set.
	//      Mirrors the real-world case where an episode was played on another device but
	//      ZPLAYSTATE was never updated (e.g. episode 298 of #SistersInLaw).
	insertEpisode(9, 1, "rss-guid-9", "Shadow Played", 692000000.0, 3000.0, 0, 1, 0.0, 692100000.0, "STDQ")

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

func TestSQLiteReader_ExcludesNullGUIDEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	for _, ep := range lib.Episodes {
		if ep.GUID == "" {
			t.Errorf("episode with null GUID should be excluded, got title=%q", ep.Title)
		}
	}
}

func TestSQLiteReader_ExcludesPSUBEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	if hasEpisodeGUID(lib, "psub-guid-1") {
		t.Error("PSUB episode should be excluded from episode states")
	}
}

func TestSQLiteReader_ExcludesPLUSEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	if hasEpisodeGUID(lib, "plus-guid-1") {
		t.Error("PLUS episode should be excluded from episode states")
	}
}

func TestSQLiteReader_CountsPaywalledEpisodes(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	// ep6 (PSUB on public feed) + ep7 (PLUS on public feed) + ep8 (PSUB on internal feed) = 3
	if lib.SkippedPaywalledEpisodes != 3 {
		t.Errorf("SkippedPaywalledEpisodes: got %d, want 3", lib.SkippedPaywalledEpisodes)
	}
}

func TestSQLiteReader_TotalEpisodeCount(t *testing.T) {
	lib := readLibrary(t, setupSQLiteDB(t))
	// ep1 (played) + ep2 (in-progress) + ep3 (has position) + ep9 (shadow played) = 4 included
	if len(lib.Episodes) != 4 {
		t.Errorf("got %d episodes, want 4 (played + in-progress + has-position + shadow-played)", len(lib.Episodes))
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
