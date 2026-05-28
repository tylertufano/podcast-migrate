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
	SkippedPaywalledEpisodes int // Apple PSUB/PLUS episodes: wrong GUIDs, DRM streams
	SkippedInternalPodcasts  int // Apple "internal://" feeds: no public RSS exists
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
		s += fmt.Sprintf("\nnote: %d podcast(s) were excluded — they use Apple-internal feed URLs (no public RSS feed exists).",
			r.SkippedInternalPodcasts)
	}
	if r.SkippedPaywalledEpisodes > 0 {
		s += fmt.Sprintf("\nnote: %d Apple Podcasts Subscription (PSUB/PLUS) episode states were excluded —\n"+
			"      these use Apple-proprietary GUIDs and DRM streams no other app can match or play.",
			r.SkippedPaywalledEpisodes)
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
		SkippedPaywalledEpisodes: srcLib.SkippedPaywalledEpisodes,
		SkippedInternalPodcasts:  srcLib.SkippedInternalPodcasts,
	}
	if dstLib != nil {
		res.SubscriptionsAdded = len(merged.Podcasts) - len(dstLib.Podcasts)
		if res.SubscriptionsAdded < 0 {
			res.SubscriptionsAdded = 0
		}
	} else {
		res.SubscriptionsAdded = len(merged.Podcasts)
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
		for _, ep := range src.Episodes {
			key := episodeKey(ep)
			if existing, ok := dstIndex[key]; ok {
				resolved := resolveConflict(ep, existing, opts.ConflictStrategy)
				out.Episodes = append(out.Episodes, resolved)
				delete(dstIndex, key)
			} else {
				out.Episodes = append(out.Episodes, ep)
			}
		}
		// Any dst episodes not in src are kept as-is.
		for _, ep := range dstIndex {
			out.Episodes = append(out.Episodes, ep)
		}
	}

	return out
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
