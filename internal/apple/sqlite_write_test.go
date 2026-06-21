package apple

// White-box unit tests for the unexported helpers remaining in sqlite_write.go.
// Tests for BuildFeedToTitle, FilterEpisodesByPodcast, WriteLogLine, PlayStateLabel,
// and CSVField live in internal/migrate (those helpers now come from there).

import (
	"testing"
	"time"
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
	if !withinDateTolerance(a, b, 72*time.Hour) {
		t.Error("difference exactly equal to tolerance should be within")
	}
}
