package cmd

// Tests for pure helper functions in observe.go.
// All tests are offline — no real Apple Podcasts database or system
// preferences are required.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ---- fromCoreData ----

// CoreData timestamps are seconds since 2001-01-01 UTC.
var coreDataEpochObsTest = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFromCoreData_KnownTimestamp(t *testing.T) {
	// 1 hour after the CoreData epoch should be 2001-01-01 01:00:00 UTC.
	got := fromCoreData(3600)
	want := coreDataEpochObsTest.Add(time.Hour)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFromCoreData_ZeroIsEpoch(t *testing.T) {
	got := fromCoreData(0)
	if !got.Equal(coreDataEpochObsTest) {
		t.Errorf("got %v, want CoreData epoch %v", got, coreDataEpochObsTest)
	}
}

func TestFromCoreData_KnownDate(t *testing.T) {
	// 2024-01-15T12:00:00Z is 725457600 seconds after 2001-01-01T00:00:00Z.
	want := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	secs := want.Sub(coreDataEpochObsTest).Seconds()
	got := fromCoreData(secs)
	if !got.Round(time.Second).Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// ---- nullStr ----

func TestNullStr_Valid(t *testing.T) {
	v := sql.NullString{String: "hello", Valid: true}
	if got := nullStr(v); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestNullStr_Invalid(t *testing.T) {
	v := sql.NullString{Valid: false}
	if got := nullStr(v); got != "NULL" {
		t.Errorf("got %q, want %q", got, "NULL")
	}
}

func TestNullStr_EmptyString_Valid(t *testing.T) {
	v := sql.NullString{String: "", Valid: true}
	if got := nullStr(v); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// ---- cdateStr ----

func TestCdateStr_Invalid_ReturnsNULL(t *testing.T) {
	v := sql.NullFloat64{Valid: false}
	if got := cdateStr(v); got != "NULL" {
		t.Errorf("got %q, want %q", got, "NULL")
	}
}

func TestCdateStr_Valid_ContainsRawValue(t *testing.T) {
	v := sql.NullFloat64{Float64: 3600, Valid: true}
	got := cdateStr(v)
	if !strings.Contains(got, "3600") {
		t.Errorf("cdateStr(%v): expected output to contain raw value 3600; got %q", v, got)
	}
}

func TestCdateStr_Valid_ContainsFormattedTime(t *testing.T) {
	// 3600 seconds after CoreData epoch → 01:00:00 UTC (in local time the
	// output format is "HH:MM:SS" in local zone, but we can at least check
	// the result is non-empty and parens-formatted).
	v := sql.NullFloat64{Float64: 3600, Valid: true}
	got := cdateStr(v)
	if !strings.Contains(got, "(") || !strings.Contains(got, ")") {
		t.Errorf("cdateStr: expected formatted time in parens; got %q", got)
	}
}

// ---- tsLookup ----

func TestTsLookup_Invalid_ReturnsNULL(t *testing.T) {
	m := map[int64]string{1: "bundleID"}
	v := sql.NullInt64{Valid: false}
	if got := tsLookup(m, v); got != "NULL" {
		t.Errorf("got %q, want NULL", got)
	}
}

func TestTsLookup_FoundInMap(t *testing.T) {
	m := map[int64]string{42: "com.apple.podcasts"}
	v := sql.NullInt64{Int64: 42, Valid: true}
	got := tsLookup(m, v)
	if !strings.Contains(got, "com.apple.podcasts") {
		t.Errorf("got %q, expected it to contain the string value", got)
	}
}

func TestTsLookup_NotInMap_ReturnsIDOnly(t *testing.T) {
	m := map[int64]string{1: "other"}
	v := sql.NullInt64{Int64: 99, Valid: true}
	got := tsLookup(m, v)
	if !strings.Contains(got, "99") {
		t.Errorf("got %q, expected it to contain id=99", got)
	}
}

func TestTsLookup_NilMap_ReturnsIDOnly(t *testing.T) {
	v := sql.NullInt64{Int64: 7, Valid: true}
	got := tsLookup(nil, v)
	if !strings.Contains(got, "7") {
		t.Errorf("got %q, expected it to contain id=7", got)
	}
}

// ---- diffEpisode ----

// captureStdout redirects os.Stdout to a buffer for the duration of fn, then
// returns everything printed.
func captureStdout(fn func()) string {
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	return buf.String()
}

func TestDiffEpisode_NoChangeProducesNoOutput(t *testing.T) {
	ep := epSnap{
		pk: 1, title: "My Episode",
		playState: 2, playStateSource: 1, playStateManuallyset: 0,
		playHead: 100.0, playCount: 1,
		zopt: 5,
	}
	out := captureStdout(func() { diffEpisode(ep, ep) })
	if out != "" {
		t.Errorf("expected no output for identical snaps, got:\n%s", out)
	}
}

func TestDiffEpisode_PlayStateChangeIsReported(t *testing.T) {
	prev := epSnap{pk: 1, title: "My Episode", playState: 0, zopt: 1}
	cur := epSnap{pk: 1, title: "My Episode", playState: 2, zopt: 2}
	out := captureStdout(func() { diffEpisode(prev, cur) })
	if !strings.Contains(out, "ZPLAYSTATE") {
		t.Errorf("expected ZPLAYSTATE in diff output; got:\n%s", out)
	}
}

func TestDiffEpisode_PlayHeadChangeIsReported(t *testing.T) {
	prev := epSnap{pk: 1, title: "Ep", playHead: 0.0, zopt: 1}
	cur := epSnap{pk: 1, title: "Ep", playHead: 300.0, zopt: 2}
	out := captureStdout(func() { diffEpisode(prev, cur) })
	if !strings.Contains(out, "ZPLAYHEAD") {
		t.Errorf("expected ZPLAYHEAD in diff output; got:\n%s", out)
	}
}

func TestDiffEpisode_MultipleChangesReportedTogether(t *testing.T) {
	prev := epSnap{playState: 0, playCount: 0, playHead: 0.0, zopt: 1}
	cur := epSnap{playState: 2, playCount: 1, playHead: 0.0, zopt: 2}
	out := captureStdout(func() { diffEpisode(prev, cur) })
	if !strings.Contains(out, "ZPLAYSTATE") {
		t.Errorf("ZPLAYSTATE missing from output:\n%s", out)
	}
	if !strings.Contains(out, "ZPLAYCOUNT") {
		t.Errorf("ZPLAYCOUNT missing from output:\n%s", out)
	}
	// ZPLAYHEAD did not change — should not appear.
	if strings.Contains(out, "ZPLAYHEAD") {
		t.Errorf("ZPLAYHEAD should not appear when value is unchanged:\n%s", out)
	}
}

// ---- queryEpisodes ----

// setupObserveDB creates an in-memory SQLite DB with the minimal tables that
// queryEpisodes requires.
func setupObserveDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE ZMTPODCAST (
			Z_PK        INTEGER PRIMARY KEY,
			ZSUBSCRIBED INTEGER,
			ZFEEDURL    TEXT,
			ZTITLE      TEXT
		);
		CREATE TABLE ZMTEPISODE (
			Z_PK                        INTEGER PRIMARY KEY,
			Z_OPT                       INTEGER DEFAULT 1,
			ZPODCAST                    INTEGER,
			ZTITLE                      TEXT,
			ZPLAYSTATE                  INTEGER DEFAULT 0,
			ZPLAYSTATESOURCE            INTEGER DEFAULT 0,
			ZPLAYSTATEMANUALLYSET       INTEGER DEFAULT 0,
			ZPLAYHEAD                   REAL    DEFAULT 0.0,
			ZPLAYCOUNT                  INTEGER DEFAULT 0,
			ZLASTDATEPLAYED             REAL,
			ZLASTUSERMARKEDASPLAYEDDATE REAL,
			ZPLAYSTATELASTMODIFIEDDATE  REAL
		);
	`)
	if err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func TestQueryEpisodes_ReturnsSubscribedHTTPEpisodes(t *testing.T) {
	db := setupObserveDB(t)

	_, _ = db.Exec(`INSERT INTO ZMTPODCAST (Z_PK, ZSUBSCRIBED, ZFEEDURL, ZTITLE)
		VALUES (1, 1, 'https://feeds.example.com/show', 'My Show')`)
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE (Z_PK, Z_OPT, ZPODCAST, ZTITLE, ZPLAYSTATE)
		VALUES (10, 1, 1, 'Episode One', 0)`)

	snaps, err := queryEpisodes(context.Background(), db, "", "")
	if err != nil {
		t.Fatalf("queryEpisodes: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snap, got %d", len(snaps))
	}
	if snaps[0].title != "Episode One" {
		t.Errorf("title = %q, want Episode One", snaps[0].title)
	}
}

func TestQueryEpisodes_ExcludesUnsubscribedPodcasts(t *testing.T) {
	db := setupObserveDB(t)

	_, _ = db.Exec(`INSERT INTO ZMTPODCAST (Z_PK, ZSUBSCRIBED, ZFEEDURL, ZTITLE)
		VALUES (1, 0, 'https://feeds.example.com/old', 'Old Show')`)
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE (Z_PK, Z_OPT, ZPODCAST, ZTITLE)
		VALUES (10, 1, 1, 'Old Episode')`)

	snaps, err := queryEpisodes(context.Background(), db, "", "")
	if err != nil {
		t.Fatalf("queryEpisodes: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("expected 0 snaps for unsubscribed podcast, got %d", len(snaps))
	}
}

func TestQueryEpisodes_ExcludesNonHTTPFeeds(t *testing.T) {
	db := setupObserveDB(t)

	_, _ = db.Exec(`INSERT INTO ZMTPODCAST (Z_PK, ZSUBSCRIBED, ZFEEDURL, ZTITLE)
		VALUES (1, 1, 'internal://com.apple.internal.show', 'Internal Show')`)
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE (Z_PK, Z_OPT, ZPODCAST, ZTITLE)
		VALUES (10, 1, 1, 'Internal Episode')`)

	snaps, err := queryEpisodes(context.Background(), db, "", "")
	if err != nil {
		t.Fatalf("queryEpisodes: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("expected 0 snaps for internal:// feed, got %d", len(snaps))
	}
}

func TestQueryEpisodes_PodcastFilter(t *testing.T) {
	db := setupObserveDB(t)

	_, _ = db.Exec(`INSERT INTO ZMTPODCAST (Z_PK, ZSUBSCRIBED, ZFEEDURL, ZTITLE)
		VALUES (1, 1, 'https://feeds.example.com/alpha', 'Alpha Show')`)
	_, _ = db.Exec(`INSERT INTO ZMTPODCAST (Z_PK, ZSUBSCRIBED, ZFEEDURL, ZTITLE)
		VALUES (2, 1, 'https://feeds.example.com/beta', 'Beta Podcast')`)
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE (Z_PK, Z_OPT, ZPODCAST, ZTITLE) VALUES (1, 1, 1, 'Alpha Ep')`)
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE (Z_PK, Z_OPT, ZPODCAST, ZTITLE) VALUES (2, 1, 2, 'Beta Ep')`)

	snaps, err := queryEpisodes(context.Background(), db, "alpha", "")
	if err != nil {
		t.Fatalf("queryEpisodes: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snap with podcast filter 'alpha', got %d", len(snaps))
	}
	if snaps[0].title != "Alpha Ep" {
		t.Errorf("title = %q, want Alpha Ep", snaps[0].title)
	}
}

func TestQueryEpisodes_EpisodeFilter(t *testing.T) {
	db := setupObserveDB(t)

	_, _ = db.Exec(`INSERT INTO ZMTPODCAST (Z_PK, ZSUBSCRIBED, ZFEEDURL, ZTITLE)
		VALUES (1, 1, 'https://feeds.example.com/show', 'My Show')`)
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE (Z_PK, Z_OPT, ZPODCAST, ZTITLE) VALUES (1, 1, 1, 'Morning News')`)
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE (Z_PK, Z_OPT, ZPODCAST, ZTITLE) VALUES (2, 1, 1, 'Evening Wrap')`)

	snaps, err := queryEpisodes(context.Background(), db, "", "evening")
	if err != nil {
		t.Fatalf("queryEpisodes: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snap with episode filter 'evening', got %d", len(snaps))
	}
	if snaps[0].title != "Evening Wrap" {
		t.Errorf("title = %q, want Evening Wrap", snaps[0].title)
	}
}

func TestQueryEpisodes_PlayStateFieldsPopulated(t *testing.T) {
	db := setupObserveDB(t)

	_, _ = db.Exec(`INSERT INTO ZMTPODCAST (Z_PK, ZSUBSCRIBED, ZFEEDURL, ZTITLE)
		VALUES (1, 1, 'https://feeds.example.com/show', 'My Show')`)
	_, _ = db.Exec(`INSERT INTO ZMTEPISODE
		(Z_PK, Z_OPT, ZPODCAST, ZTITLE, ZPLAYSTATE, ZPLAYSTATESOURCE, ZPLAYHEAD, ZPLAYCOUNT)
		VALUES (1, 3, 1, 'My Episode', 2, 1, 450.5, 1)`)

	snaps, err := queryEpisodes(context.Background(), db, "", "")
	if err != nil {
		t.Fatalf("queryEpisodes: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snap, got %d", len(snaps))
	}
	s := snaps[0]
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"playState", s.playState, int64(2)},
		{"playStateSource", s.playStateSource, int64(1)},
		{"playHead", fmt.Sprintf("%.1f", s.playHead), "450.5"},
		{"playCount", s.playCount, int64(1)},
		{"zopt", s.zopt, int64(3)},
	}
	for _, c := range checks {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}
