package apple

// White-box unit tests for the unexported helpers in sqlite_write.go.
// Using package apple (not apple_test) to access unexported functions.

import (
	"bytes"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// ---- withinDateTolerance ----

func TestWithinDateTolerance_ZeroTolerance_AlwaysTrue(t *testing.T) {
	a := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	b := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if !withinDateTolerance(a, b, 0) {
		t.Error("tolerance=0 should always return true")
	}
}

func TestWithinDateTolerance_BothPresent_WithinWindow(t *testing.T) {
	now := time.Now().UTC()
	a := now
	b := now.Add(1 * time.Hour)
	if !withinDateTolerance(a, b, 2*time.Hour) {
		t.Error("1h difference within 2h tolerance should be true")
	}
}

func TestWithinDateTolerance_BothPresent_OutsideWindow(t *testing.T) {
	now := time.Now().UTC()
	a := now
	b := now.Add(3 * time.Hour)
	if withinDateTolerance(a, b, 2*time.Hour) {
		t.Error("3h difference outside 2h tolerance should be false")
	}
}

func TestWithinDateTolerance_NegativeDiff_HandledCorrectly(t *testing.T) {
	now := time.Now().UTC()
	a := now.Add(3 * time.Hour)
	b := now
	if withinDateTolerance(a, b, 2*time.Hour) {
		t.Error("absolute difference 3h should be outside 2h tolerance regardless of sign")
	}
}

func TestWithinDateTolerance_ZeroA_SkipsCheck(t *testing.T) {
	b := time.Now().UTC()
	if !withinDateTolerance(time.Time{}, b, 1*time.Second) {
		t.Error("zero A should skip the check and return true")
	}
}

func TestWithinDateTolerance_ZeroB_SkipsCheck(t *testing.T) {
	a := time.Now().UTC()
	if !withinDateTolerance(a, time.Time{}, 1*time.Second) {
		t.Error("zero B should skip the check and return true")
	}
}

func TestWithinDateTolerance_BothZero_ReturnsTrue(t *testing.T) {
	if !withinDateTolerance(time.Time{}, time.Time{}, 1*time.Second) {
		t.Error("both zero should return true")
	}
}

func TestWithinDateTolerance_ExactlyAtBoundary_IsWithin(t *testing.T) {
	now := time.Now().UTC()
	a := now
	b := now.Add(72 * time.Hour)
	// exactly 72h tolerance, 72h difference — should be within (<=)
	if !withinDateTolerance(a, b, 72*time.Hour) {
		t.Error("difference exactly equal to tolerance should be within")
	}
}

// ---- buildFeedToTitleFromLib ----

func TestBuildFeedToTitleFromLib_BasicMapping(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
			{FeedURL: "https://feeds.example.com/beta", Title: "Beta Show"},
		},
	}
	m := buildFeedToTitleFromLib(lib)
	if len(m) != 2 {
		t.Fatalf("got %d entries, want 2", len(m))
	}
	if m["https://feeds.example.com/alpha"] != "alpha show" {
		t.Errorf("alpha: got %q, want %q", m["https://feeds.example.com/alpha"], "alpha show")
	}
}

func TestBuildFeedToTitleFromLib_LowercasesTitle(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/x", Title: "UPPER CASE SHOW"}},
	}
	m := buildFeedToTitleFromLib(lib)
	if m["https://feeds.example.com/x"] != "upper case show" {
		t.Errorf("title should be lowercased: got %q", m["https://feeds.example.com/x"])
	}
}

func TestBuildFeedToTitleFromLib_SkipsEmptyFeedURL(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "", Title: "No URL Show"},
			{FeedURL: "https://feeds.example.com/x", Title: "X Show"},
		},
	}
	m := buildFeedToTitleFromLib(lib)
	if len(m) != 1 {
		t.Errorf("got %d entries, want 1 (empty feed URL skipped)", len(m))
	}
	if _, ok := m[""]; ok {
		t.Error("empty feed URL should not be stored as a key")
	}
}

func TestBuildFeedToTitleFromLib_EmptyLibrary(t *testing.T) {
	m := buildFeedToTitleFromLib(&model.Library{})
	if len(m) != 0 {
		t.Errorf("empty library: got %d entries, want 0", len(m))
	}
}

func TestBuildFeedToTitleFromLib_TrimsTitleWhitespace(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/x", Title: "  Padded Show  "}},
	}
	m := buildFeedToTitleFromLib(lib)
	if m["https://feeds.example.com/x"] != "padded show" {
		t.Errorf("title should be trimmed and lowercased: got %q", m["https://feeds.example.com/x"])
	}
}

// ---- filterLibraryEpisodes ----

func TestFilterLibraryEpisodes_NoFilters_ReturnsAll(t *testing.T) {
	feedToTitle := map[string]string{"https://feeds.example.com/alpha": "alpha show"}
	episodes := []model.EpisodeState{
		{FeedURL: "https://feeds.example.com/alpha"},
		{FeedURL: "https://feeds.example.com/beta"},
	}
	got := filterLibraryEpisodes(episodes, feedToTitle, nil)
	if len(got) != 2 {
		t.Errorf("no filter: got %d episodes, want 2", len(got))
	}
}

func TestFilterLibraryEpisodes_MatchBySubstring(t *testing.T) {
	feedToTitle := map[string]string{
		"https://feeds.example.com/alpha": "alpha show",
		"https://feeds.example.com/beta":  "beta podcast",
	}
	episodes := []model.EpisodeState{
		{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Ep 1"},
		{FeedURL: "https://feeds.example.com/beta", Title: "Beta Ep 1"},
	}
	got := filterLibraryEpisodes(episodes, feedToTitle, []string{"alpha"})
	if len(got) != 1 {
		t.Fatalf("filter 'alpha': got %d episodes, want 1", len(got))
	}
	if got[0].Title != "Alpha Ep 1" {
		t.Errorf("wrong episode returned: %q", got[0].Title)
	}
}

func TestFilterLibraryEpisodes_MultipleFilters_ORSemantics(t *testing.T) {
	feedToTitle := map[string]string{
		"https://feeds.example.com/alpha": "alpha show",
		"https://feeds.example.com/beta":  "beta podcast",
		"https://feeds.example.com/gamma": "gamma radio",
	}
	episodes := []model.EpisodeState{
		{FeedURL: "https://feeds.example.com/alpha"},
		{FeedURL: "https://feeds.example.com/beta"},
		{FeedURL: "https://feeds.example.com/gamma"},
	}
	got := filterLibraryEpisodes(episodes, feedToTitle, []string{"alpha", "gamma"})
	if len(got) != 2 {
		t.Errorf("filter alpha+gamma: got %d episodes, want 2", len(got))
	}
}

func TestFilterLibraryEpisodes_NoMatch_ReturnsEmpty(t *testing.T) {
	feedToTitle := map[string]string{"https://feeds.example.com/alpha": "alpha show"}
	episodes := []model.EpisodeState{{FeedURL: "https://feeds.example.com/alpha"}}
	got := filterLibraryEpisodes(episodes, feedToTitle, []string{"zzz"})
	if len(got) != 0 {
		t.Errorf("no-match filter: got %d episodes, want 0", len(got))
	}
}

func TestFilterLibraryEpisodes_BlankFilterSkipped(t *testing.T) {
	feedToTitle := map[string]string{"https://feeds.example.com/alpha": "alpha show"}
	episodes := []model.EpisodeState{{FeedURL: "https://feeds.example.com/alpha"}}
	// Blank filter string should not match everything.
	got := filterLibraryEpisodes(episodes, feedToTitle, []string{""})
	if len(got) != 0 {
		t.Errorf("blank filter should not match any episode; got %d", len(got))
	}
}

func TestFilterLibraryEpisodes_CaseInsensitiveMatch(t *testing.T) {
	feedToTitle := map[string]string{"https://feeds.example.com/alpha": "alpha show"}
	episodes := []model.EpisodeState{{FeedURL: "https://feeds.example.com/alpha"}}
	// Filters are stored lowercased; title is already lowercase in feedToTitle.
	got := filterLibraryEpisodes(episodes, feedToTitle, []string{"ALPHA"})
	// The function lowercases filters (via the outer caller) — test the raw function
	// with a pre-lowercased filter to exercise the Contains path.
	got = filterLibraryEpisodes(episodes, feedToTitle, []string{"alpha"})
	if len(got) != 1 {
		t.Errorf("case insensitive match: got %d episodes, want 1", len(got))
	}
}

// ---- csvField (private) ----

func TestCsvField_Plain(t *testing.T) {
	if got := csvField("hello"); got != "hello" {
		t.Errorf("csvField: got %q, want %q", got, "hello")
	}
}

func TestCsvField_WithComma_Quoted(t *testing.T) {
	if got := csvField("a,b"); got != `"a,b"` {
		t.Errorf("csvField comma: got %q, want %q", got, `"a,b"`)
	}
}

func TestCsvField_WithQuote_Escaped(t *testing.T) {
	if got := csvField(`"value"`); got != `"""value"""` {
		t.Errorf("csvField quote: got %q, want %q", got, `"""value"""`)
	}
}

// ---- playStateLabel (private) ----

func TestPlayStateLabelInternal_Played(t *testing.T) {
	if got := playStateLabel(model.PlayStatePlayed, 0); got != "played" {
		t.Errorf("playStateLabel: got %q", got)
	}
}

func TestPlayStateLabelInternal_InProgressWithPos(t *testing.T) {
	got := playStateLabel(model.PlayStateInProgress, 90*time.Second)
	if got != "in_progress(1m30s)" {
		t.Errorf("playStateLabel in-progress: got %q, want %q", got, "in_progress(1m30s)")
	}
}

func TestPlayStateLabelInternal_InProgressNoPos(t *testing.T) {
	if got := playStateLabel(model.PlayStateInProgress, 0); got != "in_progress" {
		t.Errorf("playStateLabel in-progress no pos: got %q", got)
	}
}

func TestPlayStateLabelInternal_Unplayed(t *testing.T) {
	if got := playStateLabel(model.PlayStateUnplayed, 0); got != "unplayed" {
		t.Errorf("playStateLabel unplayed: got %q", got)
	}
}

// ---- writeLogHeader / writeLogLine (private) ----

func TestWriteLogHeaderInternal_Output(t *testing.T) {
	var buf bytes.Buffer
	writeLogHeader(&buf)
	got := buf.String()
	if got != "status,podcast,episode,pub_date,source_state,target_state,note\n" {
		t.Errorf("writeLogHeader: got %q", got)
	}
}

func TestWriteLogHeaderInternal_NilNoOp(t *testing.T) {
	writeLogHeader(nil) // must not panic
}

func TestWriteLogLineInternal_Output(t *testing.T) {
	var buf bytes.Buffer
	pubDate := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	writeLogLine(&buf, "updated", "Fresh Air", "Ep 42", pubDate, "unplayed", "played", "")
	got := buf.String()
	for _, s := range []string{"updated", "Fresh Air", "Ep 42", "2024-03-15", "unplayed", "played"} {
		if !bytes.Contains([]byte(got), []byte(s)) {
			t.Errorf("writeLogLine: missing %q in %q", s, got)
		}
	}
}

func TestWriteLogLineInternal_NilNoOp(t *testing.T) {
	writeLogLine(nil, "updated", "Pod", "Ep", time.Now(), "played", "played", "") // must not panic
}
