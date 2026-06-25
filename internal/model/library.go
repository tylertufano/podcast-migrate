package model

import (
	"strings"
	"time"
)

// PlayState represents the listening status of an episode.
type PlayState int

const (
	PlayStateUnplayed   PlayState = 0
	PlayStatePlayed     PlayState = 1
	PlayStateInProgress PlayState = 2
)

// Podcast is a subscribed show identified by its feed URL.
type Podcast struct {
	FeedURL    string
	Title      string
	Author     string
	ImageURL   string
	OvercastID string // Overcast's internal podcast ID (from OPML overcastId attribute)
	ITunesID   string // Apple iTunes Store podcast ID (used for direct page lookup at destinations)
}

// EpisodeState captures everything we migrate about a single episode.
type EpisodeState struct {
	// Identity — used for cross-provider matching in priority order:
	//   1. GUID (RSS <guid>)
	//   2. FeedURL + PubDate
	//   3. FeedURL + NormalizedTitle
	GUID    string
	FeedURL string // RSS feed URL of the parent podcast
	Title   string
	PubDate time.Time

	Duration     time.Duration
	PlayState    PlayState
	PlayPosition time.Duration // 0 means not started or unknown
	LastPlayed   time.Time

	// FromDestination is true for episodes that originated exclusively from
	// the destination provider (i.e. they had no matching episode in the source
	// library). Writers use this to skip episodes that came from themselves —
	// there is no source state to apply and re-processing them produces noise.
	FromDestination bool
}

// NormalizePlusTitle strips paid-tier and subscriber-feed suffixes from a podcast
// title and lowercases the result. This enables cross-feed matching when one app
// has a public feed and another has a paid or private equivalent.
//
// Stripped infixes/suffixes (case-insensitive):
//
// Subscriber/private feed indicators (index-based so dynamic trailing content
// such as "🔓 for <name>" or "🔓 for user@example.com" is also stripped):
//   - " - Subscriber Feed …"  — NYT pattern ("The Daily - Subscriber Feed (🔓 for you@…)")
//   - " - Member Feed …"
//   - " - Private Feed …"
//   - " - Premium Feed …"
//   - " (🔓)"                 — standalone lock emoji (exact suffix)
//
// Plus-tier indicators (podcast networks append these to paid feed titles):
//   - " Plus"  — NPR Plus and similar (e.g. "Fresh Air Plus" → "fresh air")
//   - " +"     — space + plus symbol  (e.g. "Planet Money +" → "planet money")
//   - "+"      — trailing plus symbol (e.g. "Planet Money+" → "planet money")
//
// If the title has no known suffix it is still lowercased and trimmed, so the
// return value is always safe to use as a normalised matching key.
func NormalizePlusTitle(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))

	// Strip subscriber/private feed decorations using index-based search so that
	// dynamic trailing content (e.g. "🔓 for user@example.com)") is also covered.
	// The lock-only exact suffix " (🔓)" is handled separately below.
	stripped := false
	for _, infix := range []string{
		" - subscriber feed",
		" - member feed",
		" - private feed",
		" - premium feed",
	} {
		if idx := strings.Index(t, infix); idx != -1 {
			t = strings.TrimSpace(t[:idx])
			stripped = true
			break
		}
	}

	// Standalone lock emoji suffix (only when no subscriber-feed infix was found).
	if !stripped && strings.HasSuffix(t, " (🔓)") {
		t = strings.TrimSpace(strings.TrimSuffix(t, " (🔓)"))
	}

	// Strip Plus-tier suffixes.
	for _, suffix := range []string{" plus", " +", "+"} {
		if strings.HasSuffix(t, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(t, suffix))
		}
	}
	return t
}

// Library is the canonical intermediate representation shared by all providers.
type Library struct {
	Podcasts   []Podcast
	Episodes   []EpisodeState
	ExportedAt     time.Time
	SourceProvider string

	// PaywalledEpisodesIncluded is the count of PSUB/PLUS episodes (Apple
	// Podcasts Subscriptions) that were included in the episode list for fuzzy
	// matching. These have Apple-proprietary GUIDs and DRM enclosure URLs, so
	// feed-URL-based matching will not work for them; the engine will attempt
	// to match them via podcast title + pub date against the destination.
	PaywalledEpisodesIncluded int

	// SkippedInternalPodcasts is the count of subscriptions excluded because
	// their feed URL uses the Apple-internal "internal://" scheme, meaning
	// there is no public RSS feed for another app to subscribe to.
	SkippedInternalPodcasts int
}
