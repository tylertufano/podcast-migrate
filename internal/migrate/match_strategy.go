package migrate

import "fmt"

// MatchStrategy identifies a single step in the episode-lookup cascade used by
// providers when matching source episodes to their destination equivalents.
//
// # Canonical priority order
//
// Strategies are listed highest-confidence-first. A provider should try each
// strategy in order and stop at the first hit.
//
//  1. MatchByGUID      — episode's globally-unique identifier.
//  2. MatchByFeedDate  — same feed URL + exact publication date.
//  3. MatchByFeedTitle — same feed URL + fuzzy-normalised episode title.
//  4. MatchByTitleDate — fuzzy episode title + calendar day (cross-feed, no show anchor).
//  5. MatchByPodDate   — podcast title + exact publication date (cross-feed).
//  6. MatchByPodTitle  — podcast title + fuzzy episode title (cross-feed).
//
// # Provider coverage
//
// Not all providers implement all strategies. Omissions are intentional; each
// provider's lookup function documents which strategies it uses and why any
// canonical strategies are absent.
//
//   - Apple Podcasts KVS: GUID, feeddate, feedtitle (SQLite exact-case, see note below).
//     Cross-feed strategies (poddate, podtitle) are omitted because private-feed
//     episodes are always tied to a specific feed URL in the SQLite database;
//     catalog episodes are handled separately by the web-API writer.
//     Note: the SQLite feedtitle match uses case-insensitive string equality rather
//     than FuzzyNormalizeTitle, so season-marker variants ("Ep. 4" vs "S01 Ep. 4")
//     may not match — a known accuracy gap vs the other providers.
//
//   - Apple Podcasts catalog: feeddate, feedtitle, poddate, podtitle.
//     GUID is not used because the catalog API does not expose episode GUIDs.
//
//   - Overcast: feeddate, feedtitle (opt-out via StrictFeedMatch).
//     Cross-feed strategies are absent because Overcast episode IDs are numeric
//     and scraped from podcast-page HTML; there is no API to resolve an episode
//     by podcast title, making cross-feed matching impractical.
//
//   - Pocket Casts: feeddate, feedtitle, titledate.
//     titledate (fuzzy episode title + calendar day, no show anchor) is a
//     lightweight alternative to poddate/podtitle that avoids the need to
//     resolve a podcast title → UUID mapping. It is lower-precision than
//     poddate because it anchors on the episode title rather than the show,
//     but day-level date granularity prevents most false positives.
//
//   - Sync engine episodeKey: GUID, feeddate, feedtitle.
//     Used for source-vs-destination deduplication during library merge, not
//     for provider write-path matching. Cross-feed strategies are not needed
//     here because the merge operates over an already-resolved unified library.
type MatchStrategy int

const (
	// MatchByGUID matches on the episode's RSS GUID or provider-assigned identifier.
	// Highest confidence; requires the destination to store episode GUIDs.
	// Only Apple Podcasts SQLite exposes GUIDs in a queryable form.
	MatchByGUID MatchStrategy = iota

	// MatchByFeedDate matches on the normalised feed URL and the exact publication
	// date-time. Reliable when both source and destination attribute the episode
	// to the same feed and agree on the publication timestamp.
	MatchByFeedDate

	// MatchByFeedTitle matches on the normalised feed URL and the
	// fuzzy-normalised episode title (via FuzzyNormalizeTitle). Catches episodes
	// whose timestamps differ between apps (e.g. early-access vs public release)
	// but whose titles are stable. Overcast can opt out via StrictFeedMatch.
	MatchByFeedTitle

	// MatchByTitleDate matches on the fuzzy-normalised episode title and the
	// calendar day (YYYY-MM-DD, UTC) without anchoring to a feed URL or podcast
	// title. Used by Pocket Casts as a cross-feed fallback when a podcast network
	// cross-posts the same episode to multiple feeds and Apple attributes it to a
	// different show than PC does. Day-level precision tolerates timezone
	// differences between the two apps' timestamp representations.
	MatchByTitleDate

	// MatchByPodDate matches on the lowercased podcast title and the exact
	// publication date-time without anchoring to a specific feed URL. Used by the
	// Apple catalog writer as a cross-feed fallback for episodes that have migrated
	// between feed URLs (e.g. after a hosting provider change).
	MatchByPodDate

	// MatchByPodTitle matches on the lowercased podcast title and the
	// fuzzy-normalised episode title without anchoring to a specific feed URL.
	// Lowest-confidence strategy; callers must apply a date-tolerance guard after
	// the match to reject false positives. Used only by the Apple catalog writer.
	MatchByPodTitle
)

// String returns the canonical log/key prefix for the strategy.
func (s MatchStrategy) String() string {
	switch s {
	case MatchByGUID:
		return "guid"
	case MatchByFeedDate:
		return "feeddate"
	case MatchByFeedTitle:
		return "feedtitle"
	case MatchByTitleDate:
		return "titledate"
	case MatchByPodDate:
		return "poddate"
	case MatchByPodTitle:
		return "podtitle"
	default:
		return fmt.Sprintf("MatchStrategy(%d)", int(s))
	}
}
