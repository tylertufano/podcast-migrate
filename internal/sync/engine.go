package sync

import (
	"context"
	"fmt"
	"strings"

	"github.com/tyler/podcast-migrate/internal/migrate"
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

	// Remap subscriber feed URLs to their analog destination feed URLs before
	// merging. This lets users pre-subscribe to private feeds on the target
	// (e.g. an Overcast or Pocket Casts exclusive subscriber feed) and have
	// their Apple Podcasts play state migrate to those feeds rather than to the
	// public equivalent. See provider.WriteOptions.FeedMap for details.
	if len(opts.FeedMap) > 0 {
		srcLib = applyFeedMap(srcLib, opts.FeedMap)
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
	//
	// Count only source podcasts that:
	//   1. Match the active podcast filter (if any) — so --podcast "xyz" doesn't
	//      report all N source podcasts as new subscriptions.
	//   2. Are not already present at the destination (by feed URL).
	if dstCaps.WriteSubscriptions {
		dstFeeds := make(map[string]bool)
		if dstLib != nil {
			for _, p := range dstLib.Podcasts {
				if p.FeedURL != "" {
					dstFeeds[p.FeedURL] = true
				}
			}
		}
		lowerFilters := make([]string, len(opts.PodcastFilter))
		for i, f := range opts.PodcastFilter {
			lowerFilters[i] = strings.ToLower(strings.TrimSpace(f))
		}
		for _, p := range srcLib.Podcasts {
			if p.FeedURL == "" || dstFeeds[p.FeedURL] {
				continue
			}
			if len(lowerFilters) > 0 {
				title := strings.ToLower(strings.TrimSpace(p.Title))
				matched := false
				for _, f := range lowerFilters {
					if f != "" && strings.Contains(title, f) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			res.SubscriptionsAdded++
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
		//
		// Try the exact UTC pub date first, then ±1 day. Off-day matches require
		// fuzzy title agreement to prevent false positives (a subscriber-exclusive
		// episode on day N must not silently match a different public episode on day N±1).
		// When multiple destination episodes share the same podcast+date bucket (e.g. a
		// batch release), fuzzy title matching selects the closest match.
		dstCrossIndex := buildCrossFeedIndex(dst)
		srcFeedToTitle := buildFeedToTitle(src)
		for _, ep := range unmatched {
			if !ep.PubDate.IsZero() && ep.FeedURL != "" && len(dstCrossIndex) > 0 {
				if podTitle := srcFeedToTitle[ep.FeedURL]; podTitle != "" {
					normPodTitle := model.NormalizePlusTitle(podTitle)
					matched := false
					for _, dayOffset := range []int{0, -1, 1} {
						offsetDate := ep.PubDate.UTC().AddDate(0, 0, dayOffset).Format("2006-01-02")
						xKey := "xfeed:" + normPodTitle + "|" + offsetDate
						candidates, ok := dstCrossIndex[xKey]
						if !ok {
							continue
						}
						requireTitle := dayOffset != 0
						existing, found := pickBestCrossFeedCandidate(ep, candidates, dstIndex, requireTitle)
						if !found {
							continue
						}
						existingKey := episodeKey(existing)
						out.Episodes = append(out.Episodes, resolveConflictCrossFeed(ep, existing, opts.ConflictStrategy))
						delete(dstIndex, existingKey)
						removeCrossFeedEntry(dstCrossIndex, xKey, existingKey)
						matched = true
						break
					}
					if matched {
						continue
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

// applyFeedMap returns a shallow copy of lib with feed URLs remapped according
// to feedMap. Both Podcasts and Episodes are remapped so that all downstream
// matching (feeddate, feedtitle, cross-feed title+date) operates against the
// destination analog feed rather than the original subscriber feed URL.
//
// feedMap keys are normalised via NormalizeFeedURL before comparison, so
// http/https and trailing-slash differences are treated as equivalent.
// The library itself is not mutated; new Podcasts and Episodes slices are
// allocated only when at least one entry is remapped.
func applyFeedMap(lib *model.Library, feedMap map[string]string) *model.Library {
	if lib == nil || len(feedMap) == 0 {
		return lib
	}
	// Build a normalised key → dst lookup so lookups are URL-canonical.
	normMap := make(map[string]string, len(feedMap))
	for src, dst := range feedMap {
		normMap[migrate.NormalizeFeedURL(src)] = dst
	}

	out := *lib // shallow struct copy — slices are replaced below as needed

	// Remap podcast feed URLs.
	podMapped := false
	for _, pod := range lib.Podcasts {
		if _, ok := normMap[migrate.NormalizeFeedURL(pod.FeedURL)]; ok {
			podMapped = true
			break
		}
	}
	if podMapped {
		out.Podcasts = make([]model.Podcast, len(lib.Podcasts))
		copy(out.Podcasts, lib.Podcasts)
		for i, pod := range out.Podcasts {
			if mapped, ok := normMap[migrate.NormalizeFeedURL(pod.FeedURL)]; ok {
				out.Podcasts[i].FeedURL = mapped
			}
		}
	}

	// Remap episode feed URLs.
	epMapped := false
	for _, ep := range lib.Episodes {
		if _, ok := normMap[migrate.NormalizeFeedURL(ep.FeedURL)]; ok {
			epMapped = true
			break
		}
	}
	if epMapped {
		out.Episodes = make([]model.EpisodeState, len(lib.Episodes))
		copy(out.Episodes, lib.Episodes)
		for i, ep := range out.Episodes {
			if mapped, ok := normMap[migrate.NormalizeFeedURL(ep.FeedURL)]; ok {
				out.Episodes[i].FeedURL = mapped
			}
		}
	}

	return &out
}

// buildFeedToTitle returns a map from each podcast's feed URL to its lowercased
// podcast title. Delegates to migrate.BuildFeedToTitle for consistent behaviour
// across providers.
func buildFeedToTitle(lib *model.Library) map[string]string {
	return migrate.BuildFeedToTitle(lib)
}

// buildCrossFeedIndex indexes lib's episodes by Plus-normalised podcast title +
// pub date (format "xfeed:<normTitle>|<YYYY-MM-DD UTC date>"). Intended as a
// secondary index used only when primary feed-URL-based matching fails, so that
// episodes from paid-tier variants (e.g. "Fresh Air Plus") or private/member
// feeds can match their public counterparts across providers.
//
// Date-only precision is intentional: private/member feeds often release
// episodes several hours before the public RSS (early-access window), so the
// exact pub-date timestamps differ even though it is the same episode. Using
// the UTC calendar date as the key tolerates those timing differences while
// keeping the podcast title in the key to avoid false-positive matches between
// unrelated shows that happen to publish on the same day.
//
// Each key maps to a slice of candidates because a podcast may release multiple
// episodes on the same UTC day (batch releases). Callers use
// pickBestCrossFeedCandidate to select among them.
func buildCrossFeedIndex(lib *model.Library) map[string][]model.EpisodeState {
	if lib == nil {
		return nil
	}
	feedToTitle := buildFeedToTitle(lib)
	idx := make(map[string][]model.EpisodeState)
	for _, ep := range lib.Episodes {
		if ep.PubDate.IsZero() || ep.FeedURL == "" {
			continue
		}
		podTitle := feedToTitle[ep.FeedURL]
		if podTitle == "" {
			continue
		}
		key := "xfeed:" + model.NormalizePlusTitle(podTitle) + "|" + ep.PubDate.UTC().Format("2006-01-02")
		idx[key] = append(idx[key], ep)
	}
	return idx
}

// pickBestCrossFeedCandidate selects the best destination episode from a bucket
// of candidates sharing the same cross-feed key (same normalised podcast title
// + UTC pub date). Candidates already claimed by primary matching are skipped.
//
// When requireTitle is true (used for ±1-day date fallback) the
// fuzzy-normalised source title must match at least one candidate's title;
// this prevents subscriber-exclusive episodes from being falsely matched to a
// different public episode published on an adjacent day.
//
// When requireTitle is false, an exact fuzzy-title match is preferred but the
// first available candidate is accepted as a fallback. Returns (episode, true)
// on success or (zero, false) when no suitable candidate is available.
func pickBestCrossFeedCandidate(src model.EpisodeState, candidates []model.EpisodeState, dstIndex map[string]model.EpisodeState, requireTitle bool) (model.EpisodeState, bool) {
	srcFuzzy := migrate.FuzzyNormalizeTitle(src.Title)
	var firstAvail *model.EpisodeState
	for i := range candidates {
		c := candidates[i]
		if _, ok := dstIndex[episodeKey(c)]; !ok {
			continue // already claimed by an earlier match
		}
		if migrate.FuzzyNormalizeTitle(c.Title) == srcFuzzy {
			return c, true // exact fuzzy match — best result
		}
		if firstAvail == nil {
			firstAvail = &candidates[i]
		}
	}
	if requireTitle {
		return model.EpisodeState{}, false
	}
	if firstAvail != nil {
		return *firstAvail, true
	}
	return model.EpisodeState{}, false
}

// removeCrossFeedEntry removes the episode identified by epKey from the
// candidate slice stored at idx[dateKey], shrinking it in-place. No-op if the
// key or episode is not present.
func removeCrossFeedEntry(idx map[string][]model.EpisodeState, dateKey, epKey string) {
	candidates := idx[dateKey]
	for i, c := range candidates {
		if episodeKey(c) == epKey {
			idx[dateKey] = append(candidates[:i], candidates[i+1:]...)
			return
		}
	}
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
