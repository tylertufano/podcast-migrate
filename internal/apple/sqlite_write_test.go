package apple_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/apple"
	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// --- helpers for write tests ---

// readPlayState fetches ZPLAYSTATE and ZPLAYHEAD for the given GUID from the test DB.
func readPlayState(t *testing.T, db *sql.DB, guid string) (playState int, playHead float64, lastPlayed sql.NullFloat64) {
	t.Helper()
	err := db.QueryRow(
		`SELECT e.ZPLAYSTATE, e.ZPLAYHEAD, e.ZLASTDATEPLAYED
		 FROM ZMTEPISODE e WHERE e.ZGUID = ?`, guid,
	).Scan(&playState, &playHead, &lastPlayed)
	if err != nil {
		t.Fatalf("readPlayState(%q): %v", guid, err)
	}
	return
}

// openTestDB opens the test SQLite file in read-write mode for assertion queries.
func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// buildWriteLib builds a minimal model.Library from a slice of episodes.
// All episodes share the given feedURL; podcasts contains one entry for it.
func buildWriteLib(feedURL, podTitle string, eps []model.EpisodeState) *model.Library {
	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: feedURL, Title: podTitle}},
	}
	for i := range eps {
		eps[i].FeedURL = feedURL
	}
	lib.Episodes = eps
	return lib
}

// --- SQLiteWriter tests ---

func TestSQLiteWriter_MarkEpisodePlayed_ByPubDate(t *testing.T) {
	path := setupSQLiteDB(t)
	db := openTestDB(t, path)

	// ep4 is currently untouched (ZPLAYSTATE=0, ZPLAYHEAD=0).
	// Its pubDate is 697000000.0 (CoreData) → 697000000s after 2001-01-01.
	pubDate := coreDataTime(697000000.0)

	lib := buildWriteLib("https://feeds.example.com/show-a", "Show A", []model.EpisodeState{
		{
			Title:     "Untouched",
			PubDate:   pubDate,
			PlayState: model.PlayStatePlayed,
		},
	})

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 1 {
		t.Errorf("updated count: got %d, want 1", n)
	}

	ps, ph, lp := readPlayState(t, db, "rss-guid-4")
	if ps == 0 && ph == 0 {
		t.Error("episode should no longer be at default (unplayed) state")
	}
	_ = ph
	if !lp.Valid {
		t.Error("ZLASTDATEPLAYED should be set after marking played")
	}
}

func TestSQLiteWriter_MarkEpisodePlayed_ByTitle(t *testing.T) {
	path := setupSQLiteDB(t)
	db := openTestDB(t, path)

	// ep4 has guid "rss-guid-4". Match by title when pubDate not provided.
	lib := buildWriteLib("https://feeds.example.com/show-a", "Show A", []model.EpisodeState{
		{
			Title:     "Untouched",
			PlayState: model.PlayStatePlayed,
			// No PubDate — will fall back to title matching.
		},
	})

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 1 {
		t.Errorf("updated count: got %d, want 1", n)
	}

	ps, _, lp := readPlayState(t, db, "rss-guid-4")
	_ = ps
	if !lp.Valid {
		t.Error("ZLASTDATEPLAYED should be set after title-based match")
	}
}

func TestSQLiteWriter_FurthestWins_SkipsAlreadyPlayed(t *testing.T) {
	path := setupSQLiteDB(t)

	// ep1 is already fully played (ZPLAYSTATE=2, ZPLAYHEAD=0).
	// Writing "played" again should be a no-op (FurthestWins: already satisfied).
	pubDate := coreDataTime(700000000.0)
	lib := buildWriteLib("https://feeds.example.com/show-a", "Show A", []model.EpisodeState{
		{
			GUID:      "rss-guid-1",
			Title:     "Played Episode",
			PubDate:   pubDate,
			PlayState: model.PlayStatePlayed,
		},
	})

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 updates (already satisfied), got %d", n)
	}
}

func TestSQLiteWriter_FurthestWins_SkipsAheadInProgress(t *testing.T) {
	path := setupSQLiteDB(t)

	// ep2 is in-progress at 900s. Writing in-progress at 300s should be skipped.
	pubDate := coreDataTime(699000000.0)
	lib := buildWriteLib("https://feeds.example.com/show-a", "Show A", []model.EpisodeState{
		{
			Title:        "In-Progress Episode",
			PubDate:      pubDate,
			PlayState:    model.PlayStateInProgress,
			PlayPosition: 300 * time.Second, // behind current 900s
		},
	})

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 updates (Apple at 900s, incoming at 300s), got %d", n)
	}
}

func TestSQLiteWriter_FurthestWins_AdvancesInProgress(t *testing.T) {
	path := setupSQLiteDB(t)
	db := openTestDB(t, path)

	// ep2 is at 900s. Writing in-progress at 1500s should update.
	pubDate := coreDataTime(699000000.0)
	lib := buildWriteLib("https://feeds.example.com/show-a", "Show A", []model.EpisodeState{
		{
			Title:        "In-Progress Episode",
			PubDate:      pubDate,
			PlayState:    model.PlayStateInProgress,
			PlayPosition: 1500 * time.Second,
		},
	})

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 1 {
		t.Errorf("updated count: got %d, want 1", n)
	}

	_, ph, _ := readPlayState(t, db, "rss-guid-2")
	if ph < 1499 || ph > 1501 {
		t.Errorf("ZPLAYHEAD: got %v, want ~1500", ph)
	}
}

func TestSQLiteWriter_InProgress_WinsOverPlayed_WithSourceWins(t *testing.T) {
	path := setupSQLiteDB(t)
	db := openTestDB(t, path)

	// ep1 is fully played. With SourceWins, an in-progress overwrite should succeed.
	pubDate := coreDataTime(700000000.0)
	lib := buildWriteLib("https://feeds.example.com/show-a", "Show A", []model.EpisodeState{
		{
			Title:        "Played Episode",
			PubDate:      pubDate,
			PlayState:    model.PlayStateInProgress,
			PlayPosition: 600 * time.Second,
		},
	})

	opts := provider.WriteOptions{ConflictStrategy: provider.SourceWins}
	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 1 {
		t.Errorf("updated count: got %d, want 1 (SourceWins)", n)
	}

	_, ph, _ := readPlayState(t, db, "rss-guid-1")
	if ph < 599 || ph > 601 {
		t.Errorf("ZPLAYHEAD: got %v, want ~600", ph)
	}
}

func TestSQLiteWriter_TargetWins_NeverUpdates(t *testing.T) {
	path := setupSQLiteDB(t)

	// Any episode: TargetWins should always skip.
	pubDate := coreDataTime(697000000.0) // ep4 (untouched)
	lib := buildWriteLib("https://feeds.example.com/show-a", "Show A", []model.EpisodeState{
		{
			Title:     "Untouched",
			PubDate:   pubDate,
			PlayState: model.PlayStatePlayed,
		},
	})

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{
		ConflictStrategy: provider.TargetWins,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 updates (TargetWins), got %d", n)
	}
}

func TestSQLiteWriter_DryRun_NoChanges(t *testing.T) {
	path := setupSQLiteDB(t)
	db := openTestDB(t, path)

	pubDate := coreDataTime(697000000.0) // ep4 (untouched)
	lib := buildWriteLib("https://feeds.example.com/show-a", "Show A", []model.EpisodeState{
		{
			Title:     "Untouched",
			PubDate:   pubDate,
			PlayState: model.PlayStatePlayed,
		},
	})

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Write (dry-run): %v", err)
	}
	if n != 1 {
		t.Errorf("dry-run count: got %d, want 1", n)
	}

	// The actual DB should not have changed.
	ps, ph, lp := readPlayState(t, db, "rss-guid-4")
	if ps != 0 || ph != 0 || lp.Valid {
		t.Error("dry-run should not modify the database")
	}
}

func TestSQLiteWriter_EpisodeNotInDB_IsSkipped(t *testing.T) {
	path := setupSQLiteDB(t)

	lib := buildWriteLib("https://feeds.example.com/show-a", "Show A", []model.EpisodeState{
		{
			Title:     "Episode Not In Apple Podcasts DB",
			PubDate:   time.Now(),
			PlayState: model.PlayStatePlayed,
		},
	})

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 updates for unknown episode, got %d", n)
	}
}

func TestSQLiteWriter_URLNormalization_HttpToHttps(t *testing.T) {
	path := setupSQLiteDB(t)
	db := openTestDB(t, path)

	// show-b is stored as http://feeds.example.com/show-b in the DB.
	// The incoming library uses https:// — normalisation should bridge the gap.
	// We need an episode on show-b. ep6/7 are PSUB/PLUS and excluded from the
	// writer's query; let's insert an untouched episode on show-b.
	_, err := db.Exec(`INSERT INTO ZMTEPISODE VALUES (100, 2, 'rss-guid-100', 'Show B Ep', 698500000.0, 1800.0, 0, 0, 0.0, NULL, 'STDQ')`)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	pubDate := coreDataTime(698500000.0)
	lib := buildWriteLib("https://feeds.example.com/show-b", "Show B", []model.EpisodeState{
		{
			Title:     "Show B Ep",
			PubDate:   pubDate,
			PlayState: model.PlayStatePlayed,
		},
	})

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 1 {
		t.Errorf("http→https normalisation: expected 1 update, got %d", n)
	}
}

func TestSQLiteWriter_PodcastFilter_LimitsUpdates(t *testing.T) {
	path := setupSQLiteDB(t)

	// ep4 (untouched, "Show A") and a hypothetical Show B episode.
	// Filter to "show b" — ep4 on Show A should be untouched.
	pubDate := coreDataTime(697000000.0) // ep4 pubDate
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/show-a", Title: "Show A"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/show-a",
				Title:     "Untouched",
				PubDate:   pubDate,
				PlayState: model.PlayStatePlayed,
			},
		},
	}

	n, err := apple.NewSQLiteWriter(path).Write(context.Background(), lib, provider.WriteOptions{
		PodcastFilter: []string{"show b"}, // only "show b" — no episodes match
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 0 {
		t.Errorf("podcast filter should have excluded all episodes, got %d update(s)", n)
	}
}

// TestAppleProvider_SetLibrary_WritesPlayState ensures the Provider.SetLibrary
// path (called by the sync engine) reaches the SQLiteWriter correctly.
func TestAppleProvider_SetLibrary_WritesPlayState(t *testing.T) {
	path := setupSQLiteDB(t)
	db := openTestDB(t, path)

	pubDate := coreDataTime(697000000.0) // ep4 (untouched)
	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/show-a", Title: "Show A"}},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/show-a",
				Title:     "Untouched",
				PubDate:   pubDate,
				PlayState: model.PlayStatePlayed,
			},
		},
	}

	p := apple.NewProvider(path, "")
	if err := p.SetLibrary(context.Background(), lib, provider.WriteOptions{}); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}

	_, _, lp := readPlayState(t, db, "rss-guid-4")
	if !lp.Valid {
		t.Error("ZLASTDATEPLAYED should be set after SetLibrary write")
	}
}

// TestAppleProvider_SetLibrary_ReturnsUnsupported_WithEmptyPath ensures that
// SetLibrary returns an error when the SQLite database is not accessible.
func TestAppleProvider_SetLibrary_ReturnsError_WhenSQLiteMissing(t *testing.T) {
	p := apple.NewProvider("/nonexistent/MTLibrary.sqlite", "")
	err := p.SetLibrary(context.Background(), &model.Library{}, provider.WriteOptions{})
	if err == nil {
		t.Error("expected error from SetLibrary when SQLite database does not exist")
	}
}
