package pocketcasts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/model"
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
// Reading:  fetches podcast subscriptions and in-progress episode play state
//           from the Pocket Casts web API.
//
// Writing:  writes episode play state via the same API.  Subscription writes
//           are not yet implemented (Phase 2).
//
// All operations require email and password credentials for the Pocket Casts
// account. The unofficial web API is used — see web.go for details.
type Provider struct {
	email    string
	password string
}

// NewProvider returns a Pocket Casts provider with the given credentials.
func NewProvider(email, password string) *Provider {
	return &Provider{email: email, password: password}
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
// Play state: currently-in-progress episodes only. Fully-played episodes are
// not included in the read path (Phase 1 limitation — see Phase 2 plan for
// full history support via the /user/history API).
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

	// Build podUUID → feedURL map for episode join.
	podUUIDToFeedURL := make(map[string]string, len(apiPods))
	var podcasts []model.Podcast
	for _, ap := range apiPods {
		if ap.UUID == "" || ap.URL == "" {
			continue
		}
		podUUIDToFeedURL[ap.UUID] = ap.URL
		podcasts = append(podcasts, model.Podcast{
			FeedURL: ap.URL,
			Title:   ap.Title,
			Author:  ap.Author,
		})
	}
	fmt.Printf("pocketcasts: %d subscribed podcast(s)\n", len(podcasts))

	fmt.Printf("pocketcasts: fetching in-progress episodes...\n")
	inProgress, err := FetchInProgressEpisodes(ctx, client)
	if err != nil {
		// Non-fatal: log and continue with an empty episode list.
		fmt.Printf("pocketcasts: warning: could not fetch in-progress episodes (%v)\n", err)
		inProgress = nil
	}

	var episodes []model.EpisodeState
	for _, ep := range inProgress {
		feedURL := podUUIDToFeedURL[ep.PodcastUUID]
		if feedURL == "" || ep.IsDeleted {
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
		switch ep.PlayingStatus {
		case PlayingPlayed:
			es.PlayState = model.PlayStatePlayed
		case PlayingInProgress:
			es.PlayState = model.PlayStateInProgress
			es.PlayPosition = time.Duration(ep.PlayedUpTo) * time.Second
		default:
			// Unplayed episodes from the in-progress list are unusual but skip them.
			continue
		}
		episodes = append(episodes, es)
	}
	fmt.Printf("pocketcasts: %d in-progress episode(s) read\n", len(episodes))

	return &model.Library{
		Podcasts:       podcasts,
		Episodes:       episodes,
		ExportedAt:     time.Now(),
		SourceProvider: "Pocket Casts",
	}, nil
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
			fmt.Printf("  [dry-run] would subscribe: %q\n", title)
		}
		return len(toSubscribe), nil
	}

	subscribed := 0
	failed := 0
	for _, pod := range toSubscribe {
		title := pod.Title
		if title == "" {
			title = pod.FeedURL
		}

		// Resolve RSS feed URL → Pocket Casts podcast UUID.
		pcUUID, err := ResolveFeedToPodcastUUID(ctx, pod.FeedURL)
		if err != nil {
			fmt.Printf("  warning: could not resolve %q: %v\n", title, err)
			failed++
			continue
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

	if failed > 0 {
		fmt.Printf("pocketcasts: %d subscription(s) failed (see warnings above)\n", failed)
	}
	return subscribed, nil
}

// pcIndexEntry holds the Pocket Casts data needed to update an episode.
type pcIndexEntry struct {
	episodeUUID  string
	podcastUUID  string
	currentState model.PlayState
	currentPos   time.Duration
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

	// Build bidirectional feed URL ↔ podcast UUID maps.
	podUUIDToFeedURL := make(map[string]string, len(apiPods))
	normFeedToPodUUID := make(map[string]string, len(apiPods))
	normFeedToPodTitle := make(map[string]string, len(apiPods))
	// normTitleToPodUUID: normalised PC podcast title → PC UUID.
	// Fallback for private/subscriber feeds whose URLs the refresh API doesn't
	// know (e.g. NYT "The Daily - Subscriber Feed (🔓 for you@…)").
	normTitleToPodUUID := make(map[string]string, len(apiPods))
	for _, ap := range apiPods {
		if ap.UUID == "" || ap.URL == "" {
			continue
		}
		podUUIDToFeedURL[ap.UUID] = ap.URL
		norm := normalizeFeedURL(ap.URL)
		if _, exists := normFeedToPodUUID[norm]; !exists {
			normFeedToPodUUID[norm] = ap.UUID
			normFeedToPodTitle[norm] = ap.Title
		}
		if normTitle := model.NormalizePlusTitle(ap.Title); normTitle != "" {
			if _, exists := normTitleToPodUUID[normTitle]; !exists {
				normTitleToPodUUID[normTitle] = ap.UUID
			}
		}
	}
	fmt.Printf("pocketcasts: %d subscribed podcast(s) loaded\n", len(apiPods))

	// --- Phase A: build index from in-progress episodes ---
	fmt.Printf("pocketcasts: fetching in-progress episodes for matching...\n")
	inProgress, err := FetchInProgressEpisodes(ctx, client)
	if err != nil {
		// Non-fatal: continue without in-progress data; Phase B may still match.
		fmt.Printf("pocketcasts: warning: could not fetch in-progress episodes (%v) — Phase A skipped\n", err)
		inProgress = nil
	}
	time.Sleep(requestDelay)

	index := make(map[string]pcIndexEntry)
	for _, ep := range inProgress {
		feedURL := podUUIDToFeedURL[ep.PodcastUUID]
		if feedURL == "" || ep.IsDeleted {
			continue
		}
		addToIndex(index, &ep, feedURL)
	}
	fmt.Printf("pocketcasts: Phase A: indexed %d in-progress episode(s)\n", len(inProgress))

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

		added := 0
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
				// http→https, etc.).
				resolvedUUID, resolveErr := ResolveFeedToPodcastUUID(ctx, originalFeedURL)
				if resolveErr == nil {
					if _, isSubscribed := podUUIDToFeedURL[resolvedUUID]; isSubscribed {
						podUUID = resolvedUUID
						// Use the Apple URL for index keys: findInIndex always uses
						// the Apple episode's FeedURL, so addToIndex must match.
						indexFeedURL = originalFeedURL
						fmt.Printf("  resolved feed URL mismatch for %q — PC UUID matched via refresh API\n", podTitle)
					}
				}

				// Strategy 2: title-based match.  Private/subscriber feeds (e.g.
				// NYT "The Daily - Subscriber Feed (🔓 for you@…)") have
				// personalised URLs the refresh API doesn't know. Normalise the
				// source podcast title and look it up in the PC subscription list.
				if podUUID == "" {
					srcTitle := feedToTitle[originalFeedURL]
					if normTitle := model.NormalizePlusTitle(srcTitle); normTitle != "" {
						if uuid := normTitleToPodUUID[normTitle]; uuid != "" {
							podUUID = uuid
							indexFeedURL = originalFeedURL
							fmt.Printf("  matched %q to PC podcast by title (private/subscriber feed)\n", podTitle)
						}
					}
				}

				if podUUID == "" {
					fmt.Printf("  skipping %q: could not resolve to a subscribed PC podcast\n", podTitle)
					skipped++
					continue
				}
			}

			for page := 0; page < maxPhaseBPagesPerPodcast; page++ {
				pageEps, hasMore, err := fetchPodcastEpisodesWithRetry(ctx, client, podUUID, page, requestDelay)
				if err != nil {
					fmt.Printf("  warning: could not fetch episodes for %q (page %d): %v\n", podTitle, page, err)
					break
				}

				beforeAdd := len(index)
				for _, ep := range pageEps {
					if indexFeedURL == "" || ep.IsDeleted {
						continue
					}
					addToIndex(index, &ep, indexFeedURL)
				}
				added += len(index) - beforeAdd

				if !hasMore {
					break // all pages exhausted
				}
				time.Sleep(requestDelay)
			}
		}

		if added > 0 {
			fmt.Printf("pocketcasts: Phase B: added %d additional episode(s) to index\n", added)
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
				writeLogLine(opts.LogWriter, "not_found", podTitle, ep.Title, ep.PubDate,
					playStateLabel(ep.PlayState, ep.PlayPosition), "—",
					"no match found in Pocket Casts account")
				continue
			}
			if !opts.ForceUpdate {
				if reason := pcSkipReason(ep, entry); reason != "" {
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
		if !opts.ForceUpdate && pcSkipReason(ep, entry) != "" {
			alreadyDone++
		} else {
			toUpdate++
		}
	}
	if alreadyDone > 0 {
		fmt.Printf("pocketcasts: skipping %d already-satisfied episode(s) (Pocket Casts state matches or exceeds source)\n",
			alreadyDone)
	}
	fmt.Printf("pocketcasts: request delay: %v between calls\n", requestDelay)
	fmt.Printf("pocketcasts: writing play state for %d episode(s)...\n", toUpdate)

	// --- Write loop ---
	updated := 0
	apiSkipped := 0
	skippedPlayed := 0
	skippedAhead := 0
	notFound := 0

	const (
		maxRateLimitRetries = 3
		maxTransientRetries = 3
		retryBaseDelay      = 2 * time.Second
	)

	for _, ep := range episodes {
		if ep.FromDestination || ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
			continue
		}
		podTitle := feedToTitle[ep.FeedURL]

		entry, ok := findInIndex(index, ep)
		if !ok {
			notFound++
			writeLogLine(opts.LogWriter, "not_found", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—",
				"episode not matched in Pocket Casts (not subscribed or not found in episode list)")
			continue
		}

		if !opts.ForceUpdate {
			if reason := pcSkipReason(ep, entry); reason != "" {
				switch reason {
				case "already_played":
					skippedPlayed++
				case "already_ahead":
					skippedAhead++
				}
				writeLogLine(opts.LogWriter, reason, podTitle, ep.Title, ep.PubDate,
					playStateLabel(ep.PlayState, ep.PlayPosition),
					playStateLabel(entry.currentState, entry.currentPos), "")
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

		// Retry loop: handles 429 (rate limit) and 5xx/network (transient) errors.
		var writeErr error
		rateLimitRetries := 0
		transientRetries := 0
		for {
			writeErr = UpdateEpisodeProgress(ctx, client,
				entry.episodeUUID, entry.podcastUUID,
				status, positionSec, durationSec)
			if writeErr == nil {
				break
			}

			var rl *RateLimitError
			if errors.As(writeErr, &rl) {
				if rateLimitRetries >= maxRateLimitRetries {
					break
				}
				rateLimitRetries++
				fmt.Printf("\n  rate limited (429) — pausing %v before retry...\n", rl.Wait)
				select {
				case <-ctx.Done():
					return updated, ctx.Err()
				case <-time.After(rl.Wait):
				}
				continue
			}

			var te *TransientError
			if errors.As(writeErr, &te) {
				if transientRetries >= maxTransientRetries {
					break
				}
				transientRetries++
				delay := retryBaseDelay * (1 << uint(transientRetries-1)) // 2s, 4s, 8s
				fmt.Printf("    transient error — retrying in %v (attempt %d/%d)...\n",
					delay, transientRetries, maxTransientRetries)
				select {
				case <-ctx.Done():
					return updated, ctx.Err()
				case <-time.After(delay):
				}
				continue
			}

			break // permanent error — don't retry
		}

		if writeErr != nil {
			fmt.Printf("  [%d/%d] FAILED %q: %v\n", updated+apiSkipped+1, toUpdate, ep.Title, writeErr)
			writeLogLine(opts.LogWriter, "error", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—", writeErr.Error())
			apiSkipped++
			continue
		}

		writeLogLine(opts.LogWriter, "updated", podTitle, ep.Title, ep.PubDate,
			playStateLabel(ep.PlayState, ep.PlayPosition),
			playStateLabel(entry.currentState, entry.currentPos), "")
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

	if skippedPlayed > 0 {
		fmt.Printf("pocketcasts: %d episode(s) skipped — already marked as played in Pocket Casts\n", skippedPlayed)
	}
	if skippedAhead > 0 {
		fmt.Printf("pocketcasts: %d episode(s) skipped — Pocket Casts position already at or ahead of source\n", skippedAhead)
	}
	if notFound > 0 {
		fmt.Printf("pocketcasts: %d episode(s) not matched in Pocket Casts (not subscribed or not found in episode list)\n", notFound)
	}
	if apiSkipped > 0 {
		fmt.Printf("pocketcasts: %d episode(s) failed during write (see warnings above)\n", apiSkipped)
	}
	return updated, nil
}

// addToIndex adds a Pocket Casts episode to the index under all applicable match keys.
// feedURL is the RSS feed URL of the parent podcast (resolved from podUUID → feedURL).
func addToIndex(index map[string]pcIndexEntry, ep *APIEpisode, feedURL string) {
	normFeed := normalizeFeedURL(feedURL)
	pubTime := ep.ParsePublishedAt()

	entry := pcIndexEntry{
		episodeUUID:  ep.UUID,
		podcastUUID:  ep.PodcastUUID,
		currentState: pcPlayingStatusToModel(ep.PlayingStatus),
		currentPos:   time.Duration(ep.PlayedUpTo) * time.Second,
	}

	// Primary: feed URL + pub date (second precision).
	if !pubTime.IsZero() && normFeed != "" {
		key := "feeddate:" + normFeed + "|" + pubTime.UTC().Format(time.RFC3339)
		if _, exists := index[key]; !exists {
			index[key] = entry
		}
	}
	// Fallback: feed URL + normalised title.
	if normFeed != "" && ep.Title != "" {
		key := "feedtitle:" + normFeed + "|" + strings.ToLower(strings.TrimSpace(ep.Title))
		if _, exists := index[key]; !exists {
			index[key] = entry
		}
	}
}

// findInIndex looks up an episode in the PC index using the same key priority
// as addToIndex: pub date + feed URL first, then title + feed URL.
func findInIndex(index map[string]pcIndexEntry, ep model.EpisodeState) (pcIndexEntry, bool) {
	normFeed := normalizeFeedURL(ep.FeedURL)
	if !ep.PubDate.IsZero() && ep.FeedURL != "" {
		key := "feeddate:" + normFeed + "|" + ep.PubDate.UTC().Format(time.RFC3339)
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	if ep.FeedURL != "" && ep.Title != "" {
		key := "feedtitle:" + normFeed + "|" + strings.ToLower(strings.TrimSpace(ep.Title))
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	return pcIndexEntry{}, false
}

// pcSkipReason returns "already_played", "already_ahead", or "" based on
// whether Pocket Casts' current state already satisfies the desired state.
func pcSkipReason(desired model.EpisodeState, current pcIndexEntry) string {
	return migrate.SkipReason(desired.PlayState, desired.PlayPosition, current.currentState, current.currentPos)
}

// pcPlayingStatusToModel converts a Pocket Casts playing_status integer to the
// model.PlayState enum used throughout this tool.
func pcPlayingStatusToModel(status int) model.PlayState {
	switch status {
	case PlayingPlayed:
		return model.PlayStatePlayed
	case PlayingInProgress:
		return model.PlayStateInProgress
	default:
		return model.PlayStateUnplayed
	}
}

// fetchPodcastEpisodesWithRetry calls FetchPodcastEpisodes with retry on
// rate-limit (429) responses. Up to 4 attempts with exponential back-off.
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

		var rl *RateLimitError
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
