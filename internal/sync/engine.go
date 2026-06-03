package sync

import (
	"context"
	"fmt"
	"strings"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// Result summarises what the engine did (or would do in dry-run mode).
type Result struct {
	SubscriptionsAdded      int
	EpisodesUpdated         int
	EpisodesSkipped         int
	PaywalledEpisodesIncluded int // Apple PSUB/PLUS episodes included for fuzzy title+date matching
	SkippedInternalPodcasts   int // Apple "internal://" feeds: no public RSS exists
	DryRun                  bool
}

func (r Result) String() string {
	prefix := ""
	if r.DryRun {
		prefix = "[dry-run] "
	}
	s := fmt.Sprintf("%ssubscriptions added: %d  episodes updated: %d  skipped: %d",
		prefix, r.SubscriptionsAdded, r.EpisodesUpdated, r.EpisodesSkipped)
	if r.SkippedInternalPodcasts > 0 {
		s += fmt.Sprintf("\nnote: %d podcast(s) were excluded — they use Apple-internal or platform-authenticated feed URLs\n"+
			"      (internal:// Apple-exclusive shows, NPR Plus membership feeds, etc.) that no other app can subscribe to.\n"+
			"      Search for these shows by name in Overcast to find their public feeds.",
			r.SkippedInternalPodcasts)
	}
	if r.PaywalledEpisodesIncluded > 0 {
		s += fmt.Sprintf("\nnote: %d Apple Podcasts Subscription (PSUB/PLUS) episode(s) included —\n"+
			"      these use Apple-proprietary GUIDs and DRM enclosure URLs, so feed-URL matching\n"+
			"      is skipped; they are matched by podcast title + pub date against the destination.",
			r.PaywalledEpisodesIncluded)
	}
	return s
}

// Engine reads from a source Provider and writes to a target Provider.
type Engine struct {
	src provider.Provider
	dst provider.Provider
}

func New(src, dst provider.Provider) *Engine {
	return &Engine{src: src, dst: dst}
}

// Run executes the migration according to opts.
func (e *Engine) Run(ctx context.Context, opts provider.WriteOptions) (*Result, error) {
	srcLib, err := e.src.GetLibrary(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: read from %s: %w", e.src.Name(), err)
	}

	var dstLib *model.Library
	dstCaps := e.dst.Capabilities()

	if dstCaps.ReadSubscriptions || dstCaps.ReadPlayState {
		dstLib, err = e.dst.GetLibrary(ctx)
		if err != nil {
			// Non-fatal: if we can't read the target, we can still write
			// using SourceWins strategy or subscription-only mode.
			fmt.Printf("sync: warning: could not read %s (%v); using source as authoritative\n",
				e.dst.Name(), err)
		}
	}

	merged := merge(srcLib, dstLib, opts)

	res := &Result{
		DryRun:                   opts.DryRun,
		PaywalledEpisodesIncluded: srcLib.PaywalledEpisodesIncluded,
		SkippedInternalPodcasts:  srcLib.SkippedInternalPodcasts,
	}
	// Only count subscriptions added when the destination can actually receive them.
	// Destinations like Apple Podcasts report WriteSubscriptions=false and have no
	// subscription write path — counting would produce a misleadingly large number.
	if dstCaps.WriteSubscriptions {
		if dstLib != nil {
			res.SubscriptionsAdded = len(merged.Podcasts) - len(dstLib.Podcasts)
			if res.SubscriptionsAdded < 0 {
				res.SubscriptionsAdded = 0
			}
		} else {
			res.SubscriptionsAdded = len(merged.Podcasts)
		}
	}

	if err := e.dst.SetLibrary(ctx, merged, opts); err != nil {
		return nil, fmt.Errorf("sync: write to %s: %w", e.dst.Name(), err)
	}

	return res, nil
}

// merge combines src and dst libraries according to the conflict strategy.
// dst may be nil (target was unreadable or is write-only).
func merge(src, dst *model.Library, opts provider.WriteOptions) *model.Library {
	out := &model.Library{
		SourceProvider: src.SourceProvider,
		ExportedAt:     src.ExportedAt,
	}

	// --- Subscriptions: union of both sides ---
	if !opts.OnlyPlayState {
		feedSeen := make(map[string]bool)
		for _, p := range src.Podcasts {
			if !feedSeen[p.FeedURL] {
				out.Podcasts = append(out.Podcasts, p)
				feedSeen[p.FeedURL] = true
			}
		}
		if dst != nil {
			for _, p := range dst.Podcasts {
				if !feedSeen[p.FeedURL] {
					out.Podcasts = append(out.Podcasts, p)
					feedSeen[p.FeedURL] = true
				}
			}
		}
	}

	// --- Episode states: merge by conflict strategy ---
	if !opts.OnlySubscriptions {
		dstIndex := buildEpisodeIndex(dst)

		// First pass: primary matching — GUID, feed URL + pub date, feed URL + title.
		var unmatched []model.EpisodeState
		for _, ep := range src.Episodes {
			key := episodeKey(ep)
			if existing, ok := dstIndex[key]; ok {
				out.Episodes = append(out.Episodes, resolveConflict(ep, existing, opts.ConflictStrategy))
				delete(dstIndex, key)
			} else {
				unmatched = append(unmatched, ep)
			}
		}

		// Second pass: cross-feed matching for episodes not resolved above.
		// Handles paid-tier feed variants (e.g. "Fresh Air Plus" ↔ "Fresh Air") where
		// the feed URL differs between apps but the podcast title normalises to the same
		// base. Both sides are normalised via NormalizePlusTitle before comparison.
		dstCrossIndex := buildCrossFeedIndex(dst)
		srcFeedToTitle := buildFeedToTitle(src)
		for _, ep := range unmatched {
			if !ep.PubDate.IsZero() && ep.FeedURL != "" && len(dstCrossIndex) > 0 {
				if podTitle := srcFeedToTitle[ep.FeedURL]; podTitle != "" {
					xKey := "xfeed:" + model.NormalizePlusTitle(podTitle) + "|" + ep.PubDate.UTC().Format("2006-01-02T15:04:05")
					if existing, ok := dstCrossIndex[xKey]; ok {
						// Guard: only match if the dst episode wasn't already claimed in
						// the first pass (possible when primary and cross-feed keys collide).
						existingKey := episodeKey(existing)
						if _, stillAvail := dstIndex[existingKey]; stillAvail {
							out.Episodes = append(out.Episodes, resolveConflictCrossFeed(ep, existing, opts.ConflictStrategy))
							delete(dstIndex, existingKey)
							delete(dstCrossIndex, xKey)
							continue
						}
					}
				}
			}
			out.Episodes = append(out.Episodes, ep)
		}

		// Destination-only episodes (no match in src) are included so the writer
		// can perform conflict resolution, but they are flagged so writers can
		// distinguish them from source-originated episodes and skip re-processing
		// their own data (e.g. the Apple writer ignoring Apple-sourced episodes).
		for _, ep := range dstIndex {
			ep.FromDestination = true
			out.Episodes = append(out.Episodes, ep)
		}
	}

	return out
}

// buildFeedToTitle returns a map from each podcast's feed URL to its lowercased
// podcast title, built from lib.Podcasts. Used for cross-feed episode matching.
func buildFeedToTitle(lib *model.Library) map[string]string {
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

// buildCrossFeedIndex indexes lib's episodes by Plus-normalised podcast title +
// pub date (format "xfeed:<normTitle>|<UTC date>"). Intended as a secondary index
// used only when primary feed-URL-based matching fails, so that episodes from
// paid-tier variants (e.g. "Fresh Air Plus") can match their public counterparts
// ("Fresh Air") across providers.
func buildCrossFeedIndex(lib *model.Library) map[string]model.EpisodeState {
	if lib == nil {
		return nil
	}
	feedToTitle := buildFeedToTitle(lib)
	idx := make(map[string]model.EpisodeState)
	for _, ep := range lib.Episodes {
		if ep.PubDate.IsZero() || ep.FeedURL == "" {
			continue
		}
		podTitle := feedToTitle[ep.FeedURL]
		if podTitle == "" {
			continue
		}
		key := "xfeed:" + model.NormalizePlusTitle(podTitle) + "|" + ep.PubDate.UTC().Format("2006-01-02T15:04:05")
		if _, exists := idx[key]; !exists {
			idx[key] = ep
		}
	}
	return idx
}

func buildEpisodeIndex(lib *model.Library) map[string]model.EpisodeState {
	idx := make(map[string]model.EpisodeState)
	if lib == nil {
		return idx
	}
	for _, ep := range lib.Episodes {
		idx[episodeKey(ep)] = ep
	}
	return idx
}

// episodeKey returns a canonical match string for cross-provider deduplication.
// Priority: GUID → FeedURL+PubDate → FeedURL+NormalizedTitle.
func episodeKey(ep model.EpisodeState) string {
	if ep.GUID != "" {
		return "guid:" + ep.GUID
	}
	if !ep.PubDate.IsZero() && ep.FeedURL != "" {
		return "feeddate:" + ep.FeedURL + "|" + ep.PubDate.UTC().Format("2006-01-02T15:04:05")
	}
	return "feedtitle:" + ep.FeedURL + "|" + normalizeTitle(ep.Title)
}

func normalizeTitle(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// resolveConflictCrossFeed is like resolveConflict but always emits the
// destination episode's identity fields (GUID, FeedURL, Title, PodcastTitle,
// PubDate, Duration) with only the winning play state applied. Used for
// cross-feed matches where source and destination identify the same episode
// under different feed URLs or GUIDs — for example:
//
//   - Public feed ("Fresh Air") vs paid-tier feed ("Fresh Air Plus")
//   - Apple-proprietary GUID/DRM enclosure (PSUB/PLUS) vs the destination's
//     RSS-native GUID for the same episode via a private member feed
//
// Preserving destination identifiers ensures downstream writers (e.g. the
// Overcast writer, which keys its index on the destination's feed URL) can
// locate the episode after the merge.
func resolveConflictCrossFeed(src, dst model.EpisodeState, strategy provider.ConflictStrategy) model.EpisodeState {
	winner := resolveConflict(src, dst, strategy)
	// Always use destination identifiers; apply only the play state from the winner.
	out := dst
	out.PlayState = winner.PlayState
	out.PlayPosition = winner.PlayPosition
	return out
}

func resolveConflict(src, dst model.EpisodeState, strategy provider.ConflictStrategy) model.EpisodeState {
	switch strategy {
	case provider.SourceWins:
		return src
	case provider.TargetWins:
		return dst
	default: // FurthestWins
		return furthestWins(src, dst)
	}
}

// furthestWins returns whichever episode has the most listening progress.
// Played always beats in-progress, which beats unplayed.
func furthestWins(a, b model.EpisodeState) model.EpisodeState {
	if a.PlayState == model.PlayStatePlayed || b.PlayState == model.PlayStatePlayed {
		if a.PlayState == model.PlayStatePlayed {
			return a
		}
		return b
	}
	if a.PlayPosition >= b.PlayPosition {
		return a
	}
	return b
}
