package apple

import (
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Index-key helpers (used by catalog.go and sqlite-based write paths)
// ---------------------------------------------------------------------------

// withinDateTolerance reports whether two pub dates are close enough to allow
// a title-based match. A tolerance ≤ 0 disables the guard.
func withinDateTolerance(a, b time.Time, tolerance time.Duration) bool {
	if tolerance <= 0 {
		return true
	}
	if a.IsZero() || b.IsZero() {
		return true
	}
	diff := a.Sub(b)
	if diff < 0 {
		diff = -diff
	}
	return diff <= tolerance
}

func feedDateKey(normFeedURL string, pubDate time.Time) string {
	return "feeddate:" + normFeedURL + "|" + pubDate.UTC().Format(time.RFC3339)
}

func feedTitleKey(normFeedURL, title string) string {
	return "feedtitle:" + normFeedURL + "|" + strings.ToLower(strings.TrimSpace(title))
}

func podDateKey(normPodTitle string, pubDate time.Time) string {
	return "poddate:" + normPodTitle + "|" + pubDate.UTC().Format(time.RFC3339)
}

func podTitleKey(normPodTitle, epTitle string) string {
	return "podtitle:" + normPodTitle + "|" + strings.ToLower(strings.TrimSpace(epTitle))
}
