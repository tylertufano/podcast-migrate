package model

import "time"

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

// Library is the canonical intermediate representation shared by all providers.
type Library struct {
	Podcasts   []Podcast
	Episodes   []EpisodeState
	ExportedAt     time.Time
	SourceProvider string

	// SkippedPaywalledEpisodes is the count of episodes excluded because they
	// are behind an Apple-proprietary paywall (PSUB/PLUS) — their GUIDs are
	// Apple-internal and their enclosure URLs are Apple DRM streams.
	SkippedPaywalledEpisodes int

	// SkippedInternalPodcasts is the count of subscriptions excluded because
	// their feed URL uses the Apple-internal "internal://" scheme, meaning
	// there is no public RSS feed for another app to subscribe to.
	SkippedInternalPodcasts int
}
