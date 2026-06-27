package migrate

// match.go contains episode and feed matching utilities shared by all
// write-side providers (Overcast, Pocket Casts) and the sync engine.
//
// These were previously duplicated across internal/overcast, internal/pocketcasts,
// and internal/sync. Centralising them ensures consistent behaviour and makes
// bug fixes (e.g. the ±1-day title-guard) apply everywhere.

import (
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// Regexes used by FuzzyNormalizeTitle.
var (
	// fuzzySeasonRe matches season markers: "S01", "S1", "Season 1", "Season 01", etc.
	fuzzySeasonRe = regexp.MustCompile(`(?i)(?:season\s+\d+|\bs\d{1,3}\b)`)
	// fuzzyApostropheRe matches apostrophes and typographic equivalents.
	// These are removed (not replaced with a space) so "O'Brien" and "OBrien"
	// normalise to the same string "obrien", enabling podcast title matching
	// across databases that may or may not include apostrophes.
	// Note: Unicode escapes inside [...] require the \x{NNNN} form in RE2.
	fuzzyApostropheRe = regexp.MustCompile(`['\x60\x{2018}\x{2019}]`)
	// fuzzyNonAlnumRe matches any remaining non-alphanumeric, non-space character
	// after the apostrophe pass. These are replaced with a space (e.g. hyphens
	// in "Self-Help" become a word boundary rather than nothing).
	fuzzyNonAlnumRe = regexp.MustCompile(`[^a-z0-9 ]`)
)

// NormalizeFeedURL returns a canonical form of a podcast feed URL used as a
// matching key when comparing feed URLs across providers. It:
//   - lowercases scheme and host (RFC 3986 requires these to be case-insensitive)
//   - promotes http to https (treating the two schemes as equivalent for matching)
//   - strips a trailing slash from the path for canonical form
//   - drops the fragment (never meaningful for feed identity)
//
// Query parameters are preserved because some feeds use them as part of their
// identity (e.g. ?feed=rss2). Apple's cache-buster params (?t=...) are already
// stripped upstream before a URL reaches this function.
//
// URLs are never rewritten to a different host — index URLs (e.g. from the
// iTunes Search API) and user subscription URLs are treated as canonical as-is.
//
// This function is used only for matching keys, never for making HTTP requests.
func NormalizeFeedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Not a parseable URL — fall back to simple lowercasing.
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	// Treat http and https as equivalent — use https as canonical form.
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	// Strip trailing slash from the path for a canonical form.
	// A bare root path "/" is left intact.
	if len(u.Path) > 1 {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	u.Fragment = ""
	return u.String()
}

// BuildFeedToTitle returns a map from each podcast's feed URL to its
// lowercased podcast title, built from lib.Podcasts. Returns nil when lib is nil.
// Used for cross-provider episode filtering and logging.
func BuildFeedToTitle(lib *model.Library) map[string]string {
	if lib == nil {
		return nil
	}
	m := make(map[string]string, len(lib.Podcasts))
	for _, pod := range lib.Podcasts {
		if pod.FeedURL != "" {
			m[pod.FeedURL] = strings.ToLower(strings.TrimSpace(pod.Title))
		}
	}
	return m
}

// FilterEpisodesByPodcast returns the subset of episodes whose podcast title
// (looked up via feedToTitle) contains at least one of the filter strings
// (case-insensitive). If filters is empty, all episodes are returned unchanged.
func FilterEpisodesByPodcast(episodes []model.EpisodeState, feedToTitle map[string]string, filters []string) []model.EpisodeState {
	if len(filters) == 0 {
		return episodes
	}
	// Normalise filter patterns once.
	lower := make([]string, len(filters))
	for i, f := range filters {
		lower[i] = strings.ToLower(strings.TrimSpace(f))
	}

	var out []model.EpisodeState
	for _, ep := range episodes {
		title := feedToTitle[ep.FeedURL] // already lowercased by BuildFeedToTitle
		for _, f := range lower {
			if f != "" && strings.Contains(title, f) {
				out = append(out, ep)
				break
			}
		}
	}
	return out
}

// FilterPodcastsByTitle returns the subset of podcasts whose title contains at
// least one of the filter strings (case-insensitive). If filters is empty, all
// podcasts are returned unchanged.
func FilterPodcastsByTitle(podcasts []model.Podcast, filters []string) []model.Podcast {
	if len(filters) == 0 {
		return podcasts
	}
	lower := make([]string, len(filters))
	for i, f := range filters {
		lower[i] = strings.ToLower(strings.TrimSpace(f))
	}
	var out []model.Podcast
	for _, p := range podcasts {
		title := strings.ToLower(strings.TrimSpace(p.Title))
		for _, f := range lower {
			if f != "" && strings.Contains(title, f) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// FuzzyNormalizeTitle normalises an episode title for approximate cross-feed
// matching. It:
//   - lowercases
//   - strips season markers: "S01", "S1", "Season 1", etc.
//   - removes non-alphanumeric characters (hyphens, colons, periods, …)
//   - collapses whitespace
//
// This makes titles that differ only by season-marker conventions compare
// equal: "The Retrievals - Ep. 4" and "The Retrievals S01 - Ep. 4" both
// normalise to "the retrievals ep 4". Used when matching episodes across
// subscriber and public RSS feeds where publishers use inconsistent naming.
func FuzzyNormalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = fuzzySeasonRe.ReplaceAllString(s, " ")
	s = fuzzyApostropheRe.ReplaceAllString(s, "") // remove apostrophes (no gap)
	s = fuzzyNonAlnumRe.ReplaceAllString(s, " ")  // other punctuation → word gap
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// SkipReason returns the log status string when the destination's current play
// state already matches or exceeds the desired state — "already_played" or
// "already_ahead". Returns "" when a write should proceed.
//
// Played beats everything. For in-progress, the write is skipped when the
// destination is already played (ahead) or its position is at or beyond the
// desired position.
//
// This logic is shared by Overcast and Pocket Casts write paths and produces
// the same skip-reason labels for both, ensuring consistent --log-file output.
func SkipReason(desired model.PlayState, desiredPos time.Duration, current model.PlayState, currentPos time.Duration) string {
	switch desired {
	case model.PlayStatePlayed:
		if current == model.PlayStatePlayed {
			return "already_played"
		}
	case model.PlayStateInProgress:
		if current == model.PlayStatePlayed {
			return "already_played" // destination is ahead
		}
		if current == model.PlayStateInProgress && currentPos >= desiredPos {
			return "already_ahead"
		}
	}
	return ""
}

// FuzzyPodcastTitle normalises a podcast title for cross-library matching.
// It strips paid-tier suffixes ("Plus", "+", "Premium", etc.) via
// model.NormalizePlusTitle, then applies FuzzyNormalizeTitle (lowercase,
// strip season markers and punctuation, collapse whitespace).
//
// This two-step approach handles both "Pod Save America+" ↔ "Pod Save America"
// and "O'Brien" ↔ "OBrien" style differences across providers.
func FuzzyPodcastTitle(title string) string {
	norm := model.NormalizePlusTitle(title)
	if norm == "" {
		norm = title
	}
	return FuzzyNormalizeTitle(norm)
}

// TitleHasWordPrefix reports whether shorter is a word-aligned prefix of longer.
// Both strings should be the output of FuzzyPodcastTitle (lowercase,
// space-separated words, no punctuation).
//
// Word-aligned means that if shorter is strictly shorter than longer, the
// character in longer immediately after the prefix must be a space — preventing
// "pod save america" from matching "breaking news from pod save america" while
// still allowing "crooked city" to match "crooked city dixon il".
func TitleHasWordPrefix(longer, shorter string) bool {
	if !strings.HasPrefix(longer, shorter) {
		return false
	}
	if len(longer) == len(shorter) {
		return true
	}
	return longer[len(shorter)] == ' '
}
