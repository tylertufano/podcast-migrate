//go:build darwin

package apple

// Internal tests for SQLiteReader's live KVS read path. Uses package apple
// (not apple_test) to access unexported buildItemValue for constructing test
// raw KVS values that match the DEFLATE-compressed binary plist format the
// real server returns.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tyler/podcast-migrate/internal/model"
	_ "modernc.org/sqlite"
)

// buildLiveKVSValue builds a raw value (DEFLATE-compressed binary plist) for
// use with SetLiveKVSValues, using the same encoding as the real KVS server.
func buildLiveKVSValue(t *testing.T, hasBeenPlayed bool, bookmarkTimeSec float64) []byte {
	t.Helper()
	raw, err := buildItemValue(kvsItem{
		HasBeenPlayed:   hasBeenPlayed,
		BookmarkTimeSec: bookmarkTimeSec,
		PlayCount:       0,
		TimestampSec:    0,
		UPPVersion:      1,
	})
	if err != nil {
		t.Fatalf("buildLiveKVSValue: %v", err)
	}
	return raw
}

func setupLiveKVSDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "MTLibrary.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE ZMTPODCAST (
			Z_PK INTEGER PRIMARY KEY, ZSUBSCRIBED INTEGER,
			ZFEEDURL TEXT, ZTITLE TEXT, ZAUTHOR TEXT, ZIMAGEURL TEXT)`,
		`CREATE TABLE ZMTEPISODE (
			Z_PK INTEGER PRIMARY KEY, ZPODCAST INTEGER,
			ZGUID TEXT, ZTITLE TEXT, ZPUBDATE REAL, ZDURATION REAL,
			ZPLAYSTATE INTEGER DEFAULT 0, ZPLAYCOUNT INTEGER DEFAULT 0,
			ZPLAYHEAD REAL DEFAULT 0.0, ZLASTDATEPLAYED REAL, ZPRICETYPE TEXT,
			ZPLAYSTATESOURCE INTEGER DEFAULT 0, ZMETADATAIDENTIFIER TEXT)`,
		`CREATE TABLE ZMTUPPMETADATA (
			Z_PK INTEGER PRIMARY KEY, ZHASBEENPLAYED INTEGER,
			ZBOOKMARKTIME REAL, ZMETADATAIDENTIFIER TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}

	exec(`INSERT INTO ZMTPODCAST VALUES (1,1,'https://feeds.example.com/show','Show','Author',NULL)`)

	// ep1: played per live KVS (no local ZMTUPPMETADATA, no ZMTEPISODE play evidence)
	exec(`INSERT INTO ZMTEPISODE (Z_PK,ZPODCAST,ZGUID,ZTITLE,ZPUBDATE,ZDURATION,ZMETADATAIDENTIFIER)
		  VALUES (1,1,'guid-1','Live KVS Played',700000000.0,3600.0,'meta-1')`)

	// ep2: in-progress per live KVS (bookmarkTime > 0)
	exec(`INSERT INTO ZMTEPISODE (Z_PK,ZPODCAST,ZGUID,ZTITLE,ZPUBDATE,ZDURATION,ZMETADATAIDENTIFIER)
		  VALUES (2,1,'guid-2','Live KVS InProgress',699000000.0,3600.0,'meta-2')`)

	// ep3: unplayed per live KVS (HasBeenPlayed=false, bookmark=0) — would be trustedPlayed by heuristics
	exec(`INSERT INTO ZMTEPISODE (Z_PK,ZPODCAST,ZGUID,ZTITLE,ZPUBDATE,ZDURATION,ZPLAYSTATE,ZPLAYSTATESOURCE,ZMETADATAIDENTIFIER)
		  VALUES (3,1,'guid-3','Live KVS Unplayed Overrides Heuristic',698000000.0,3600.0,2,1,'meta-3')`)

	// ep4: not in live KVS at all — falls through to ZMTEPISODE heuristics (ZPLAYSTATE=2, trusted)
	exec(`INSERT INTO ZMTEPISODE (Z_PK,ZPODCAST,ZGUID,ZTITLE,ZPUBDATE,ZDURATION,ZPLAYSTATE,ZPLAYSTATESOURCE,ZMETADATAIDENTIFIER)
		  VALUES (4,1,'guid-4','Heuristic Fallback',697000000.0,3600.0,2,1,'meta-4')`)

	// ep5: not in live KVS, no metadataID — falls through to ZMTEPISODE heuristics
	exec(`INSERT INTO ZMTEPISODE (Z_PK,ZPODCAST,ZGUID,ZTITLE,ZPUBDATE,ZDURATION,ZPLAYSTATE,ZPLAYSTATESOURCE)
		  VALUES (5,1,'guid-5','No MetaID Heuristic',696000000.0,3600.0,2,1)`)

	return path
}

func readLibraryWithLiveKVS(t *testing.T, path string, rawValues map[string][]byte) *model.Library {
	t.Helper()
	r := NewSQLiteReader(path)
	r.SetLiveKVSValues(rawValues)
	lib, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return lib
}

func findEpisodeByGUID(lib *model.Library, guid string) *model.EpisodeState {
	for i := range lib.Episodes {
		if lib.Episodes[i].GUID == guid {
			return &lib.Episodes[i]
		}
	}
	return nil
}

func TestSQLiteReader_LiveKVS_PlayedIncluded(t *testing.T) {
	path := setupLiveKVSDB(t)
	rawValues := map[string][]byte{
		"meta-1": buildLiveKVSValue(t, true, 0),
		"meta-2": buildLiveKVSValue(t, false, 450.0),
		"meta-3": buildLiveKVSValue(t, false, 0),
	}
	lib := readLibraryWithLiveKVS(t, path, rawValues)

	ep := findEpisodeByGUID(lib, "guid-1")
	if ep == nil {
		t.Fatal("guid-1: expected episode to be included, got nil")
	}
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("guid-1: PlayState = %v, want PlayStatePlayed", ep.PlayState)
	}
}

func TestSQLiteReader_LiveKVS_InProgressIncluded(t *testing.T) {
	path := setupLiveKVSDB(t)
	rawValues := map[string][]byte{
		"meta-1": buildLiveKVSValue(t, true, 0),
		"meta-2": buildLiveKVSValue(t, false, 450.0),
		"meta-3": buildLiveKVSValue(t, false, 0),
	}
	lib := readLibraryWithLiveKVS(t, path, rawValues)

	ep := findEpisodeByGUID(lib, "guid-2")
	if ep == nil {
		t.Fatal("guid-2: expected episode to be included, got nil")
	}
	if ep.PlayState != model.PlayStateInProgress {
		t.Errorf("guid-2: PlayState = %v, want PlayStateInProgress", ep.PlayState)
	}
	wantPos := 450 * 1e9 // 450 seconds in nanoseconds
	if float64(ep.PlayPosition) != wantPos {
		t.Errorf("guid-2: PlayPosition = %v, want 450s", ep.PlayPosition)
	}
}

func TestSQLiteReader_LiveKVS_UnplayedOverridesTrustedPlayed(t *testing.T) {
	path := setupLiveKVSDB(t)
	rawValues := map[string][]byte{
		"meta-1": buildLiveKVSValue(t, true, 0),
		"meta-2": buildLiveKVSValue(t, false, 450.0),
		"meta-3": buildLiveKVSValue(t, false, 0),
	}
	lib := readLibraryWithLiveKVS(t, path, rawValues)

	// ep3 has ZPLAYSTATE=2/source=1 (would be trustedPlayed) but live KVS says unplayed
	ep := findEpisodeByGUID(lib, "guid-3")
	if ep != nil {
		t.Errorf("guid-3: expected episode to be excluded (live KVS says unplayed), got PlayState=%v", ep.PlayState)
	}
}

func TestSQLiteReader_LiveKVS_FallsBackToHeuristics_WhenNotInKVS(t *testing.T) {
	path := setupLiveKVSDB(t)
	rawValues := map[string][]byte{
		"meta-1": buildLiveKVSValue(t, true, 0),
		"meta-2": buildLiveKVSValue(t, false, 450.0),
		"meta-3": buildLiveKVSValue(t, false, 0),
		// meta-4 intentionally absent → heuristics apply
	}
	lib := readLibraryWithLiveKVS(t, path, rawValues)

	// ep4: not in live KVS, ZPLAYSTATE=2/source=1 → trustedPlayed heuristic fires
	ep := findEpisodeByGUID(lib, "guid-4")
	if ep == nil {
		t.Fatal("guid-4: expected episode to be included via heuristics, got nil")
	}
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("guid-4: PlayState = %v, want PlayStatePlayed", ep.PlayState)
	}
}

func TestSQLiteReader_LiveKVS_FallsBackToHeuristics_WhenNoMetaID(t *testing.T) {
	path := setupLiveKVSDB(t)
	rawValues := map[string][]byte{
		"meta-1": buildLiveKVSValue(t, true, 0),
		"meta-2": buildLiveKVSValue(t, false, 450.0),
		"meta-3": buildLiveKVSValue(t, false, 0),
	}
	lib := readLibraryWithLiveKVS(t, path, rawValues)

	// ep5: no ZMETADATAIDENTIFIER → live KVS cannot be checked → heuristics apply
	ep := findEpisodeByGUID(lib, "guid-5")
	if ep == nil {
		t.Fatal("guid-5: expected episode to be included via heuristics (no metaID), got nil")
	}
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("guid-5: PlayState = %v, want PlayStatePlayed", ep.PlayState)
	}
}
