package migrate_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/model"
)

// ---- CSVField ----

func TestCSVField_PlainString(t *testing.T) {
	if got := migrate.CSVField("hello world"); got != "hello world" {
		t.Errorf("CSVField plain: got %q, want %q", got, "hello world")
	}
}

func TestCSVField_Empty(t *testing.T) {
	if got := migrate.CSVField(""); got != "" {
		t.Errorf("CSVField empty: got %q, want empty", got)
	}
}

func TestCSVField_WithComma_Quoted(t *testing.T) {
	got := migrate.CSVField("Fresh Air, NPR")
	if got != `"Fresh Air, NPR"` {
		t.Errorf("CSVField with comma: got %q, want %q", got, `"Fresh Air, NPR"`)
	}
}

func TestCSVField_WithDoubleQuote_Escaped(t *testing.T) {
	got := migrate.CSVField(`say "hello"`)
	want := `"say ""hello"""`
	if got != want {
		t.Errorf("CSVField with quote: got %q, want %q", got, want)
	}
}

func TestCSVField_WithNewline_Quoted(t *testing.T) {
	got := migrate.CSVField("line1\nline2")
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("CSVField with newline: not quoted, got %q", got)
	}
}

func TestCSVField_WithCarriageReturn_Quoted(t *testing.T) {
	got := migrate.CSVField("line1\rline2")
	if !strings.HasPrefix(got, `"`) {
		t.Errorf("CSVField with CR: not quoted, got %q", got)
	}
}

// ---- PlayStateLabel ----

func TestPlayStateLabel_Played(t *testing.T) {
	if got := migrate.PlayStateLabel(model.PlayStatePlayed, 0); got != "played" {
		t.Errorf("PlayStateLabel played: got %q, want %q", got, "played")
	}
}

func TestPlayStateLabel_InProgress_WithPosition(t *testing.T) {
	got := migrate.PlayStateLabel(model.PlayStateInProgress, 65*time.Second)
	want := "in_progress(1m5s)"
	if got != want {
		t.Errorf("PlayStateLabel in-progress with pos: got %q, want %q", got, want)
	}
}

func TestPlayStateLabel_InProgress_NoPosition(t *testing.T) {
	if got := migrate.PlayStateLabel(model.PlayStateInProgress, 0); got != "in_progress" {
		t.Errorf("PlayStateLabel in-progress no pos: got %q, want %q", got, "in_progress")
	}
}

func TestPlayStateLabel_Unplayed(t *testing.T) {
	if got := migrate.PlayStateLabel(model.PlayStateUnplayed, 0); got != "unplayed" {
		t.Errorf("PlayStateLabel unplayed: got %q, want %q", got, "unplayed")
	}
}

func TestPlayStateLabel_InProgress_HoursAndMinutes(t *testing.T) {
	got := migrate.PlayStateLabel(model.PlayStateInProgress, 2*time.Hour+30*time.Minute)
	want := "in_progress(2h30m0s)"
	if got != want {
		t.Errorf("PlayStateLabel in-progress hours: got %q, want %q", got, want)
	}
}

// ---- WriteLogHeader ----

func TestWriteLogHeader_WritesExpectedColumns(t *testing.T) {
	var buf bytes.Buffer
	migrate.WriteLogHeader(&buf)
	got := buf.String()
	want := "status,podcast,episode,pub_date,source_state,target_state,note\n"
	if got != want {
		t.Errorf("WriteLogHeader: got %q, want %q", got, want)
	}
}

func TestWriteLogHeader_NilWriter_NoOp(t *testing.T) {
	// Must not panic.
	migrate.WriteLogHeader(nil)
}

// ---- WriteLogLine ----

func TestWriteLogLine_AllFields(t *testing.T) {
	var buf bytes.Buffer
	pubDate := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	migrate.WriteLogLine(&buf, "updated", "Fresh Air", "Episode One", pubDate, "unplayed", "played", "")
	got := buf.String()

	for _, substr := range []string{"updated", "Fresh Air", "Episode One", "2024-06-15", "unplayed", "played"} {
		if !strings.Contains(got, substr) {
			t.Errorf("WriteLogLine: missing %q in output %q", substr, got)
		}
	}
}

func TestWriteLogLine_ZeroPubDate_EmptyDateField(t *testing.T) {
	var buf bytes.Buffer
	migrate.WriteLogLine(&buf, "skipped", "Pod", "Ep", time.Time{}, "played", "played", "already satisfied")
	got := strings.TrimRight(buf.String(), "\n")
	parts := strings.Split(got, ",")
	if len(parts) != 7 {
		t.Fatalf("expected 7 columns, got %d: %q", len(parts), got)
	}
	if parts[3] != "" {
		t.Errorf("zero pubDate: date column = %q, want empty", parts[3])
	}
}

func TestWriteLogLine_NilWriter_NoOp(t *testing.T) {
	// Must not panic.
	migrate.WriteLogLine(nil, "updated", "Pod", "Ep", time.Now(), "unplayed", "played", "")
}

func TestWriteLogLine_FieldWithComma_Quoted(t *testing.T) {
	var buf bytes.Buffer
	migrate.WriteLogLine(&buf, "updated", "Pod, The Podcast", "Ep", time.Time{}, "played", "played", "")
	got := buf.String()
	if !strings.Contains(got, `"Pod, The Podcast"`) {
		t.Errorf("WriteLogLine: CSV field with comma should be quoted: %q", got)
	}
}

func TestWriteLogLine_NoteWithComma_Quoted(t *testing.T) {
	var buf bytes.Buffer
	migrate.WriteLogLine(&buf, "skipped", "Pod", "Ep", time.Time{}, "played", "played", "already satisfied, no update needed")
	got := buf.String()
	if !strings.Contains(got, `"already satisfied, no update needed"`) {
		t.Errorf("WriteLogLine: note with comma should be quoted: %q", got)
	}
}

func TestWriteLogLine_SevenColumns(t *testing.T) {
	var buf bytes.Buffer
	pubDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	migrate.WriteLogLine(&buf, "updated", "Show", "Ep", pubDate, "unplayed", "played", "note")
	line := strings.TrimRight(buf.String(), "\n")
	// Count commas outside quoted fields (simple check: total columns = 7)
	// This works because none of our test values have commas.
	cols := strings.Split(line, ",")
	if len(cols) != 7 {
		t.Errorf("WriteLogLine: expected 7 comma-separated columns, got %d: %q", len(cols), line)
	}
}
