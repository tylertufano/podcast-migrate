package apple

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// TestKVSReaderLive exercises KVSReader.Read() against the live Apple KVS.
// Run with: go test -v -run TestKVSReaderLive ./internal/apple/
// Requires APPLE_KVS_DSID and APPLE_KVS_COOKIES to be set.
func TestKVSReaderLive(t *testing.T) {
	if os.Getenv("APPLE_KVS_DSID") == "" {
		t.Skip("APPLE_KVS_DSID not set — skipping live KVS reader test")
	}

	r, err := NewKVSReader()
	if err != nil {
		t.Fatalf("NewKVSReader: %v", err)
	}

	ctx := context.Background()
	lib, err := r.Read(ctx)
	if err != nil {
		t.Fatalf("KVSReader.Read: %v", err)
	}

	t.Logf("subscriptions: %d", len(lib.Podcasts))
	t.Logf("episodes with play state: %d", len(lib.Episodes))

	if len(lib.Podcasts) == 0 {
		t.Error("expected at least one subscription, got none")
	}
	if len(lib.Episodes) == 0 {
		t.Error("expected at least one episode with play state, got none")
	}

	// Spot-check podcast fields.
	for _, pod := range lib.Podcasts {
		if pod.FeedURL == "" {
			t.Errorf("podcast %q has empty FeedURL", pod.Title)
		}
	}

	// Spot-check episode fields.
	var played, inProgress int
	var missingGUID, missingFeedURL int
	for _, ep := range lib.Episodes {
		switch ep.PlayState {
		case model.PlayStatePlayed:
			played++
		case model.PlayStateInProgress:
			inProgress++
		}
		if ep.GUID == "" {
			missingGUID++
		}
		if ep.FeedURL == "" {
			missingFeedURL++
		}
	}
	t.Logf("played: %d  in-progress: %d  missing-guid: %d  missing-feedURL: %d",
		played, inProgress, missingGUID, missingFeedURL)

	if missingFeedURL > 0 {
		t.Errorf("%d episodes have empty FeedURL", missingFeedURL)
	}

	// Verify all episode FeedURLs resolve to a known podcast (no dangling episodes).
	podFeedURLs := make(map[string]bool, len(lib.Podcasts))
	for _, pod := range lib.Podcasts {
		podFeedURLs[pod.FeedURL] = true
	}
	var dangling int
	for _, ep := range lib.Episodes {
		if !podFeedURLs[ep.FeedURL] {
			dangling++
			t.Logf("dangling episode: guid=%q feedURL=%q", ep.GUID, ep.FeedURL)
		}
	}
	if dangling > 0 {
		t.Errorf("%d episodes have FeedURLs not present in lib.Podcasts", dangling)
	}
}

// TestKVSReaderVsSQLiteLive compares KVSReader output against SQLiteReader on
// the same account, verifying that subscription sets are consistent.
// Only runs on macOS where SQLite is available.
func TestKVSReaderVsSQLiteLive(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("SQLite comparison requires macOS")
	}
	if os.Getenv("APPLE_KVS_DSID") == "" {
		t.Skip("APPLE_KVS_DSID not set — skipping live KVS reader test")
	}
	dbPath := DefaultSQLitePath()
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("SQLite database not accessible (%v) — skipping comparison", err)
	}

	ctx := context.Background()

	// SQLiteReader with live KVS overlay (existing path).
	sqliteReader := NewSQLiteReader(dbPath)
	kvsWriter, err := NewKVSWriter(dbPath)
	if err != nil {
		t.Fatalf("NewKVSWriter: %v", err)
	}
	if err := kvsWriter.initSession(ctx); err != nil {
		t.Fatalf("initSession: %v", err)
	}
	sqliteReader.SetLiveKVSValues(kvsWriter.serverRawValues)
	sqliteLib, err := sqliteReader.Read(ctx)
	if err != nil {
		t.Fatalf("SQLiteReader.Read: %v", err)
	}

	// KVSReader (new path).
	kvsReader, err := NewKVSReader()
	if err != nil {
		t.Fatalf("NewKVSReader: %v", err)
	}
	kvsLib, err := kvsReader.Read(ctx)
	if err != nil {
		t.Fatalf("KVSReader.Read: %v", err)
	}

	t.Logf("SQLiteReader: %d podcasts, %d episodes", len(sqliteLib.Podcasts), len(sqliteLib.Episodes))
	t.Logf("KVSReader:    %d podcasts, %d episodes", len(kvsLib.Podcasts), len(kvsLib.Episodes))

	// Both should find the same subscriptions.
	sqlitePods := make(map[string]string, len(sqliteLib.Podcasts)) // feedURL → title
	for _, pod := range sqliteLib.Podcasts {
		sqlitePods[pod.FeedURL] = pod.Title
	}
	kvsPods := make(map[string]string, len(kvsLib.Podcasts))
	for _, pod := range kvsLib.Podcasts {
		kvsPods[pod.FeedURL] = pod.Title
	}

	var inSQLiteNotKVS, inKVSNotSQLite int
	for url, title := range sqlitePods {
		if _, ok := kvsPods[url]; !ok {
			inSQLiteNotKVS++
			t.Logf("in SQLite not KVS: %s (%s)", url, title)
		}
	}
	for url, title := range kvsPods {
		if _, ok := sqlitePods[url]; !ok {
			inKVSNotSQLite++
			t.Logf("in KVS not SQLite: %s (%s)", url, title)
		}
	}
	t.Logf("subscription diff: %d only in SQLite, %d only in KVS", inSQLiteNotKVS, inKVSNotSQLite)

	// Episode play state overlap: episodes that both paths know about should
	// agree on play state (within one play-state level — KVS is authoritative,
	// but SQLite+KVS overlay may capture additional local heuristics).
	sqliteEps := make(map[string]model.PlayState) // guid → playState
	for _, ep := range sqliteLib.Episodes {
		if ep.GUID != "" {
			sqliteEps[ep.GUID] = ep.PlayState
		}
	}
	var agree, disagree int
	for _, ep := range kvsLib.Episodes {
		if ep.GUID == "" {
			continue
		}
		if sqliteState, ok := sqliteEps[ep.GUID]; ok {
			if sqliteState == ep.PlayState {
				agree++
			} else {
				disagree++
				t.Logf("disagree guid=%q: SQLite=%v KVS=%v title=%q",
					ep.GUID, sqliteState, ep.PlayState, ep.Title)
			}
		}
	}
	t.Logf("overlapping episodes: %d agree, %d disagree on play state", agree, disagree)

	// Allow a small number of disagreements (local heuristics may capture things
	// the pure KVS path cannot). More than 5% disagreement is a signal of a bug.
	if agree+disagree > 0 {
		pct := float64(disagree) / float64(agree+disagree) * 100
		t.Logf("disagreement rate: %.1f%%", pct)
		if pct > 5 {
			t.Errorf("play state disagreement rate %.1f%% exceeds 5%% threshold", pct)
		}
	}

	// --since filter: verify KVSReader correctly filters by timestamp.
	t.Run("since_filter", func(t *testing.T) {
		sinceReader, err := NewKVSReader()
		if err != nil {
			t.Fatalf("NewKVSReader: %v", err)
		}
		sinceReader.SetSinceTime(time.Now().Add(-48 * time.Hour))
		sinceLib, err := sinceReader.Read(ctx)
		if err != nil {
			t.Fatalf("KVSReader.Read with --since: %v", err)
		}
		t.Logf("--since 48h: %d episodes (vs %d total)", len(sinceLib.Episodes), len(kvsLib.Episodes))
		if len(sinceLib.Episodes) > len(kvsLib.Episodes) {
			t.Error("--since 48h returned more episodes than full read")
		}
	})
}
