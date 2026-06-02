package apple

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// ---------------------------------------------------------------------------
// Shared index-key helpers (used by both CatalogClient and tests)
// ---------------------------------------------------------------------------

// normalizeWriteFeedURL produces a canonical feed URL key for matching.
func normalizeWriteFeedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	if len(u.Path) > 1 {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	u.Fragment = ""
	return u.String()
}

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

// buildFeedToTitleFromLib returns a map from feed URL to lowercased podcast title.
func buildFeedToTitleFromLib(lib *model.Library) map[string]string {
	m := make(map[string]string, len(lib.Podcasts))
	for _, pod := range lib.Podcasts {
		if pod.FeedURL != "" {
			m[pod.FeedURL] = strings.ToLower(strings.TrimSpace(pod.Title))
		}
	}
	return m
}

// filterLibraryEpisodes returns only episodes from podcasts whose title
// contains at least one filter pattern (case-insensitive substring match).
// If filters is empty, all episodes are returned unchanged.
func filterLibraryEpisodes(episodes []model.EpisodeState, feedToTitle map[string]string, filters []string) []model.EpisodeState {
	if len(filters) == 0 {
		return episodes
	}
	lower := make([]string, len(filters))
	for i, f := range filters {
		lower[i] = strings.ToLower(strings.TrimSpace(f))
	}
	var out []model.EpisodeState
	for _, ep := range episodes {
		title := feedToTitle[ep.FeedURL]
		for _, f := range lower {
			if f != "" && strings.Contains(title, f) {
				out = append(out, ep)
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Index key constructors (feeddate / feedtitle / poddate / podtitle)
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Per-episode log helpers
// ---------------------------------------------------------------------------

func writeLogHeader(w io.Writer) {
	if w == nil {
		return
	}
	fmt.Fprintln(w, "status,podcast,episode,pub_date,source_state,target_state,note")
}

func writeLogLine(w io.Writer, status, podcast, episode string, pubDate time.Time, srcState, tgtState, note string) {
	if w == nil {
		return
	}
	dateStr := ""
	if !pubDate.IsZero() {
		dateStr = pubDate.UTC().Format("2006-01-02")
	}
	fmt.Fprintf(w, "%s,%s,%s,%s,%s,%s,%s\n",
		csvField(status), csvField(podcast), csvField(episode),
		dateStr,
		csvField(srcState), csvField(tgtState), csvField(note))
}

func playStateLabel(ps model.PlayState, pos time.Duration) string {
	switch ps {
	case model.PlayStatePlayed:
		return "played"
	case model.PlayStateInProgress:
		if pos > 0 {
			return "in_progress(" + pos.Round(time.Second).String() + ")"
		}
		return "in_progress"
	default:
		return "unplayed"
	}
}

func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\n\r") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}
