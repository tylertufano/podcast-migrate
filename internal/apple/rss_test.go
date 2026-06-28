package apple

import (
	"testing"
	"time"
)

// ---- parsePubDate ----

func TestParsePubDate_RFC1123Z(t *testing.T) {
	got := parsePubDate("Mon, 15 Jan 2024 09:30:00 +0000")
	want := time.Date(2024, 1, 15, 9, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePubDate_RFC1123Z_WithOffset(t *testing.T) {
	got := parsePubDate("Mon, 15 Jan 2024 09:30:00 -0500")
	want := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePubDate_RFC1123_UTCNamedZone(t *testing.T) {
	got := parsePubDate("Mon, 15 Jan 2024 09:30:00 UTC")
	if got.IsZero() {
		t.Error("expected non-zero time for RFC1123 with UTC timezone")
	}
	if got.Hour() != 9 || got.Minute() != 30 {
		t.Errorf("time fields wrong: got %v", got)
	}
}

func TestParsePubDate_SingleDigitDay(t *testing.T) {
	got := parsePubDate("Mon, 1 Jan 2024 00:00:00 +0000")
	want := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePubDate_SingleDigitDay_WithOffset(t *testing.T) {
	got := parsePubDate("Mon, 1 Jan 2024 00:00:00 -0700")
	want := time.Date(2024, 1, 1, 7, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePubDate_ISO8601_UTC(t *testing.T) {
	got := parsePubDate("2024-01-15T09:30:00Z")
	want := time.Date(2024, 1, 15, 9, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePubDate_ISO8601_WithOffset(t *testing.T) {
	got := parsePubDate("2024-01-15T09:30:00-05:00")
	want := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePubDate_ResultIsUTC(t *testing.T) {
	got := parsePubDate("Mon, 15 Jan 2024 09:30:00 -0800")
	if got.Location() != time.UTC {
		t.Errorf("expected UTC location, got %v", got.Location())
	}
}

func TestParsePubDate_Empty_ReturnsZero(t *testing.T) {
	got := parsePubDate("")
	if !got.IsZero() {
		t.Errorf("expected zero time for empty input, got %v", got)
	}
}

func TestParsePubDate_Invalid_ReturnsZero(t *testing.T) {
	got := parsePubDate("not a date at all")
	if !got.IsZero() {
		t.Errorf("expected zero time for invalid input, got %v", got)
	}
}

func TestParsePubDate_WhitespaceOnly_ReturnsZero(t *testing.T) {
	got := parsePubDate("   ")
	if !got.IsZero() {
		t.Errorf("expected zero time for whitespace-only input, got %v", got)
	}
}

// ---- parseItunesDuration ----

func TestParseItunesDuration_HHMMSS(t *testing.T) {
	got := parseItunesDuration("1:30:45")
	want := time.Duration(1*3600+30*60+45) * time.Second
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseItunesDuration_HHMMSS_Zero(t *testing.T) {
	got := parseItunesDuration("0:00:00")
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestParseItunesDuration_MMSS(t *testing.T) {
	got := parseItunesDuration("30:45")
	want := time.Duration(30*60+45) * time.Second
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseItunesDuration_SecondsOnly(t *testing.T) {
	got := parseItunesDuration("3600")
	if got != time.Hour {
		t.Errorf("got %v, want 1h", got)
	}
}

func TestParseItunesDuration_SecondsUnit_IsSeconds(t *testing.T) {
	// Verify that plain integers are treated as seconds, not minutes.
	if got := parseItunesDuration("60"); got != time.Minute {
		t.Errorf("60 → got %v, want 1 minute (60 seconds)", got)
	}
}

func TestParseItunesDuration_Empty_ReturnsZero(t *testing.T) {
	if got := parseItunesDuration(""); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestParseItunesDuration_WhitespaceOnly_ReturnsZero(t *testing.T) {
	if got := parseItunesDuration("   "); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestParseItunesDuration_InvalidString_ReturnsZero(t *testing.T) {
	if got := parseItunesDuration("not a duration"); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestParseItunesDuration_InvalidHHMMSS_ReturnsZero(t *testing.T) {
	if got := parseItunesDuration("x:y:z"); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}
