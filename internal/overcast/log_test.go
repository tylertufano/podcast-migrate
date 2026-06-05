package overcast

// Tests for log.go: csvField, playStateLabel, writeLogHeader, writeLogLine.

import (
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// ---- csvField ----

func TestCSVField_PlainString(t *testing.T) {
	cases := []string{"hello", "world", "simple", ""}
	for _, s := range cases {
		if got := csvField(s); got != s {
			t.Errorf("csvField(%q) = %q, want unchanged", s, got)
		}
	}
}

func TestCSVField_WithComma_IsQuoted(t *testing.T) {
	got := csvField("hello, world")
	if got != `"hello, world"` {
		t.Errorf("csvField with comma: got %q, want %q", got, `"hello, world"`)
	}
}

func TestCSVField_WithDoubleQuote_IsEscaped(t *testing.T) {
	// A quote inside the value must be doubled per RFC 4180.
	got := csvField(`say "hello"`)
	want := `"say ""hello"""`
	if got != want {
		t.Errorf("csvField with quote: got %q, want %q", got, want)
	}
}

func TestCSVField_WithNewline_IsQuoted(t *testing.T) {
	got := csvField("line1\nline2")
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("csvField with newline should be quoted: got %q", got)
	}
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") {
		t.Errorf("csvField with newline should preserve content: got %q", got)
	}
}

func TestCSVField_WithCarriageReturn_IsQuoted(t *testing.T) {
	got := csvField("a\rb")
	if !strings.HasPrefix(got, `"`) {
		t.Errorf("csvField with \\r should be quoted: got %q", got)
	}
}

func TestCSVField_Empty(t *testing.T) {
	if got := csvField(""); got != "" {
		t.Errorf("csvField(%q) = %q, want empty", "", got)
	}
}

// ---- playStateLabel ----

func TestPlayStateLabel_Played(t *testing.T) {
	if got := playStateLabel(model.PlayStatePlayed, 0); got != "played" {
		t.Errorf("PlayStatePlayed: got %q, want %q", got, "played")
	}
}

func TestPlayStateLabel_Unplayed(t *testing.T) {
	if got := playStateLabel(model.PlayStateUnplayed, 0); got != "unplayed" {
		t.Errorf("PlayStateUnplayed: got %q, want %q", got, "unplayed")
	}
}

func TestPlayStateLabel_InProgress_WithPosition(t *testing.T) {
	cases := []struct {
		pos  time.Duration
		want string
	}{
		{90 * time.Second, "in_progress(1m30s)"},
		{3600 * time.Second, "in_progress(1h0m0s)"},
		{61 * time.Second, "in_progress(1m1s)"},
		{30 * time.Second, "in_progress(30s)"},
	}
	for _, tc := range cases {
		got := playStateLabel(model.PlayStateInProgress, tc.pos)
		if got != tc.want {
			t.Errorf("InProgress(%v): got %q, want %q", tc.pos, got, tc.want)
		}
	}
}

func TestPlayStateLabel_InProgress_ZeroPosition(t *testing.T) {
	// Zero position for InProgress → no position suffix.
	if got := playStateLabel(model.PlayStateInProgress, 0); got != "in_progress" {
		t.Errorf("InProgress(0): got %q, want %q", got, "in_progress")
	}
}

// ---- writeLogHeader ----

func TestWriteLogHeader_NilWriter_NoOp(t *testing.T) {
	// Must not panic when w is nil.
	writeLogHeader(nil)
}

func TestWriteLogHeader_WritesAllColumns(t *testing.T) {
	var b strings.Builder
	writeLogHeader(&b)
	out := b.String()

	wantCols := []string{"status", "podcast", "episode", "pub_date", "source_state", "target_state", "note"}
	for _, col := range wantCols {
		if !strings.Contains(out, col) {
			t.Errorf("header missing column %q: %q", col, out)
		}
	}
}

func TestWriteLogHeader_EndsWithNewline(t *testing.T) {
	var b strings.Builder
	writeLogHeader(&b)
	if !strings.HasSuffix(b.String(), "\n") {
		t.Errorf("header should end with newline: %q", b.String())
	}
}

// ---- writeLogLine ----

func TestWriteLogLine_NilWriter_NoOp(t *testing.T) {
	// Must not panic when w is nil.
	writeLogLine(nil, "updated", "My Pod", "Ep Title", time.Time{}, "played", "unplayed", "")
}

func TestWriteLogLine_ContainsAllFields(t *testing.T) {
	var b strings.Builder
	pubDate := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	writeLogLine(&b, "updated", "My Pod", "Ep Title", pubDate, "played", "unplayed", "a note")
	out := b.String()

	for _, want := range []string{"updated", "My Pod", "Ep Title", "2024-06-15", "played", "unplayed", "a note"} {
		if !strings.Contains(out, want) {
			t.Errorf("log line missing %q: %q", want, out)
		}
	}
}

func TestWriteLogLine_ZeroPubDate_EmptyDateField(t *testing.T) {
	var b strings.Builder
	writeLogLine(&b, "not_found", "Pod", "Ep", time.Time{}, "played", "—", "")
	out := b.String()
	// Date should be absent — no year like 0001 or 1970.
	if strings.Contains(out, "0001") || strings.Contains(out, "1970") {
		t.Errorf("zero pubDate should produce empty date column: %q", out)
	}
}

func TestWriteLogLine_QuotesPodcastWithComma(t *testing.T) {
	var b strings.Builder
	writeLogLine(&b, "updated", "Pod, The", "Ep", time.Time{}, "played", "unplayed", "")
	out := b.String()
	if !strings.Contains(out, `"Pod, The"`) {
		t.Errorf("podcast name with comma should be quoted in CSV output: %q", out)
	}
}

func TestWriteLogLine_QuotesEpisodeTitleWithComma(t *testing.T) {
	var b strings.Builder
	writeLogLine(&b, "updated", "Pod", "Ep, Part 2", time.Time{}, "played", "unplayed", "")
	out := b.String()
	if !strings.Contains(out, `"Ep, Part 2"`) {
		t.Errorf("episode title with comma should be quoted: %q", out)
	}
}

func TestWriteLogLine_EndsWithNewline(t *testing.T) {
	var b strings.Builder
	writeLogLine(&b, "updated", "Pod", "Ep", time.Time{}, "played", "unplayed", "")
	if !strings.HasSuffix(b.String(), "\n") {
		t.Errorf("log line should end with newline: %q", b.String())
	}
}

func TestWriteLogLine_CorrectColumnCount(t *testing.T) {
	var b strings.Builder
	pubDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	writeLogLine(&b, "updated", "Pod", "Ep", pubDate, "played", "unplayed", "")
	// Seven comma-separated fields: status,podcast,episode,pub_date,source_state,target_state,note
	line := strings.TrimRight(b.String(), "\n")
	fields := strings.Split(line, ",")
	if len(fields) != 7 {
		t.Errorf("expected 7 CSV fields, got %d: %q", len(fields), line)
	}
}
