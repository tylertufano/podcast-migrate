package pocketcasts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/httputil"
	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/opml"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// DefaultRequestDelay is the pause between consecutive Pocket Casts API
// requests when WriteOptions.RequestDelay is not set.
const DefaultRequestDelay = 1 * time.Second

// maxPhaseBPagesPerPodcast caps the number of episode-list pages fetched per
// podcast during Phase B (per-podcast episode lookup). One page holds ~50
// episodes, so 20 pages covers ~1000 episodes — sufficient for almost any
// real podcast back-catalogue.
const maxPhaseBPagesPerPodcast = 20

// Provider implements provider.Provider for Pocket Casts.
//
// Reading:  fetches podcast subscriptions, in-progress episodes, and play
//           history from the Pocket Casts web API.
//
// Writing:  writes episode play state via the same API.
//
// All operations require email and password credentials for the Pocket Casts
// account. The unofficial web API is used — see web.go for details.
type Provider struct {
	email    string
	password string

	// IncludeUnsubscribed, when true, also exports play state for podcasts
	// that appear in the PC sync history but are no longer subscribed to. The
	// feed URL is recovered via the PC CDN or the iTunes Search API. Off by
	// default — unsubscribed podcasts are intentionally excluded because the
	// user may have left them, and including them could clutter a migration.
	IncludeUnsubscribed bool

	// skippedOPMLPath is where the skipped-private-feeds OPML is written after
	// a subscription run. Feeds with IsPrivate=true that could not be resolved
	// via add_feed_url (e.g. auth-required subscriber URLs) are collected here.
	// Defaults to "skipped-private-feeds.opml" in the working directory when empty.
	skippedOPMLPath string
}

// NewProvider returns a Pocket Casts provider with the given credentials.
func NewProvider(email, password string) *Provider {
	return &Provider{email: email, password: password}
}

// SetSkippedOPMLPath configures the path where an OPML of skipped private feeds
// is written after a subscription run.
func (p *Provider) SetSkippedOPMLPath(path string) { p.skippedOPMLPath = path }

// writeSkippedOPML writes an OPML file containing feeds that could not be
// subscribed automatically (private/subscriber feeds whose URL could not be
// resolved via the Pocket Casts add_feed_url API).
func (p *Provider) writeSkippedOPML(pods []model.Podcast, dryRun bool) {
	outPath := p.skippedOPMLPath
	if outPath == "" {
		outPath = "skipped-private-feeds.opml"
	}
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("\npocketcasts: %d podcast(s) could not be subscribed automatically:\n", len(pods))
	for _, pod := range pods {
		fmt.Printf("  • %s (%s)\n", pod.Title, pod.FeedURL)
	}
	if dryRun {
		fmt.Printf("%sWould write skipped-feeds OPML to %s\n", prefix, outPath)
		fmt.Printf("  To subscribe: Pocket Casts → Add Podcast → Add via podcast URL\n")
		return
	}
	lib := &model.Library{Podcasts: pods}
	w := &opml.Writer{}
	if err := w.Write(lib, outPath); err != nil {
		fmt.Printf("pocketcasts: warning: could not write skipped-feeds OPML: %v\n", err)
		return
	}
	fmt.Printf("pocketcasts: skipped-feeds OPML written to %s\n", outPath)
	fmt.Printf("  To subscribe: Pocket Casts → Add Podcast → Add via podcast URL\n")
}

func (p *Provider) Name() string { return "Pocket Casts" }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ReadSubscriptions:  true,
		ReadPlayState:      true,
		WriteSubscriptions: true,
		WritePlayState:     true,
	}
}

// GetLibrary fetches the authenticated user's Pocket Casts library.
//
// Subscriptions: all subscribed podcasts with their RSS feed URLs.
// Play state: complete — uses /user/sync/update (the same protobuf endpoint
// the mobile apps use) to identify every podcast with play activity, then
// fetches full episode metadata from /user/podcast/episodes for each of those
// podcasts. Falls back to /user/in_progress + /user/history if the sync
// endpoint is unavailable.
func (p *Provider) GetLibrary(ctx context.Context) (*model.Library, error) {
	fmt.Printf("pocketcasts: authenticating as %s...\n", p.email)
	client, err := Login(ctx, p.email, p.password)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts: authentication failed: %w", err)
	}

	fmt.Printf("pocketcasts: fetching subscribed podcasts...\n")
	apiPods, err := FetchSubscribedPodcasts(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts: fetch subscriptions: %w", err)
	}

	// Batch-resolve feed URLs for any podcast the subscription list returned
	// without one. /user/podcast/list omits URLs for private/subscriber feeds
	// (isPrivate:true) and some public feeds (webFeed:false). The export
	// endpoint uses the same URL the user originally subscribed with, so it
	// returns subscriber URLs (e.g. Slate Plus, NPR Plus) as well as the
	// public feed for webFeed:false podcasts.
	var missingURLUUIDs []string
	for _, ap := range apiPods {
		if ap.UUID != "" && ap.URL == "" {
			missingURLUUIDs = append(missingURLUUIDs, ap.UUID)
		}
	}
	exportedURLs := make(map[string]string)
	if len(missingURLUUIDs) > 0 {
		fmt.Printf("pocketcasts: resolving feed URLs for %d podcast(s) via export service...\n", len(missingURLUUIDs))
		var exportErr error
		exportedURLs, exportErr = FetchExportFeedURLs(ctx, client, missingURLUUIDs)
		if exportErr != nil {
			fmt.Printf("pocketcasts: warning: export-feed-urls failed (%v) — affected podcasts will be skipped\n", exportErr)
		}
	}

	podUUIDToFeedURL := make(map[string]string, len(apiPods))
	var podcasts []model.Podcast
	for _, ap := range apiPods {
		if ap.UUID == "" {
			continue
		}
		feedURL := ap.URL
		if feedURL == "" {
			feedURL = exportedURLs[ap.UUID]
		}
		if feedURL == "" {
			fmt.Printf("pocketcasts: warning: no feed URL found for %q — skipping\n", ap.Title)
			continue
		}
		podUUIDToFeedURL[ap.UUID] = feedURL
		podcasts = append(podcasts, model.Podcast{
			FeedURL:   feedURL,
			Title:     ap.Title,
			Author:    ap.Author,
			IsPrivate: ap.IsPrivate || model.IsSubscriberFeed(ap.Title, feedURL),
		})
	}
	fmt.Printf("pocketcasts: %d subscribed podcast(s)\n", len(podcasts))

	// seenUUIDs prevents duplicates across all episode sources.
	seenUUIDs := make(map[string]bool)
	var episodes []model.EpisodeState

	// Primary path: use /user/sync/update to get the complete play-state index,
	// then fetch full episode metadata only for the podcasts that have play activity.
	// lastModified=1 is used (not 0) because PC treats lastModified=0 as "initial
	// setup sync" and returns only ~2500 recent entries; lastModified=1 triggers
	// the full incremental path and returns all stored play history.
	fmt.Printf("pocketcasts: fetching full sync state...\n")
	syncEps, _, syncErr := FetchSyncUpdate(ctx, client, 1)
	if syncErr != nil {
		fmt.Printf("pocketcasts: warning: sync/update failed (%v) — falling back to in_progress + history\n", syncErr)
	}

	if syncErr == nil {
		// Collect unique podcast UUIDs that have played or in-progress episodes.
		activePodcasts := make(map[string]bool)
		for _, se := range syncEps {
			if se.PlayingStatus == PlayingPlayed || se.PlayingStatus == PlayingInProgress {
				activePodcasts[se.PodcastUUID] = true
			}
		}
		fmt.Printf("pocketcasts: sync/update: %d episode(s) with play state across %d podcast(s)\n",
			len(syncEps), len(activePodcasts))

		// Build a UUID → SyncEpisodeState lookup for play-state overlay.
		syncByUUID := make(map[string]SyncEpisodeState, len(syncEps))
		for _, se := range syncEps {
			syncByUUID[se.EpisodeUUID] = se
		}

		// Fetch CDN episode metadata (title, pub date, UUID) for each active
		// podcast. /user/podcast/episodes does not return titles; the CDN endpoint
		// does. We overlay play state from the sync map onto each CDN episode.
		fetched := 0
		skippedUnsubscribed := 0
		for podUUID := range activePodcasts {
			feedURL := podUUIDToFeedURL[podUUID]
			if feedURL == "" {
				// Podcast is in sync state but not in subscription list (or
				// subscription has no URL). Only attempt recovery when
				// IncludeUnsubscribed is set; otherwise skip silently and count.
				if !p.IncludeUnsubscribed {
					skippedUnsubscribed++
					continue
				}
				podTitle, cdnURL, metaErr := FetchPodcastMeta(ctx, client, podUUID)
				if metaErr == nil && cdnURL != "" {
					feedURL = cdnURL
				} else if metaErr == nil && podTitle != "" {
					feedURL, _ = LookupFeedURL(ctx, podTitle, "")
				}
				if feedURL == "" {
					if podTitle != "" {
						fmt.Printf("pocketcasts: warning: no feed URL found for %q (%s) — play state skipped\n", podTitle, podUUID)
					} else {
						fmt.Printf("pocketcasts: warning: no feed URL found for podcast %s — play state skipped\n", podUUID)
					}
					continue
				}
				podUUIDToFeedURL[podUUID] = feedURL
				// Add as a podcast entry so it appears in the subscription list.
				if podTitle == "" {
					podTitle, _, _ = FetchPodcastMeta(ctx, client, podUUID)
				}
				podcasts = append(podcasts, model.Podcast{FeedURL: feedURL, Title: podTitle})
				fmt.Printf("pocketcasts: recovered feed URL for unsubscribed podcast %q via CDN/iTunes\n", podTitle)
				time.Sleep(DefaultRequestDelay)
			}
			for page := 0; ; page++ {
				pageEps, hasMore, err := FetchPodcastEpisodes(ctx, client, podUUID, page)
				if err != nil {
					fmt.Printf("  warning: could not fetch CDN episodes for podcast %s (page %d): %v\n", podUUID, page, err)
					break
				}
				for _, ep := range pageEps {
					if seenUUIDs[ep.UUID] || ep.IsDeleted {
						continue
					}
					se, ok := syncByUUID[ep.UUID]
					if !ok {
						continue // not interacted with
					}
					switch se.PlayingStatus {
					case PlayingPlayed, PlayingInProgress:
					default:
						continue
					}
					es := model.EpisodeState{
						FeedURL: feedURL,
						Title:   ep.Title,
						PubDate: ep.ParsePublishedAt(),
					}
					if ep.Duration > 0 {
						es.Duration = time.Duration(ep.Duration) * time.Second
					}
					es.PlayState, es.PlayPosition = inferPlayState(se.PlayingStatus, int(se.PlayedUpTo), ep.Duration)
					seenUUIDs[ep.UUID] = true
					episodes = append(episodes, es)
					fetched++
				}
				if !hasMore {
					break
				}
				time.Sleep(DefaultRequestDelay)
			}
			time.Sleep(DefaultRequestDelay)
		}
		if skippedUnsubscribed > 0 {
			fmt.Printf("pocketcasts: skipped %d unsubscribed podcast(s) with play history (use --pc-include-unsubscribed to include)\n", skippedUnsubscribed)
		}
		fmt.Printf("pocketcasts: %d episode(s) read (complete history via sync/update)\n", fetched)
	} else {
		// Fallback: /user/in_progress + /user/history (capped at ~100 played).
		fmt.Printf("pocketcasts: fetching in-progress episodes...\n")
		inProgress, err := FetchInProgressEpisodes(ctx, client)
		if err != nil {
			fmt.Printf("pocketcasts: warning: could not fetch in-progress episodes (%v)\n", err)
		}
		for _, ep := range inProgress {
			feedURL := podUUIDToFeedURL[ep.PodcastUUID]
			if feedURL == "" || ep.IsDeleted {
				continue
			}
			es := episodeStateFromAPI(&ep, feedURL)
			if es == nil {
				continue
			}
			seenUUIDs[ep.UUID] = true
			episodes = append(episodes, *es)
		}
		fmt.Printf("pocketcasts: %d in-progress episode(s) read\n", len(episodes))

		time.Sleep(DefaultRequestDelay)
		fmt.Printf("pocketcasts: fetching play history...\n")
		played, err := FetchPlayedEpisodes(ctx, client)
		if err != nil {
			fmt.Printf("pocketcasts: warning: could not fetch play history (%v)\n", err)
		}
		playedAdded := 0
		for _, ep := range played {
			if seenUUIDs[ep.UUID] {
				continue
			}
			feedURL := podUUIDToFeedURL[ep.PodcastUUID]
			if feedURL == "" {
				continue
			}
			es := episodeStateFromAPI(&ep, feedURL)
			if es == nil {
				continue
			}
			seenUUIDs[ep.UUID] = true
			episodes = append(episodes, *es)
			playedAdded++
		}
		fmt.Printf("pocketcasts: %d played episode(s) read from history (capped ~100)\n", playedAdded)
	}

	return &model.Library{
		Podcasts:       podcasts,
		Episodes:       episodes,
		ExportedAt:     time.Now(),
		SourceProvider: "Pocket Casts",
	}, nil
}

// episodeStateFromAPI converts an APIEpisode to a model.EpisodeState.
// Returns nil if the episode has no played/in-progress state.
func episodeStateFromAPI(ep *APIEpisode, feedURL string) *model.EpisodeState {
	es := &model.EpisodeState{
		FeedURL: feedURL,
		Title:   ep.Title,
		PubDate: ep.ParsePublishedAt(),
	}
	if ep.Duration > 0 {
		es.Duration = time.Duration(ep.Duration) * time.Second
	}
	switch ep.PlayingStatus {
	case PlayingPlayed, PlayingInProgress:
		es.PlayState, es.PlayPosition = inferPlayState(ep.PlayingStatus, ep.PlayedUpTo, ep.Duration)
	default:
		return nil
	}
	return es
}

// SetLibrary writes subscriptions and/or episode play state to Pocket Casts.
//
// Subscriptions are always written first: any podcast in lib that the user is
// not yet subscribed to in Pocket Casts is resolved via RSS feed URL and
// subscribed to before the play-state pass begins. This ensures that newly
// subscribed podcasts are visible during episode matching.
//
// With opts.OnlySubscriptions the play-state pass is skipped.
func (p *Provider) SetLibrary(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	prefix := ""
	if opts.DryRun {
		prefix = "[dry-run] "
	}

	// Step 1: subscribe to any missing podcasts (unless --subscribed-only, which
	// means "only write play state for already-subscribed feeds, don't subscribe
	// to new ones").
	if !opts.SubscribedOnly {
		subCount, err := p.doWriteSubscriptions(ctx, lib, opts)
		if err != nil {
			return err
		}
		if subCount > 0 {
			fmt.Printf("%ssubscribed to %d podcast(s)\n", prefix, subCount)
		}
	}

	if opts.OnlySubscriptions {
		return nil
	}

	// Step 2: write episode play state. doWritePlayState does its own auth and
	// subscription fetch, so it will see any podcasts added in Step 1.
	n, err := p.doWritePlayState(ctx, lib, opts)
	if err != nil {
		return err
	}
	fmt.Printf("%supdated play state for %d episode(s)\n", prefix, n)
	return nil
}

// doWriteSubscriptions subscribes the authenticated user to every podcast in
// lib that is not already in their Pocket Casts library. Each new podcast is
// resolved from its RSS feed URL to a Pocket Casts UUID via the public
// refresh.pocketcasts.com service, then subscribed via the authenticated API.
// Returns the number of new subscriptions created (or that would be created in
// a dry-run).
func (p *Provider) doWriteSubscriptions(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	if len(lib.Podcasts) == 0 {
		return 0, nil
	}

	requestDelay := opts.RequestDelay
	if requestDelay <= 0 {
		requestDelay = DefaultRequestDelay
	}

	fmt.Printf("pocketcasts: authenticating as %s...\n", p.email)
	client, err := Login(ctx, p.email, p.password)
	if err != nil {
		return 0, fmt.Errorf("pocketcasts: authentication failed: %w", err)
	}
	time.Sleep(requestDelay)

	fmt.Printf("pocketcasts: fetching existing subscriptions...\n")
	existing, err := FetchSubscribedPodcasts(ctx, client)
	if err != nil {
		return 0, fmt.Errorf("pocketcasts: fetch subscriptions: %w", err)
	}

	// Build lookup structures for the existing subscription list.
	// subscribedFeeds:  normalised URL → true (fast URL-based check).
	// subscribedUUIDs:  PC UUID → true (fallback for URL-mismatch — same podcast
	//   can have different RSS URLs across apps).
	// subscribedTitles: normalised title → true (fallback for private/subscriber
	//   feeds whose URLs are personalised and never match the public PC URL, e.g.
	//   NYT "The Daily - Subscriber Feed (🔓 for you@…)").
	subscribedFeeds := make(map[string]bool, len(existing))
	subscribedUUIDs := make(map[string]bool, len(existing))
	subscribedTitles := make(map[string]bool, len(existing))
	for _, pod := range existing {
		if pod.URL != "" {
			subscribedFeeds[normalizeFeedURL(pod.URL)] = true
		}
		if pod.UUID != "" {
			subscribedUUIDs[pod.UUID] = true
		}
		if normTitle := model.NormalizePlusTitle(pod.Title); normTitle != "" {
			subscribedTitles[normTitle] = true
		}
	}

	// Build the candidate list: source podcasts not yet subscribed, narrowed by
	// any active podcast filter (--podcast / --podcast-list). The filter is
	// applied here so that `--podcast "xyz"` only subscribes to matching feeds,
	// not every feed in the source library.
	lowerFilters := make([]string, len(opts.PodcastFilter))
	for i, f := range opts.PodcastFilter {
		lowerFilters[i] = strings.ToLower(strings.TrimSpace(f))
	}

	var toSubscribe []model.Podcast
	skippedByFilter := 0
	alreadySubscribedByURL := 0
	for _, pod := range lib.Podcasts {
		if pod.FeedURL == "" {
			continue
		}
		// Apply podcast filter when set.
		if len(lowerFilters) > 0 {
			title := strings.ToLower(strings.TrimSpace(pod.Title))
			matched := false
			for _, f := range lowerFilters {
				if f != "" && strings.Contains(title, f) {
					matched = true
					break
				}
			}
			if !matched {
				skippedByFilter++
				continue
			}
		}
		if subscribedFeeds[normalizeFeedURL(pod.FeedURL)] {
			alreadySubscribedByURL++
			continue
		}
		// Title-based check: private/subscriber feeds (e.g. NYT subscriber
		// feeds) have personalised URLs that won't URL-match any PC subscription,
		// but their normalised title ("the daily") does.
		if normTitle := model.NormalizePlusTitle(pod.Title); normTitle != "" && subscribedTitles[normTitle] {
			alreadySubscribedByURL++
			continue
		}
		toSubscribe = append(toSubscribe, pod)
	}

	if len(toSubscribe) == 0 {
		if skippedByFilter > 0 {
			fmt.Printf("pocketcasts: no unsubscribed podcasts match the podcast filter (%d skipped by filter)\n",
				skippedByFilter)
		} else {
			fmt.Printf("pocketcasts: all %d in-scope podcast(s) already subscribed\n",
				alreadySubscribedByURL)
		}
		return 0, nil
	}

	if skippedByFilter > 0 {
		fmt.Printf("pocketcasts: podcast filter active — %d/%d podcast(s) to check (%d skipped by filter, %d already subscribed by URL)\n",
			len(toSubscribe), len(lib.Podcasts), skippedByFilter, alreadySubscribedByURL)
	} else {
		fmt.Printf("pocketcasts: %d/%d podcast(s) to check (%d already subscribed by URL)\n",
			len(toSubscribe), len(lib.Podcasts), alreadySubscribedByURL)
	}

	if opts.DryRun {
		for _, pod := range toSubscribe {
			title := pod.Title
			if title == "" {
				title = pod.FeedURL
			}
			if pod.IsPrivate {
				fmt.Printf("  [dry-run] would attempt private feed: %q (%s)\n", title, pod.FeedURL)
			} else {
				fmt.Printf("  [dry-run] would subscribe: %q\n", title)
			}
		}
		return len(toSubscribe), nil
	}

	subscribed := 0
	failed := 0
	var skippedPods []model.Podcast
	for _, pod := range toSubscribe {
		title := pod.Title
		if title == "" {
			title = pod.FeedURL
		}

		// Resolve podcast to a Pocket Casts UUID.
		// For private/subscriber feeds (IsPrivate=true) always try the feed URL
		// first via add_feed_url — the iTunes ID always resolves to the public
		// canonical, which is wrong when the KVS subscriber URL is the target.
		// If add_feed_url also fails, collect in skipped-feeds OPML.
		// For public catalog feeds, use iTunes ID as a fast path first.
		var pcUUID string
		if pod.ITunesID != "" && !pod.IsPrivate {
			if itunesID, parseErr := strconv.ParseInt(pod.ITunesID, 10, 64); parseErr == nil {
				if uuid, lookupErr := FindPodcastByITunesID(ctx, itunesID); lookupErr == nil && uuid != "" {
					pcUUID = uuid
				}
			}
		}
		if pcUUID == "" {
			var err error
			pcUUID, err = ResolveFeedToPodcastUUID(ctx, pod.FeedURL)
			if err != nil {
				if pod.IsPrivate {
					fmt.Printf("  note: %q is a private/subscriber feed — will include in skipped-feeds OPML for manual import\n", title)
					skippedPods = append(skippedPods, pod)
				} else {
					fmt.Printf("  warning: could not resolve %q: %v\n", title, err)
					failed++
				}
				continue
			}
		}

		// Check by UUID: the same podcast may be subscribed under a different RSS
		// feed URL (different CDN, http vs https, etc.). If the resolved UUID is
		// already in the subscription list, skip without re-subscribing.
		if subscribedUUIDs[pcUUID] {
			fmt.Printf("  already subscribed: %q (feed URL differs from PC but same podcast)\n", title)
			continue
		}

		// Subscribe.
		if err := SubscribePodcast(ctx, client, pcUUID); err != nil {
			fmt.Printf("  warning: could not subscribe to %q: %v\n", title, err)
			failed++
			continue
		}

		subscribed++
		if subscribed <= 5 || subscribed%20 == 0 {
			fmt.Printf("  [%d/%d] ✓ subscribed: %q\n", subscribed, len(toSubscribe), title)
		}
		time.Sleep(requestDelay)
	}

	fmt.Printf("pocketcasts: subscriptions: %d added, %d private/skipped\n", subscribed, len(skippedPods))
	if failed > 0 {
		fmt.Printf("pocketcasts: %d subscription(s) failed (see warnings above)\n", failed)
	}
	if len(skippedPods) > 0 {
		p.writeSkippedOPML(skippedPods, false)
	}
	return subscribed, nil
}

// pcIndexEntry holds the Pocket Casts data needed to update an episode.
type pcIndexEntry struct {
	episodeUUID  string
	podcastUUID  string
	currentState model.PlayState
	currentPos   time.Duration
	// source records where this entry came from, for diagnostics.
	// Values: "A1" (in_progress endpoint), "A2" (history), "B-auth" (authenticated
	// per-podcast endpoint), "B-cdn" (public cache CDN, no real play state).
	source string
}

// doWritePlayState is the main write implementation. It:
//  1. Logs in and fetches the Pocket Casts library for matching (Phase A).
//  2. For unmatched episodes, fetches per-podcast episode lists (Phase B).
//  3. Iterates the merged library and calls UpdateEpisodeProgress.
func (p *Provider) doWritePlayState(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	// Build feedURL → podcast title lookup for filtering and logging.
	feedToTitle := buildFeedToTitle(lib)
	episodes := filterEpisodesByPodcast(lib.Episodes, feedToTitle, opts.PodcastFilter)
	if len(opts.PodcastFilter) > 0 {
		fmt.Printf("pocketcasts: podcast filter active — %q — %d/%d episode(s) in scope\n",
			opts.PodcastFilter, len(episodes), len(lib.Episodes))
	}
	writeLogHeader(opts.LogWriter)

	if len(episodes) == 0 {
		return 0, nil
	}

	requestDelay := opts.RequestDelay
	if requestDelay <= 0 {
		requestDelay = DefaultRequestDelay
	}

	// --- Login ---
	fmt.Printf("pocketcasts: authenticating as %s...\n", p.email)
	client, err := Login(ctx, p.email, p.password)
	if err != nil {
		return 0, fmt.Errorf("pocketcasts: authentication failed: %w", err)
	}
	time.Sleep(requestDelay)

	// --- Fetch subscribed podcasts ---
	fmt.Printf("pocketcasts: fetching subscribed podcasts...\n")
	apiPods, err := FetchSubscribedPodcasts(ctx, client)
	if err != nil {
		return 0, fmt.Errorf("pocketcasts: fetch subscriptions: %w", err)
	}
	time.Sleep(requestDelay)

	// Batch-resolve feed URLs missing from the subscription list (same logic as
	// GetLibrary — see that function for the full rationale).
	var missingURLUUIDs []string
	for _, ap := range apiPods {
		if ap.UUID != "" && ap.URL == "" {
			missingURLUUIDs = append(missingURLUUIDs, ap.UUID)
		}
	}
	exportedURLs := make(map[string]string)
	if len(missingURLUUIDs) > 0 {
		var exportErr error
		exportedURLs, exportErr = FetchExportFeedURLs(ctx, client, missingURLUUIDs)
		if exportErr != nil {
			fmt.Printf("pocketcasts: warning: export-feed-urls failed (%v) — some feed URLs may be missing\n", exportErr)
		}
	}
	time.Sleep(requestDelay)

	// Build bidirectional feed URL ↔ podcast UUID maps.
	podUUIDToFeedURL := make(map[string]string, len(apiPods))
	normFeedToPodUUID := make(map[string]string, len(apiPods))
	normFeedToPodTitle := make(map[string]string, len(apiPods))
	// normTitleToPodUUID: NormalizePlusTitle(pc.Title) → PC UUID.
	// Fallback for private/subscriber feeds whose URLs the refresh API doesn't
	// know (e.g. NYT "The Daily - Subscriber Feed (🔓 for you@…)").
	normTitleToPodUUID := make(map[string]string, len(apiPods))
	// fuzzyTitleToPodUUID: fuzzyPodcastTitle(pc.Title) → PC UUID.
	// Broader fallback that also strips season markers and punctuation, enabling
	// contains-match for subtitle differences ("Crooked City" ↔ "Crooked City: Dixon, IL").
	fuzzyTitleToPodUUID := make(map[string]string, len(apiPods))
	for _, ap := range apiPods {
		if ap.UUID == "" {
			continue
		}
		feedURL := ap.URL
		if feedURL == "" {
			feedURL = exportedURLs[ap.UUID]
		}
		// URL-based maps: only when a feed URL is known.
		if feedURL != "" {
			podUUIDToFeedURL[ap.UUID] = feedURL
			norm := normalizeFeedURL(feedURL)
			if _, exists := normFeedToPodUUID[norm]; !exists {
				normFeedToPodUUID[norm] = ap.UUID
				normFeedToPodTitle[norm] = ap.Title
			}
		}
		// Title-based maps: always populate, even for podcasts still without a
		// known URL — Phase B can still match by title in that case.
		if normTitle := model.NormalizePlusTitle(ap.Title); normTitle != "" {
			if _, exists := normTitleToPodUUID[normTitle]; !exists {
				normTitleToPodUUID[normTitle] = ap.UUID
			}
		}
		if ft := migrate.FuzzyPodcastTitle(ap.Title); ft != "" {
			if _, exists := fuzzyTitleToPodUUID[ft]; !exists {
				fuzzyTitleToPodUUID[ft] = ap.UUID
			}
		}
	}
	fmt.Printf("pocketcasts: %d subscribed podcast(s) loaded\n", len(apiPods))

	// --- Phase A: build index from in-progress and recently-played episodes ---
	//
	// Phase A is split into two fetches that both carry real user play state:
	//
	//   A1 — /user/in_progress : episodes currently being listened to.
	//   A2 — /user/history     : recently played (completed) episodes.
	//
	// Both are indexed before Phase B runs. This is critical because Phase B
	// uses the public cache CDN (podcast/full) which has NO play state — every
	// episode it returns gets PlayingStatus = PlayingUnplayed. Without A2, an
	// episode already played to completion in Pocket Casts would appear as
	// "unplayed" to the skip-reason check and be needlessly re-written, and
	// worse, an in-progress Apple episode would overwrite a "played" Pocket
	// Casts state that is ahead of the Apple position.
	fmt.Printf("pocketcasts: fetching in-progress episodes for matching...\n")
	inProgress, err := FetchInProgressEpisodes(ctx, client)
	if err != nil {
		// Non-fatal: continue without in-progress data; Phase B may still match.
		fmt.Printf("pocketcasts: warning: could not fetch in-progress episodes (%v) — Phase A1 skipped\n", err)
		inProgress = nil
	}
	time.Sleep(requestDelay)

	index := make(map[string]pcIndexEntry)
	for _, ep := range inProgress {
		feedURL := podUUIDToFeedURL[ep.PodcastUUID]
		if feedURL == "" || ep.IsDeleted {
			continue
		}
		addToIndex(index, &ep, feedURL, "A1")
	}
	fmt.Printf("pocketcasts: Phase A1: indexed %d in-progress episode(s)\n", len(inProgress))

	// Phase A2: recently-played history.  Non-fatal if the endpoint is
	// unavailable — Phase B will still find the episode UUID; the only
	// downside is that already-played episodes may be written again
	// (idempotent for played state, but potentially harmful for in-progress
	// episodes that have since been played to completion in Pocket Casts).
	fmt.Printf("pocketcasts: fetching recently-played episodes for matching...\n")
	played, err := FetchPlayedEpisodes(ctx, client)
	if err != nil {
		fmt.Printf("pocketcasts: warning: could not fetch play history (%v) — Phase A2 skipped\n", err)
		played = nil
	}
	time.Sleep(requestDelay)

	phaseA2Added := 0
	for _, ep := range played {
		feedURL := podUUIDToFeedURL[ep.PodcastUUID]
		if feedURL == "" {
			continue
		}
		// NOTE: we intentionally do NOT skip ep.IsDeleted here. The /user/history
		// endpoint routinely returns isDeleted=true even for normally-played
		// episodes of active subscriptions (it appears to mean "removed from the
		// active queue" rather than "episode deleted"). Skipping those entries
		// would make Phase A2 a near-no-op for many users.
		//
		// addToIndex uses "first entry wins" so in-progress entries from A1
		// are never overwritten by played entries from A2.
		beforeLen := len(index)
		addToIndex(index, &ep, feedURL, "A2")
		if len(index) > beforeLen {
			phaseA2Added++
		}
	}
	fmt.Printf("pocketcasts: Phase A2: indexed %d recently-played episode(s)\n", phaseA2Added)

	// --- Phase A_sync: full play-state overlay from /user/sync/update ---
	//
	// /user/sync/update is the protobuf endpoint used by Pocket Casts mobile apps.
	// It returns ALL episodes PC has ever interacted with — not capped at ~100 like
	// /user/history. We fetch it once here and use the resulting map as a play-state
	// overlay when CDN-fetched episodes (Phase B Pass 2) would otherwise default to
	// PlayingUnplayed.
	// lastModified=1 (not 0): see GetLibrary for why 0 returns a truncated subset.
	fmt.Printf("pocketcasts: fetching complete sync state for play-state overlay...\n")
	syncStateByUUID := make(map[string]SyncEpisodeState)
	syncEps, _, syncErr := FetchSyncUpdate(ctx, client, 1)
	if syncErr != nil {
		fmt.Printf("pocketcasts: warning: sync/update failed (%v) — CDN play-state overlay disabled\n", syncErr)
	} else {
		for _, se := range syncEps {
			syncStateByUUID[se.EpisodeUUID] = se
		}
		fmt.Printf("pocketcasts: sync/update: %d episode(s) in play-state overlay\n", len(syncStateByUUID))
	}
	time.Sleep(requestDelay)

	// --- Phase B: per-podcast episode fetch for unmatched source episodes ---
	//
	// Group source episodes with play state that are not yet in the index by
	// their (normalised) feed URL. For each such feed, look up the Pocket Casts
	// podcast UUID and page through the full episode list, adding every episode
	// to the index. This handles episodes played in Apple that have never been
	// opened in Pocket Casts (and therefore don't appear in the in-progress list).
	// unmatchedFeeds maps normalised source feed URL → original (Apple) feed URL.
	// The original URL is preserved so Phase B can pass it to ResolveFeedToPodcastUUID
	// when the normalised form doesn't match any PC subscription URL.
	unmatchedFeeds := make(map[string]string)
	for _, ep := range episodes {
		if ep.FromDestination || ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
			continue
		}
		if _, found := findInIndex(index, ep); !found {
			normFeed := normalizeFeedURL(ep.FeedURL)
			if _, exists := unmatchedFeeds[normFeed]; !exists {
				unmatchedFeeds[normFeed] = ep.FeedURL
			}
		}
	}

	if len(unmatchedFeeds) > 0 {
		fmt.Printf("pocketcasts: Phase B: %d feed(s) have unmatched episodes — fetching per-podcast episode lists...\n",
			len(unmatchedFeeds))

		// authEpAvailable starts true: attempt the authenticated /user/podcast/episodes
		// endpoint for each podcast before fetching CDN pages. If the endpoint
		// returns a non-retriable error it is disabled for all remaining Phase B
		// calls in this run.
		authEpAvailable := true

		fetched := 0 // episodes processed (auth + CDN passes combined)
		skipped := 0
		for normFeed, originalFeedURL := range unmatchedFeeds {
			podUUID := normFeedToPodUUID[normFeed]
			podTitle := normFeedToPodTitle[normFeed]
			if podTitle == "" {
				podTitle = originalFeedURL
			}

			// indexFeedURL is the URL used as the key when building the index.
			// It must match what findInIndex expects — which keys on the Apple
			// episode's FeedURL. In the normal case (URLs match after normalisation)
			// using the PC URL is fine; they normalise identically. In the mismatch
			// case we fall back to the original Apple URL so keys line up.
			indexFeedURL := podUUIDToFeedURL[podUUID]

			if podUUID == "" {
				// The source feed URL doesn't match any PC subscription URL.
				//
				// Strategy 1: resolve via the PC refresh API (handles CDN changes,
				// http→https, feed URL migration, etc.).
				//
				// Three outcomes when the refresh API succeeds:
				//  a) UUID is in our subscription map  → URL mismatch resolved, proceed.
				//  b) UUID not in map, !SubscribedOnly → podcast found in catalog but not
				//     yet subscribed; auto-subscribe so play state can be applied.
				//  c) UUID not in map, SubscribedOnly  → the subscription list API may
				//     have returned incomplete data (PC's /user/podcast/list endpoint
				//     occasionally omits subscriptions). Trust the refresh API result and
				//     proceed; this flag only prevents subscribing to genuinely new
				//     podcasts, not updating existing ones the API failed to return.
				resolvedUUID, resolveErr := ResolveFeedToPodcastUUID(ctx, originalFeedURL)
				if resolveErr == nil {
					if _, isSubscribed := podUUIDToFeedURL[resolvedUUID]; isSubscribed {
						// (a) Normal URL-mismatch case: UUID already in our list.
						podUUID = resolvedUUID
						// Use the Apple URL for index keys: findInIndex always uses
						// the Apple episode's FeedURL, so addToIndex must match.
						indexFeedURL = originalFeedURL
						fmt.Printf("  resolved feed URL mismatch for %q — PC UUID matched via refresh API\n", podTitle)
					} else if !opts.SubscribedOnly {
						// (b) Found in catalog but not yet subscribed — auto-subscribe.
						if subErr := SubscribePodcast(ctx, client, resolvedUUID); subErr != nil {
							fmt.Printf("  warning: could not auto-subscribe to %q: %v\n", podTitle, subErr)
						} else {
							podUUID = resolvedUUID
							indexFeedURL = originalFeedURL
							// Register in the local map so any later lookup for the
							// same podcast doesn't try to subscribe a second time.
							podUUIDToFeedURL[resolvedUUID] = originalFeedURL
							fmt.Printf("  auto-subscribed %q (found in PC catalog, not in subscription list)\n", podTitle)
							time.Sleep(requestDelay) // allow PC to process the subscription
						}
					} else {
						// (c) --subscribed-only and UUID missing from local map.
						// Proceed anyway: the subscription list API returned incomplete
						// data and this podcast is almost certainly already subscribed
						// (the refresh API only knows about podcasts in PC's catalog).
						podUUID = resolvedUUID
						indexFeedURL = originalFeedURL
						fmt.Printf("  resolved %q via refresh API (not in subscription list — subscription API may be incomplete)\n", podTitle)
					}
				}

				// Strategy 2: title-based match.  Private/subscriber feeds (e.g.
				// NYT "The Daily - Subscriber Feed (🔓 for you@…)") have
				// personalised URLs the refresh API doesn't know. Normalise the
				// source podcast title and look it up in the PC subscription list.
				//
				// Two passes:
				//  a) NormalizePlusTitle exact match — fast path for subscriber feeds.
				//  b) fuzzyPCTitle exact match, then contains-match — handles subtitle
				//     differences (e.g. Apple "Crooked City" ↔ PC "Crooked City: Dixon, IL").
				if podUUID == "" {
					srcTitle := feedToTitle[originalFeedURL]
					if srcTitle != "" {
						// Pass (a): NormalizePlusTitle exact match.
						if normTitle := model.NormalizePlusTitle(srcTitle); normTitle != "" {
							if uuid := normTitleToPodUUID[normTitle]; uuid != "" {
								podUUID = uuid
							}
						}
						// Pass (b): fuzzy exact, then contains.
						if podUUID == "" {
							srcFuzzy := migrate.FuzzyPodcastTitle(srcTitle)
							if srcFuzzy != "" {
								if uuid := fuzzyTitleToPodUUID[srcFuzzy]; uuid != "" {
									podUUID = uuid
								}
								if podUUID == "" && len(srcFuzzy) >= 5 {
									for pcFuzzy, uuid := range fuzzyTitleToPodUUID {
										if migrate.TitleHasWordPrefix(pcFuzzy, srcFuzzy) || migrate.TitleHasWordPrefix(srcFuzzy, pcFuzzy) {
											podUUID = uuid
											break
										}
									}
								}
							}
						}
						if podUUID != "" {
							indexFeedURL = originalFeedURL
							fmt.Printf("  matched %q to PC podcast by title\n", podTitle)
						}
					}
				}

				if podUUID == "" {
					fmt.Printf("  skipping %q: could not resolve to a PC podcast (feed URL not in PC catalog)\n", podTitle)
					skipped++
					continue
				}
			}

			// Pass 1 (authenticated): fetch episodes with real play state so
			// Phase B doesn't blindly treat already-played episodes as unplayed.
			// addToIndex is first-entry-wins: these entries are never overwritten
			// by the CDN pass below.
			if authEpAvailable {
				authEps, err := fetchUserEpisodesWithRetry(ctx, client, podUUID, requestDelay)
				if err != nil {
					var rl *httputil.RateLimitError
					var te *httputil.TransientError
					if !errors.As(err, &rl) && !errors.As(err, &te) {
						// Non-retriable error — authenticated endpoint not available for
						// this run; skip it for all remaining podcasts.
						fmt.Printf("  authenticated episode endpoint unavailable (%v) — using CDN only\n", err)
						authEpAvailable = false
					}
				} else {
					for _, ep := range authEps {
					if indexFeedURL == "" {
						continue
					}
					addToIndex(index, &ep, indexFeedURL, "B-auth")
					fetched++
				}
				time.Sleep(requestDelay)
				}
			}

			// Pass 2 (CDN): fill in episodes not returned by the authenticated
			// endpoint (typically older episodes). addToIndex's first-entry-wins
			// ensures Pass 1 play-state entries are not overwritten.
			for page := 0; page < maxPhaseBPagesPerPodcast; page++ {
				pageEps, hasMore, err := fetchPodcastEpisodesWithRetry(ctx, client, podUUID, page, requestDelay)
				if err != nil {
					fmt.Printf("  warning: could not fetch episodes for %q (page %d): %v\n", podTitle, page, err)
					break
				}

				for _, ep := range pageEps {
					if indexFeedURL == "" || ep.IsDeleted {
						continue
					}
					// Overlay accurate play state from sync/update. CDN episodes
					// default to PlayingUnplayed; the sync state has the real values.
					if se, ok := syncStateByUUID[ep.UUID]; ok {
						ep.PlayingStatus = se.PlayingStatus
						ep.PlayedUpTo = int(se.PlayedUpTo)
					}
					addToIndex(index, &ep, indexFeedURL, "B-cdn")
					fetched++
				}

				if !hasMore {
					break // all pages exhausted
				}
				time.Sleep(requestDelay)
			}
		}

		if fetched > 0 {
			fmt.Printf("pocketcasts: Phase B: indexed %d episode(s) across %d feed(s)\n", fetched, len(unmatchedFeeds)-skipped)
		}
		if skipped > 0 {
			fmt.Printf("pocketcasts: Phase B: %d feed(s) skipped — not subscribed in Pocket Casts\n", skipped)
		}
	}

	// --- Dry-run preview ---
	if opts.DryRun {
		n := 0
		for _, ep := range episodes {
			if ep.FromDestination || ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
				continue
			}
			podTitle := feedToTitle[ep.FeedURL]
			entry, ok := findInIndex(index, ep)
			if !ok {
				dryRunNote := "no match found in Pocket Casts account"
				if ep.FeedURL != "" {
					dryRunNote += " [feed: " + ep.FeedURL + "]"
				}
				writeLogLine(opts.LogWriter, "not_found", podTitle, ep.Title, ep.PubDate,
					playStateLabel(ep.PlayState, ep.PlayPosition), "—", dryRunNote)
				continue
			}
			if !opts.ForceUpdate {
				if reason := migrate.SkipReason(ep.PlayState, ep.PlayPosition, entry.currentState, entry.currentPos); reason != "" {
					writeLogLine(opts.LogWriter, reason, podTitle, ep.Title, ep.PubDate,
						playStateLabel(ep.PlayState, ep.PlayPosition),
						playStateLabel(entry.currentState, entry.currentPos), "")
					continue
				}
			}
			fmt.Printf("  [dry-run] would set progress: %q — %q → %s\n",
				podTitle, ep.Title, playStateLabel(ep.PlayState, ep.PlayPosition))
			writeLogLine(opts.LogWriter, "would_update", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—", "")
			n++
		}
		return n, nil
	}

	// --- Pre-count ---
	toUpdate := 0
	alreadyDone := 0
	for _, ep := range episodes {
		if ep.FromDestination || ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
			continue
		}
		entry, ok := findInIndex(index, ep)
		if !ok {
			continue
		}
		if !opts.ForceUpdate && migrate.SkipReason(ep.PlayState, ep.PlayPosition, entry.currentState, entry.currentPos) != "" {
			alreadyDone++
		} else {
			toUpdate++
		}
	}
	if alreadyDone > 0 {
		fmt.Printf("pocketcasts: skipping %d already-satisfied episode(s) (Pocket Casts state matches or exceeds source)\n", alreadyDone)
	}
	fmt.Printf("pocketcasts: request delay: %v between calls\n", requestDelay)
	fmt.Printf("pocketcasts: writing play state for %d episode(s)...\n", toUpdate)

	// --- Write loop ---
	updated := 0
	apiSkipped := 0
	alreadySatisfied := 0
	notFound := 0

	for _, ep := range episodes {
		if ep.FromDestination || ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
			continue
		}
		podTitle := feedToTitle[ep.FeedURL]

		entry, ok := findInIndex(index, ep)
		if !ok {
			notFound++
			notFoundNote := "episode not matched in Pocket Casts (not subscribed or not found in episode list)"
			if ep.FeedURL != "" {
				notFoundNote += " [feed: " + ep.FeedURL + "]"
			}
			writeLogLine(opts.LogWriter, "not_found", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—", notFoundNote)
			continue
		}

		if !opts.ForceUpdate {
			if reason := migrate.SkipReason(ep.PlayState, ep.PlayPosition, entry.currentState, entry.currentPos); reason != "" {
				writeLogLine(opts.LogWriter, reason, podTitle, ep.Title, ep.PubDate,
					playStateLabel(ep.PlayState, ep.PlayPosition),
					playStateLabel(entry.currentState, entry.currentPos), "")
				alreadySatisfied++
				continue
			}
		}

		// Determine API parameters.
		status := PlayingPlayed
		positionSec := 0
		if ep.PlayState == model.PlayStateInProgress {
			status = PlayingInProgress
			positionSec = int(ep.PlayPosition.Seconds())
		}
		durationSec := 0
		if ep.Duration > 0 {
			durationSec = int(ep.Duration.Seconds())
			if ep.PlayState == model.PlayStatePlayed {
				positionSec = durationSec
			}
		}

		writeErr := httputil.RetryFunc(ctx, func() error {
			return UpdateEpisodeProgress(ctx, client,
				entry.episodeUUID, entry.podcastUUID,
				status, positionSec, durationSec)
		}, httputil.RetryOptions{})

		if writeErr != nil {
			fmt.Printf("  [%d/%d] FAILED %q: %v\n", updated+apiSkipped+1, toUpdate, ep.Title, writeErr)
			writeLogLine(opts.LogWriter, "error", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—", writeErr.Error())
			apiSkipped++
			continue
		}

		writeLogLine(opts.LogWriter, "updated", podTitle, ep.Title, ep.PubDate,
			playStateLabel(ep.PlayState, ep.PlayPosition),
			playStateLabel(entry.currentState, entry.currentPos),
			"pc_source:"+entry.source)
		updated++

		if updated <= 5 || updated%50 == 0 {
			posStr := "played"
			if ep.PlayState == model.PlayStateInProgress {
				posStr = fmt.Sprintf("%ds", positionSec)
			}
			title := ep.Title
			if len(title) > 60 {
				title = title[:57] + "..."
			}
			fmt.Printf("  [%d/%d] ✓ %q → %s\n", updated, toUpdate, title, posStr)
		}

		time.Sleep(requestDelay)
	}

	if notFound > 0 {
		fmt.Printf("pocketcasts: %d episode(s) not matched in Pocket Casts (not subscribed or not found in episode list)\n", notFound)
	}
	if alreadySatisfied > 0 {
		fmt.Printf("pocketcasts: %d episode(s) skipped — Pocket Casts state already matches or exceeds source\n", alreadySatisfied)
	}
	if apiSkipped > 0 {
		fmt.Printf("pocketcasts: %d episode(s) failed during write (see warnings above)\n", apiSkipped)
	}
	return updated, nil
}

// addToIndex adds a Pocket Casts episode to the index under all applicable match keys.
// feedURL is the RSS feed URL of the parent podcast (resolved from podUUID → feedURL).
// source identifies which phase/endpoint provided this entry (e.g. "A1", "A2", "B-auth", "B-cdn").
func addToIndex(index map[string]pcIndexEntry, ep *APIEpisode, feedURL string, source string) {
	normFeed := normalizeFeedURL(feedURL)
	pubTime := ep.ParsePublishedAt()

	currState, currPos := inferPlayState(ep.PlayingStatus, ep.PlayedUpTo, ep.Duration)
	entry := pcIndexEntry{
		episodeUUID:  ep.UUID,
		podcastUUID:  ep.PodcastUUID,
		currentState: currState,
		currentPos:   currPos,
		source:       source,
	}

	// Primary: feed URL + pub date (second precision).
	if !pubTime.IsZero() && normFeed != "" {
		key := "feeddate:" + normFeed + "|" + pubTime.UTC().Format(time.RFC3339)
		if _, exists := index[key]; !exists {
			index[key] = entry
		}
	}
	// Fallback: feed URL + fuzzy-normalised title.
	// FuzzyNormalizeTitle strips season markers (S01, Season 1, …) and
	// punctuation so that title variants across subscriber and public feeds
	// ("The Retrievals - Ep. 4" vs "The Retrievals S01 - Ep. 4") resolve to
	// the same key and are recognised as the same episode.
	if normFeed != "" && ep.Title != "" {
		key := "feedtitle:" + normFeed + "|" + migrate.FuzzyNormalizeTitle(ep.Title)
		if _, exists := index[key]; !exists {
			index[key] = entry
		}
	}
	// Cross-podcast fallback: fuzzy title + calendar date (no feed URL).
	// Used when a podcast network cross-posts an episode to multiple feeds
	// and Apple attributes it to a different show than PC does.  Day-level
	// precision is intentional: it tolerates timezone differences between
	// Apple's CoreData timestamps and PC's published_at field while still
	// being specific enough to avoid false positives across different shows.
	// findInIndex only uses this key as a last resort.
	if !pubTime.IsZero() && ep.Title != "" {
		key := "titledate:" + migrate.FuzzyNormalizeTitle(ep.Title) + "|" + pubTime.UTC().Format("2006-01-02")
		if _, exists := index[key]; !exists {
			index[key] = entry
		}
	}
}

// findInIndex matches ep against the Pocket Casts episode index.
//
// Implemented strategies (migrate.MatchStrategy), in priority order:
//   - MatchByFeedDate  — feed URL + exact pub date.
//   - MatchByFeedTitle — feed URL + fuzzy-normalised title.
//   - MatchByTitleDate — fuzzy title + calendar day (no feed URL); cross-feed fallback
//     for episodes that a podcast network cross-posts to multiple feeds and that
//     Apple and PC attribute to different shows.
//
// Absent strategies and rationale:
//   - MatchByGUID: Pocket Casts exposes episode UUIDs, not RSS GUIDs; the two
//     ID spaces are incompatible so GUID-based matching is not possible.
//   - MatchByPodDate, MatchByPodTitle: Pocket Casts podcast titles are stored
//     in a separate API call that is not always available at match time.
//     MatchByTitleDate covers the same cross-feed scenarios with a cheaper lookup.
func findInIndex(index map[string]pcIndexEntry, ep model.EpisodeState) (pcIndexEntry, bool) {
	normFeed := normalizeFeedURL(ep.FeedURL)
	if !ep.PubDate.IsZero() && ep.FeedURL != "" {
		key := "feeddate:" + normFeed + "|" + ep.PubDate.UTC().Format(time.RFC3339)
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	// Uses FuzzyNormalizeTitle to match across season-marker variants
	// ("The Retrievals - Ep. 4" ↔ "The Retrievals S01 - Ep. 4").
	if ep.FeedURL != "" && ep.Title != "" {
		key := "feedtitle:" + normFeed + "|" + migrate.FuzzyNormalizeTitle(ep.Title)
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	// Cross-podcast fallback: title + date without feed URL.  Only reached
	// when both feed-URL-based lookups fail, meaning the episode may have
	// been attributed to a different podcast in Apple than in PC.
	if !ep.PubDate.IsZero() && ep.Title != "" {
		key := "titledate:" + migrate.FuzzyNormalizeTitle(ep.Title) + "|" + ep.PubDate.UTC().Format("2006-01-02")
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	return pcIndexEntry{}, false
}

// nearEndThreshold is the maximum seconds before the end of an episode at which
// a Pocket Casts InProgress episode is promoted to Played. Pocket Casts does not
// always transition playing_status to 3 (Played) when the playhead reaches the
// last few seconds (e.g. after the main content ends before outro music finishes).
const nearEndThreshold = 60

// inferPlayState converts a Pocket Casts playing_status + playedUpToSec + durationSec
// into a model PlayState and position. InProgress episodes within nearEndThreshold
// seconds of the total duration are promoted to Played so skip-reason checks fire
// correctly instead of triggering redundant writes.
func inferPlayState(playingStatus, playedUpToSec, durationSec int) (model.PlayState, time.Duration) {
	switch playingStatus {
	case PlayingPlayed:
		return model.PlayStatePlayed, 0
	case PlayingInProgress:
		if durationSec > 0 && playedUpToSec > 0 && playedUpToSec >= durationSec-nearEndThreshold {
			return model.PlayStatePlayed, 0
		}
		return model.PlayStateInProgress, time.Duration(playedUpToSec) * time.Second
	default:
		return model.PlayStateUnplayed, 0
	}
}

// fetchUserEpisodesWithRetry calls FetchUserPodcastEpisodes (authenticated, real
// play state) with retry on rate-limit and transient errors. Returns the episode
// slice; the authenticated endpoint returns all episodes in one response so
// there is no page parameter.
func fetchUserEpisodesWithRetry(ctx context.Context, client *http.Client,
	podcastUUID string, requestDelay time.Duration) ([]APIEpisode, error) {

	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<uint(attempt)) * 30 * time.Second
			fmt.Printf("  rate limited (auth) fetching episode list — waiting %v (attempt %d/%d)...\n",
				wait, attempt+1, maxAttempts)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		eps, _, err := FetchUserPodcastEpisodes(ctx, client, podcastUUID, 0)
		if err == nil {
			return eps, nil
		}

		var rl *httputil.RateLimitError
		if errors.As(err, &rl) {
			if attempt == 0 {
				fmt.Printf("  rate limited (auth) — waiting %v...\n", rl.Wait)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(rl.Wait):
				}
			}
			lastErr = err
			continue
		}
		var te *httputil.TransientError
		if errors.As(err, &te) {
			lastErr = err
			continue
		}
		return nil, err // non-retriable
	}
	return nil, lastErr
}

// fetchPodcastEpisodesWithRetry calls FetchPodcastEpisodes (public cache CDN,
// no play state) with retry on rate-limit (429) responses. Up to 4 attempts
// with exponential back-off.
func fetchPodcastEpisodesWithRetry(ctx context.Context, client *http.Client,
	podcastUUID string, page int, requestDelay time.Duration) ([]APIEpisode, bool, error) {

	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<uint(attempt)) * 30 * time.Second
			fmt.Printf("  rate limited fetching episode list (page %d) — waiting %v (attempt %d/%d)...\n",
				page, wait, attempt+1, maxAttempts)
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-time.After(wait):
			}
		}

		eps, hasMore, err := FetchPodcastEpisodes(ctx, client, podcastUUID, page)
		if err == nil {
			return eps, hasMore, nil
		}

		var rl *httputil.RateLimitError
		if errors.As(err, &rl) {
			wait := rl.Wait
			if attempt == 0 {
				fmt.Printf("  rate limited — waiting %v...\n", wait)
				select {
				case <-ctx.Done():
					return nil, false, ctx.Err()
				case <-time.After(wait):
				}
			}
			lastErr = err
			continue
		}
		return nil, false, err // non-rate-limit error: give up immediately
	}
	return nil, false, lastErr
}

// The following helpers delegate to internal/migrate for shared behaviour
// across providers. Package-local names are preserved so call sites throughout
// this file don't need to change.

// buildFeedToTitle returns a map from feed URL to lowercased podcast title.
func buildFeedToTitle(lib *model.Library) map[string]string {
	return migrate.BuildFeedToTitle(lib)
}

// filterEpisodesByPodcast returns episodes matching any of the filter strings.
func filterEpisodesByPodcast(episodes []model.EpisodeState, feedToTitle map[string]string, filters []string) []model.EpisodeState {
	return migrate.FilterEpisodesByPodcast(episodes, feedToTitle, filters)
}

// normalizeFeedURL returns a canonical form of a podcast feed URL for matching.
// See migrate.NormalizeFeedURL for full documentation.
func normalizeFeedURL(raw string) string { return migrate.NormalizeFeedURL(raw) }

