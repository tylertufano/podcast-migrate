package model

import (
	"net/url"
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

	// IsPrivate is true when the feed is a subscriber or private edition that
	// should not be replaced with a public iTunes canonical URL at destinations.
	// Destination providers skip iTunes lookup for private feeds and subscribe
	// using FeedURL directly.
	//
	// Set by source readers when:
	//   - the provider explicitly marks the feed as private (PC: APIPodcast.IsPrivate)
	//   - title markers indicate a subscriber edition (NormalizePlusTitle strips them)
	//   - the feed URL matches a known subscriber platform domain
	//   - the KVS feed URL differs from the iTunes canonical URL (Apple source only)
	IsPrivate bool
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

// IsSubscriberFeed reports whether a podcast is a subscriber or private edition
// based on its title and feed URL. It is used by source readers (Overcast, PC)
// that lack an explicit private-feed flag, and by the KVS writer to distinguish
// existing public subscriptions from subscriber ones during dedup.
//
// Three signals are checked:
//
//  1. Title markers: NormalizePlusTitle strips subscriber suffixes (" Plus",
//     "- Subscriber Feed", etc.). If the stripped title differs from the
//     lowercased-and-trimmed original, the title carried a subscriber marker.
//
//  2. URL scheme: Apple-internal feeds use the "internal://" scheme. These
//     are proprietary Apple Originals with no public RSS equivalent.
//
//  3. Known subscriber platform domains: feed hosting services exclusively
//     used for paid subscriber feeds (supercast.com, memberful.com, etc.).
//
// Note: the Apple KVS reader applies an additional signal (KVS feed URL ≠
// iTunes canonical URL) that requires the iTunes lookup result and is
// therefore handled separately in kvsreader.go rather than here.
func IsSubscriberFeed(title, feedURL string) bool {
	// Signal 1: title has subscriber/paid-tier markers.
	if strings.ToLower(strings.TrimSpace(title)) != NormalizePlusTitle(title) {
		return true
	}
	// Signals 2 & 3: feed URL scheme or domain.
	if u, err := url.Parse(feedURL); err == nil {
		if u.Scheme == "internal" {
			return true
		}
		host := u.Hostname()
		for _, h := range []string{
			"supercast.com",
			"memberful.com",
			"supporting.cast.st",
			"patreon.com",
		} {
			if host == h || strings.HasSuffix(host, "."+h) {
				return true
			}
		}
	}
	return false
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
