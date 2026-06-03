package overcast

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// DefaultRequestDelay is the pause between consecutive Overcast API requests
// when WriteOptions.RequestDelay is not set. 1 s is conservative enough to
// avoid triggering Overcast's rate limiter, especially during the extended
// matching phase which fetches one page per subscribed podcast.
const DefaultRequestDelay = 1 * time.Second

// Provider implements provider.Provider for Overcast.
//
// Reading: parses the OPML export from overcast.fm/account/export_opml.
// Writing subscriptions: generates an OPML file the user imports via
//
//	Overcast > Settings > Import OPML.
//
// Writing play state: uses the unofficial Overcast web API (requires credentials).
// When email and password are set, the provider POSTs to the same set_progress
// endpoint used by the Overcast web player. This is unofficial and may break.
//
// The matching OPML (used to build the episode-ID index for play state writes) is
// resolved in this priority order:
//  1. matchOPMLPath (set via SetMatchOPMLPath / --overcast-match-opml) — explicit snapshot
//  2. Auto-fetched from overcast.fm/account/export_opml/extended after login — live state
type Provider struct {
	sourceOPMLPath string // path to Overcast OPML export used by GetLibrary (--overcast-source-opml)
	matchOPMLPath  string // path to OPML used for write-side episode matching (--overcast-match-opml, optional)
	exportOPMLPath string // destination path for generated import file (for subscription writes)
	email          string // Overcast account email (enables play state writes)
	password       string // Overcast account password
}

// NewProvider returns an Overcast provider without web API credentials.
// sourceOPMLPath is the path to an Overcast export file (for GetLibrary).
// exportOPMLPath is where the generated subscription import file will be written (for SetLibrary).
func NewProvider(sourceOPMLPath, exportOPMLPath string) *Provider {
	return &Provider{
		sourceOPMLPath: sourceOPMLPath,
		exportOPMLPath: exportOPMLPath,
	}
}

// NewProviderWithCredentials returns an Overcast provider that can also write episode
// play state using the unofficial Overcast web API.
//
// sourceOPMLPath is used by GetLibrary to read the source library; it does not need to
// point to the destination account's current state. The matching OPML for write-side
// episode resolution is either provided via SetMatchOPMLPath or auto-fetched from
// the live account after login.
func NewProviderWithCredentials(sourceOPMLPath, exportOPMLPath, email, password string) *Provider {
	return &Provider{
		sourceOPMLPath: sourceOPMLPath,
		exportOPMLPath: exportOPMLPath,
		email:          email,
		password:       password,
	}
}

// SetMatchOPMLPath sets an explicit OPML file to use as the destination matching index
// for play state writes. When set, this file is used instead of auto-fetching the live
// Overcast library. Equivalent to passing --overcast-match-opml on the command line.
func (p *Provider) SetMatchOPMLPath(path string) { p.matchOPMLPath = path }

func (p *Provider) Name() string { return "Overcast" }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ReadSubscriptions:  p.sourceOPMLPath != "",
		ReadPlayState:      p.sourceOPMLPath != "",
		WriteSubscriptions: p.exportOPMLPath != "",
		// Play state writes require credentials; the matching OPML is either provided
		// explicitly via SetMatchOPMLPath or auto-fetched from the live account after login.
		WritePlayState: p.email != "",
	}
}

func (p *Provider) GetLibrary(ctx context.Context) (*model.Library, error) {
	if p.sourceOPMLPath == "" {
		return nil, &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "read (no source OPML path configured — use --overcast-source-opml)",
		}
	}
	return NewOPMLReader(p.sourceOPMLPath).Read(ctx)
}

func (p *Provider) SetLibrary(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	// Only write subscriptions when an export path is configured and play-state-only
	// mode was not explicitly requested. When exportOPMLPath is empty we skip the
	// subscription write silently — the caller already knows WriteSubscriptions is
	// false via Capabilities() and has chosen not to provide a path.
	writeSubscriptions := !opts.OnlyPlayState && p.exportOPMLPath != ""
	writePlayState := !opts.OnlySubscriptions && p.email != ""

	// When OnlyPlayState is explicitly requested but no credentials are configured,
	// return a clear error rather than silently doing nothing.
	if opts.OnlyPlayState && p.email == "" {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write play state (no credentials configured — set OVERCAST_EMAIL and OVERCAST_PASSWORD, or use --overcast-email/--overcast-password)",
		}
	}

	// Guard against a no-op call: nothing to write.
	if !writeSubscriptions && !writePlayState {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write anything (no export OPML path and no credentials configured)",
		}
	}

	if writeSubscriptions {
		if opts.DryRun {
			fmt.Printf("[dry-run] would write %d subscriptions to %s\n",
				len(lib.Podcasts), p.exportOPMLPath)
		} else {
			if err := (&OPMLWriter{}).Write(lib, p.exportOPMLPath); err != nil {
				return err
			}
		}
	}

	if writePlayState {
		n, err := p.doWritePlayState(ctx, lib, opts)
		if err != nil {
			return err
		}
		prefix := ""
		if opts.DryRun {
			prefix = "[dry-run] "
		}
		fmt.Printf("%supdated play state for %d episode(s)\n", prefix, n)
	}

	return nil
}

// doWritePlayState matches lib's episodes against the Overcast matching library,
// then posts set_progress for each matched episode that has play state.
//
// The matching library (overcastLib) is resolved in this order:
//  1. matchOPMLPath (explicit file) — if set, read from disk; no login required for this step
//  2. Auto-fetch from overcast.fm/account/export_opml/extended — live account state
//
// Returns the number of episodes successfully updated.
func (p *Provider) doWritePlayState(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	// 1. Build feedURL → title and filter episode list (no I/O needed).
	feedToTitle := buildFeedToTitle(lib)
	episodes := filterEpisodesByPodcast(lib.Episodes, feedToTitle, opts.PodcastFilter)
	if len(opts.PodcastFilter) > 0 {
		fmt.Printf("overcast: podcast filter active — %q — %d/%d episode(s) in scope\n",
			opts.PodcastFilter, len(episodes), len(lib.Episodes))
	}
	writeLogHeader(opts.LogWriter)

	// Early exit: nothing to process.
	if len(episodes) == 0 {
		return 0, nil
	}

	// 2. Resolve the matching library:
	//    - Explicit matchOPMLPath: read from file (no login needed yet).
	//    - No matchOPMLPath: login and auto-fetch the live account library.
	var (
		matchLib  *model.Library
		httpClient *http.Client
		loginDone  bool
	)
	if p.matchOPMLPath != "" {
		var err error
		matchLib, err = NewOPMLReader(p.matchOPMLPath).Read(ctx)
		if err != nil {
			return 0, fmt.Errorf("overcast: read match OPML: %w", err)
		}
	} else {
		fmt.Printf("overcast: authenticating as %s...\n", p.email)
		var err error
		httpClient, err = Login(ctx, p.email, p.password)
		if err != nil {
			return 0, fmt.Errorf("overcast: authentication failed: %w", err)
		}
		loginDone = true
		fmt.Printf("overcast: fetching current library from account for episode matching...\n")
		matchLib, err = FetchExtendedOPML(ctx, httpClient)
		if err != nil {
			return 0, fmt.Errorf("overcast: fetch live OPML for matching: %w", err)
		}
		fmt.Printf("overcast: fetched %d podcast(s), %d episode(s) from live account\n",
			len(matchLib.Podcasts), len(matchLib.Episodes))
	}

	// 3. Build the episode-ID index from the resolved matching library.
	index := buildOvercastIndex(matchLib)

	// 4. Authenticate (if not already done during OPML auto-fetch above).
	//    This runs for both dry-run and live — augmentIndexFromPodcastPages (step 5)
	//    is read-only and must run in both modes to give an accurate dry-run preview.
	if !loginDone {
		fmt.Printf("overcast: authenticating as %s...\n", p.email)
		var err error
		httpClient, err = Login(ctx, p.email, p.password)
		if err != nil {
			return 0, fmt.Errorf("overcast: authentication failed: %w", err)
		}
	}

	// 5. Resolve the request delay: honour the caller's preference, or fall
	//    back to the conservative default.
	requestDelay := opts.RequestDelay
	if requestDelay <= 0 {
		requestDelay = DefaultRequestDelay
	}

	// 6. Augment the index with episode IDs from Overcast podcast pages.
	//    This handles episodes absent from the matching OPML (e.g. episodes the
	//    user listened to in Apple but never opened in Overcast). Runs in both
	//    dry-run and live mode: the page fetches are read-only GETs, so running
	//    them in dry-run gives an accurate preview of what the live run will do.
	added := augmentIndexFromPodcastPages(ctx, httpClient, matchLib, episodes, index, requestDelay, feedToTitle, opts.StrictFeedMatch)
	if added > 0 {
		fmt.Printf("overcast: extended matching added %d additional episode(s)\n", added)
	}

	// 7. In dry-run mode, report what would be written without making any state
	//    changes. The index is now fully populated (including extended matching),
	//    so not_found here means the episode genuinely has no match in the account.
	if opts.DryRun {
		n := 0
		for _, ep := range episodes {
			if ep.FromDestination || ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
				continue
			}
			podTitle := feedToTitle[ep.FeedURL]
			entry, ok := findInOvercastIndex(index, ep, opts.StrictFeedMatch)
			if !ok {
				writeLogLine(opts.LogWriter, "not_found", podTitle, ep.Title, ep.PubDate,
					playStateLabel(ep.PlayState, ep.PlayPosition), "—",
					"no match found in Overcast account")
				continue
			}
			if !opts.ForceUpdate {
				if reason := overcastSkipReason(ep, entry); reason != "" {
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

	// 8. Pre-count: how many episodes need an API call vs. are already satisfied.
	fmt.Printf("overcast: request delay: %v between calls\n", requestDelay)

	toUpdate := 0
	alreadyDone := 0
	for _, ep := range episodes {
		if ep.FromDestination || ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
			continue
		}
		entry, ok := findInOvercastIndex(index, ep, opts.StrictFeedMatch)
		if !ok {
			continue // unmatched — not in Overcast's history
		}
		if !opts.ForceUpdate && overcastSkipReason(ep, entry) != "" {
			alreadyDone++
		} else {
			toUpdate++
		}
	}
	if alreadyDone > 0 {
		fmt.Printf("overcast: skipping %d already-satisfied episode(s) (Overcast state matches or exceeds source)\n", alreadyDone)
	}
	fmt.Printf("overcast: writing play state for %d episode(s)...\n", toUpdate)

	// 9. For each episode with play state, look up its Overcast numeric ID and post
	//    set_progress. Failures on individual episodes are logged and skipped.
	updated := 0
	apiSkipped := 0
	skippedPlayed := 0
	skippedAhead := 0
	notFound := 0

	for _, ep := range episodes {
		if ep.FromDestination || ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
			continue
		}
		podTitle := feedToTitle[ep.FeedURL]

		entry, ok := findInOvercastIndex(index, ep, opts.StrictFeedMatch)
		if !ok {
			notFound++
			writeLogLine(opts.LogWriter, "not_found", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—",
				"episode not matched in Overcast (not subscribed or not found via extended matching)")
			continue
		}

		// Skip episodes that Overcast already has in the desired state, unless
		// ForceUpdate is set (caller wants to force a full re-sync).
		if !opts.ForceUpdate {
			if reason := overcastSkipReason(ep, entry); reason != "" {
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

		numericID := entry.numericID
		pos := int(ep.PlayPosition.Seconds())
		if ep.PlayState == model.PlayStatePlayed {
			pos = PlayedSentinel
		}

		// Retry loop: 429 (rate limit) and 5xx/network (transient) errors are
		// handled independently with separate attempt counters so neither can
		// exhaust the other's budget. Permanent 4xx errors (other than 429)
		// break immediately.
		const maxRateLimitRetries = 3
		const maxTransientRetries = 3
		const retryBaseDelay = 2 * time.Second

		var setErr error
		rateLimitRetries := 0
		transientRetries := 0
		for {
			setErr = SetProgress(ctx, httpClient, numericID, pos)
			if setErr == nil {
				break
			}

			var rl *RateLimitError
			if errors.As(setErr, &rl) {
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
			if errors.As(setErr, &te) {
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

			break // permanent error (4xx other than 429) — don't retry
		}
		if setErr != nil {
			fmt.Printf("  [%d/%d] FAILED %q (id=%s): %v\n",
				updated+apiSkipped+1, toUpdate, ep.Title, numericID, setErr)
			writeLogLine(opts.LogWriter, "error", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—", setErr.Error())
			apiSkipped++

			// If the first call already indicates an auth failure, abort immediately.
			if updated == 0 && apiSkipped == 1 && strings.Contains(setErr.Error(), "login") {
				return 0, fmt.Errorf("overcast: aborting — first set_progress call redirected to login page.\n" +
					"This usually means the password is wrong or the account requires 2FA.\n" +
					"Verify your credentials and try again")
			}
			continue
		}

		targetLabel := playStateLabel(ep.PlayState, ep.PlayPosition)
		writeLogLine(opts.LogWriter, "updated", podTitle, ep.Title, ep.PubDate,
			playStateLabel(ep.PlayState, ep.PlayPosition), targetLabel, "")
		updated++

		// Log the first 5 successes and then every 50th so the user can verify
		// things are working without drowning in lines.
		if updated <= 5 || updated%50 == 0 {
			posStr := fmt.Sprintf("%ds", pos)
			if ep.PlayState == model.PlayStatePlayed {
				posStr = "played"
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
		fmt.Printf("overcast: %d episode(s) skipped — already marked as played in Overcast\n", skippedPlayed)
	}
	if skippedAhead > 0 {
		fmt.Printf("overcast: %d episode(s) skipped — Overcast position already at or ahead of source\n", skippedAhead)
	}
	if notFound > 0 {
		fmt.Printf("overcast: %d episode(s) not matched in Overcast (not subscribed or not found via extended matching)\n", notFound)
	}
	if apiSkipped > 0 {
		fmt.Printf("overcast: %d episode(s) failed during write (see warnings above)\n", apiSkipped)
	}
	return updated, nil
}

// opmlPodInfo holds the Overcast podcast metadata used when matching Apple podcast
// feed URLs to their Overcast equivalents during extended episode matching.
type opmlPodInfo struct {
	title      string
	overcastID string
}

// buildOpmlTitleIndex builds a normalised-title → podcast-info lookup from the
// Overcast library. Two keys are added per podcast:
//
//  1. The exact lowercased title (e.g. "fresh air plus").
//  2. The Plus-normalised title (e.g. "fresh air"), so Apple episodes on the
//     public feed can match the paid Overcast subscription — and vice-versa.
//
// Earlier entries (index order) win on key collision.
func buildOpmlTitleIndex(lib *model.Library) map[string]opmlPodInfo {
	idx := make(map[string]opmlPodInfo, len(lib.Podcasts))
	for _, pod := range lib.Podcasts {
		if pod.Title == "" {
			continue
		}
		normTitle := strings.ToLower(strings.TrimSpace(pod.Title))
		info := opmlPodInfo{title: pod.Title, overcastID: pod.OvercastID}
		if _, exists := idx[normTitle]; !exists {
			idx[normTitle] = info
		}
		// Also index under the Plus-normalised base title so "Fresh Air" finds
		// "Fresh Air Plus" and "Fresh Air Plus" finds "Fresh Air".
		baseTitle := model.NormalizePlusTitle(pod.Title)
		if baseTitle != normTitle {
			if _, exists := idx[baseTitle]; !exists {
				idx[baseTitle] = info
			}
		}
	}
	return idx
}

// augmentIndexFromPodcastPages extends index with entries for Apple episodes whose
// Overcast numeric ID was not in the OPML export. It does this by:
//
//  1. Building a feedURL → OPML podcast lookup from overcastLib.
//  2. For each Apple feed with unmatched episodes, calling the Overcast search
//     JSON API (/podcasts/search_autocomplete) to find the podcast's iTunes ID,
//     then constructing the podcast listing page URL as /itunes{iTunesID}.
//  3. Fetching each podcast listing page to build a date → episode-hash map.
//  4. Resolving episode hashes to numeric IDs by fetching each episode page.
//
// The search API (structured JSON) replaces the previous /podcasts HTML scrape,
// which was fragile (regex against HTML) and only worked for iTunes-linked podcasts.
// Matching is by Overcast podcast ID (from the OPML overcastId attribute) first,
// then by exact title. This handles all subscribed podcasts regardless of their
// URL format (/itunes vs /p/).
//
// Entries are added directly into index (by feeddate key).
// Returns the number of new entries added.
func augmentIndexFromPodcastPages(
	ctx context.Context,
	client *http.Client,
	overcastLib *model.Library,
	episodes []model.EpisodeState,
	index map[string]overcastIndexEntry,
	requestDelay time.Duration,
	feedToTitle map[string]string, // Apple feedURL → lowercased podcast title (for title-based fallback)
	strictFeedMatch bool,
) int {
	// Build per-feed Apple episode set (only episodes with play state, by feed).
	appleByFeed := make(map[string][]model.EpisodeState)
	for _, ep := range episodes {
		if ep.PlayState != model.PlayStateUnplayed && ep.FeedURL != "" && !ep.PubDate.IsZero() {
			appleByFeed[ep.FeedURL] = append(appleByFeed[ep.FeedURL], ep)
		}
	}
	if len(appleByFeed) == 0 {
		return 0
	}

	// Step 1: build feedURL → OPML podcast info.
	// OvercastID (from the OPML overcastId attribute) is used to verify search results.

	// Primary lookup: normalised Overcast feed URL → podcast info.
	// Normalisation bridges minor differences (http vs https, trailing slash, host case)
	// between the URL Apple Podcasts has stored and the URL Overcast uses.
	opmlByNormFeed := make(map[string]opmlPodInfo, len(overcastLib.Podcasts))
	for _, pod := range overcastLib.Podcasts {
		if pod.FeedURL != "" {
			opmlByNormFeed[normalizeFeedURL(pod.FeedURL)] = opmlPodInfo{title: pod.Title, overcastID: pod.OvercastID}
		}
	}
	// Fallback lookup: normalised podcast title → podcast info.
	// Indexed with both exact and Plus-normalised keys (see buildOpmlTitleIndex).
	opmlByTitle := buildOpmlTitleIndex(overcastLib)

	// Count feeds that have episodes not yet in the index, for the progress log.
	unmatched := 0
	for feedURL, apEps := range appleByFeed {
		normFeed := normalizeFeedURL(feedURL)
		for _, ap := range apEps {
			key := "feeddate:" + normFeed + "|" + ap.PubDate.UTC().Format(time.RFC3339)
			if _, exists := index[key]; !exists {
				unmatched++
				break
			}
		}
	}
	if unmatched == 0 {
		return 0
	}
	fmt.Printf("overcast: extended matching: %d feed(s) have episodes not in OPML — resolving via /podcasts page...\n", unmatched)

	// Step 2: resolve each Apple feed to its Overcast podcast listing page URL.
	//
	// Primary: fetch /podcasts once to get every subscribed podcast's page URL.
	// This replaces N per-podcast search_autocomplete calls, handles private
	// subscriptions (no iTunes ID), and costs only one HTTP request.
	//
	// Fallback: if the /podcasts fetch fails or a podcast isn't found there,
	// fall back to the search_autocomplete API (original behaviour).
	pageURLByNormTitle := make(map[string]string) // normalised title → page URL
	subs, subsErr := FetchSubscribedPodcasts(ctx, client)
	if subsErr != nil {
		fmt.Printf("  warning: could not fetch /podcasts page (%v) — falling back to per-podcast search\n", subsErr)
	} else {
		for _, s := range subs {
			norm := strings.ToLower(strings.TrimSpace(s.Title))
			if _, exists := pageURLByNormTitle[norm]; !exists {
				pageURLByNormTitle[norm] = s.PageURL
			}
			// Also index under the Plus-normalised title so "Fresh Air Plus" matches
			// a source podcast titled "Fresh Air" (and vice-versa).
			if base := model.NormalizePlusTitle(s.Title); base != norm {
				if _, exists := pageURLByNormTitle[base]; !exists {
					pageURLByNormTitle[base] = s.PageURL
				}
			}
		}
		fmt.Printf("overcast: /podcasts page listed %d subscribed podcast(s)\n", len(subs))
	}

	feedToPageURL := make(map[string]string)
	skippedFeeds := 0
	for feedURL, apEps := range appleByFeed {
		normFeed := normalizeFeedURL(feedURL)
		// Only process feeds that have at least one episode not yet in the index.
		hasUnmatched := false
		for _, ap := range apEps {
			key := "feeddate:" + normFeed + "|" + ap.PubDate.UTC().Format(time.RFC3339)
			if _, exists := index[key]; !exists {
				hasUnmatched = true
				break
			}
		}
		if !hasUnmatched {
			continue
		}

		// Apple podcast title — used for /podcasts lookup and search query.
		appleTitle := feedToTitle[feedURL]
		if appleTitle == "" {
			skippedFeeds++
			continue
		}
		if strictFeedMatch {
			// Strict mode: only feed-URL-anchored matches are allowed.
			skippedFeeds++
			continue
		}

		// Step A: check the /podcasts page (subscribed podcasts).
		// If found, the podcast is already subscribed — use the page URL directly.
		if pageURL, found := pageURLByNormTitle[appleTitle]; found {
			feedToPageURL[feedURL] = pageURL
			continue
		}
		if base := model.NormalizePlusTitle(appleTitle); base != appleTitle {
			if pageURL, found := pageURLByNormTitle[base]; found {
				feedToPageURL[feedURL] = pageURL
				continue
			}
		}

		// Step B: not on /podcasts — fall back to search_autocomplete.
		// Use the overcastID from the OPML as a hint to verify the search result
		// (empty string is fine — search will fall back to title matching).
		overcastID := ""
		if info, ok := opmlByNormFeed[normFeed]; ok {
			overcastID = info.overcastID
		} else if info, ok := opmlByTitle[appleTitle]; ok {
			overcastID = info.overcastID
		}

		iTunesID, err := searchPodcastITunesIDWithRetry(ctx, client, appleTitle, overcastID, requestDelay)
		if err != nil {
			fmt.Printf("  warning: search failed for %q: %v\n", appleTitle, err)
			skippedFeeds++
			continue
		}
		if iTunesID == "" {
			fmt.Printf("  warning: %q not found in Overcast search\n", appleTitle)
			skippedFeeds++
			continue
		}
		pageURL := overcastBaseURL + "/itunes" + iTunesID
		time.Sleep(requestDelay)

		// Step C: subscribe before writing play state — Overcast discards set_progress
		// calls for podcasts the account is not subscribed to. SubscribeToPodcast is
		// idempotent: it returns nil immediately when the podcast is already subscribed
		// (unsubscribe form detected on the page).
		fmt.Printf("  subscribing to %q...\n", appleTitle)
		if err := SubscribeToPodcast(ctx, client, pageURL); err != nil {
			fmt.Printf("  warning: could not subscribe to %q: %v — play state may not persist\n",
				appleTitle, err)
		} else {
			fmt.Printf("  subscribed to %q\n", appleTitle)
		}
		time.Sleep(requestDelay)
		feedToPageURL[feedURL] = pageURL
	}

	// Step 3: for each shared feed, fetch the podcast page and collect
	// (feedURL, dateStr "YYYY-MM-DD", episodeURL) triples where the episode
	// is not already in the index.
	type pendingFetch struct {
		normFeedURL string // normalised feed URL — used as index key
		dateRFC3339 string
		episodeURL  string // "https://overcast.fm/+HASH"
	}
	var pending []pendingFetch

	// emptyPages collects podcast page URLs that returned 0 episode listings.
	// When non-empty after step 3, the page is very likely JavaScript-rendered
	// and the raw HTML has been saved to disk for inspection.
	var emptyPages []string

	matched := 0
	for feedURL, apEps := range appleByFeed {
		normFeed := normalizeFeedURL(feedURL)
		pageURL, ok := feedToPageURL[feedURL]
		if !ok {
			continue // not subscribed or search failed
		}

		listings, err := fetchPodcastEpisodesWithRetry(ctx, client, pageURL, requestDelay)
		if err != nil {
			fmt.Printf("  warning: could not fetch podcast page %s: %v\n", pageURL, err)
			continue
		}
		time.Sleep(requestDelay)

		if len(listings) == 0 {
			// No episode cells in the static HTML — the page is almost certainly
			// JavaScript-rendered. Record it and skip per-episode matching for this feed.
			emptyPages = append(emptyPages, pageURL)
			continue
		}

		// Build date → episode URL map for this podcast page.
		// Key is "YYYY-MM-DD"; Overcast pages use day-level precision.
		dateToURL := make(map[string]string, len(listings))
		// Also build a normalised-title list for fallback matching when date matching fails.
		// The cell body HTML may contain the podcast name before the episode title, so we
		// use strings.Contains (case-insensitive) rather than an exact map key lookup.
		type titleEntry struct {
			normTitle string
			url       string
		}
		var titleEntries []titleEntry
		for _, l := range listings {
			if _, exists := dateToURL[l.DateStr]; !exists {
				dateToURL[l.DateStr] = l.OvercastURL
			}
			if l.Title != "" {
				titleEntries = append(titleEntries, titleEntry{
					normTitle: strings.ToLower(l.Title),
					url:       l.OvercastURL,
				})
			}
		}

		for _, ap := range apEps {
			dateKey := "feeddate:" + normFeed + "|" + ap.PubDate.UTC().Format(time.RFC3339)
			if _, exists := index[dateKey]; exists {
				continue // already have the numeric ID from OPML
			}
			// Try to find the episode by the date portion of its UTC pubDate (±1 day tolerance).
			apDate := ap.PubDate.UTC().Format("2006-01-02")
			epURL := dateToURL[apDate]
			if epURL == "" {
				epURL = dateToURL[ap.PubDate.UTC().AddDate(0, 0, -1).Format("2006-01-02")]
			}
			if epURL == "" {
				epURL = dateToURL[ap.PubDate.UTC().AddDate(0, 0, 1).Format("2006-01-02")]
			}
			// Fallback: title-based matching when date matching fails.
			// This handles episodes where the pubDate stored in Apple Podcasts doesn't
			// align with the date Overcast shows on the podcast page (e.g. timezone
			// differences in the RSS feed's pubDate field).
			// Skipped when strictFeedMatch is true: only date-anchored results allowed.
			if epURL == "" && !strictFeedMatch && ap.Title != "" {
				normAppleTitle := strings.ToLower(strings.TrimSpace(ap.Title))
				// Exact match first (cell text == episode title).
				for _, te := range titleEntries {
					if te.normTitle == normAppleTitle {
						epURL = te.url
						break
					}
				}
				// Broader contains-match: cell text may include podcast name prefix.
				if epURL == "" {
					for _, te := range titleEntries {
						if strings.Contains(te.normTitle, normAppleTitle) {
							epURL = te.url
							break
						}
					}
				}
				if epURL != "" {
					fmt.Printf("  title match: %q (date match failed)\n", ap.Title)
				}
			}
			if epURL == "" {
				continue
			}
			pending = append(pending, pendingFetch{normFeed, ap.PubDate.UTC().Format(time.RFC3339), epURL})
			matched++
		}
	}

	// Report JS-rendered pages. The raw HTML of each was saved to disk by
	// FetchPodcastEpisodes when it found 0 episode cells, so it can be inspected
	// to find the actual API endpoint or embedded JSON the JS page uses.
	if len(emptyPages) > 0 {
		debugHTML := filepath.Join(os.TempDir(), "overcast-podcast-page-debug.html")
		fmt.Printf("overcast: %d podcast page(s) returned 0 episode listings (JavaScript-rendered):\n", len(emptyPages))
		for _, p := range emptyPages {
			fmt.Printf("  %s\n", p)
		}
		fmt.Printf("  raw HTML of last fetched page saved to: %s\n", debugHTML)
		fmt.Printf("  inspect that file to find the episode-list API endpoint or embedded JSON.\n")
	}

	if skippedFeeds > 0 {
		fmt.Printf("overcast: extended matching: %d feed(s) not found in Overcast or search (not subscribed / no iTunes ID)\n", skippedFeeds)
	}
	if len(pending) == 0 {
		fmt.Printf("overcast: extended matching found no additional episodes\n")
		return 0
	}
	fmt.Printf("overcast: fetching numeric IDs for %d additional episode(s) via episode pages...\n", len(pending))

	// Step 4: resolve hashes → numeric IDs sequentially with retry on 429.
	// Sequential (one request at a time) keeps us well within Overcast's
	// undocumented rate limit. At ~300ms per request, 10 K episodes ≈ 50 min.
	const maxFetchRetries = 4

	added := 0
	consecutiveErrors := 0
	for i, item := range pending {
		var numericID string
		for attempt := 0; attempt < maxFetchRetries; attempt++ {
			if attempt > 0 {
				// Exponential backoff: 30 s, 60 s, 120 s.
				wait := time.Duration(1<<uint(attempt)) * 30 * time.Second
				fmt.Printf("  rate limited — waiting %v before retry (attempt %d/%d)...\n",
					wait, attempt+1, maxFetchRetries)
				select {
				case <-ctx.Done():
					return added
				case <-time.After(wait):
				}
			}

			id, err := FetchEpisodeNumericID(ctx, client, item.episodeURL)
			if err != nil {
				var rl *RateLimitError
				if errors.As(err, &rl) {
					fmt.Printf("  rate limited (429) — waiting %v\n", rl.Wait)
					select {
					case <-ctx.Done():
						return added
					case <-time.After(rl.Wait):
					}
					continue // retry same attempt slot
				}
				// Log the first few individual errors so the root cause is visible.
				if consecutiveErrors < 3 {
					fmt.Printf("  error fetching %s: %v\n", item.episodeURL, err)
				}
				consecutiveErrors++
				if consecutiveErrors > 10 {
					fmt.Printf("  too many consecutive errors, stopping extended matching\n")
					return added
				}
				break // non-rate-limit error — skip this episode
			}
			numericID = id
			consecutiveErrors = 0
			break
		}

		if numericID != "" {
			key := "feeddate:" + item.normFeedURL + "|" + item.dateRFC3339
			if _, exists := index[key]; !exists {
				index[key] = overcastIndexEntry{numericID: numericID}
				added++
			}
		}

		if (i+1)%200 == 0 {
			fmt.Printf("  resolved %d/%d episode IDs (%d added to index)\n",
				i+1, len(pending), added)
		}

		time.Sleep(requestDelay)
	}

	_ = matched // matched is the upper bound; added is the confirmed count
	return added
}

// overcastIndexEntry holds the data needed to update an episode's progress in Overcast.
// currentState is populated from the OPML export so we can skip no-op API calls.
type overcastIndexEntry struct {
	numericID    string            // overcastId value, e.g. "2891974064154832"
	currentState model.PlayState   // play state already in Overcast (from OPML)
	currentPos   time.Duration     // current playback position in Overcast (from OPML)
}

// buildOvercastIndex creates a lookup map from match keys to Overcast episode data.
// Each Overcast episode is indexed by pubDate+feedURL (primary) and title+feedURL (fallback).
// The GUID key is intentionally omitted: overcastId ≠ RSS GUID, so GUID keys from Apple
// Podcasts will never match an overcastId.
func buildOvercastIndex(lib *model.Library) map[string]overcastIndexEntry {
	index := make(map[string]overcastIndexEntry)
	for _, ep := range lib.Episodes {
		if ep.GUID == "" {
			continue // need overcastId to call set_progress
		}
		// ep.GUID stores the overcastId value (confirmed == data-item-id for set_progress).
		entry := overcastIndexEntry{
			numericID:    ep.GUID,
			currentState: ep.PlayState,
			currentPos:   ep.PlayPosition,
		}

		// Primary key: pubDate + feedURL (most precise).
		// Feed URLs are normalised (http→https, host lowercase, no trailing slash) so
		// that minor differences between Apple Podcasts and Overcast don't break matching.
		if !ep.PubDate.IsZero() && ep.FeedURL != "" {
			key := "feeddate:" + normalizeFeedURL(ep.FeedURL) + "|" + ep.PubDate.UTC().Format(time.RFC3339)
			if _, exists := index[key]; !exists {
				index[key] = entry
			}
		}
		// Fallback key: normalized title + feedURL.
		if ep.FeedURL != "" && ep.Title != "" {
			key := "feedtitle:" + normalizeFeedURL(ep.FeedURL) + "|" + strings.ToLower(strings.TrimSpace(ep.Title))
			if _, exists := index[key]; !exists {
				index[key] = entry
			}
		}
	}
	return index
}

// searchPodcastITunesIDWithRetry calls SearchPodcastITunesID with a single 429 retry.
// On rate-limit, it waits the Retry-After period then tries once more.
func searchPodcastITunesIDWithRetry(ctx context.Context, client *http.Client, title, overcastID string, requestDelay time.Duration) (string, error) {
	id, err := SearchPodcastITunesID(ctx, client, title, overcastID)
	if err == nil {
		return id, nil
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		return "", err
	}
	wait := rl.Wait
	if wait < requestDelay {
		wait = requestDelay
	}
	fmt.Printf("  rate limited searching for %q — waiting %v...\n", title, wait)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(wait):
	}
	return SearchPodcastITunesID(ctx, client, title, overcastID)
}

// fetchPodcastEpisodesWithRetry calls FetchPodcastEpisodes with exponential backoff
// on 429 responses. It attempts up to 4 times with delays of 30 s, 60 s, and 120 s.
func fetchPodcastEpisodesWithRetry(ctx context.Context, client *http.Client, pageURL string, requestDelay time.Duration) ([]PodcastEpisodeListing, error) {
	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 30 s, 60 s, 120 s.
			wait := time.Duration(1<<uint(attempt)) * 30 * time.Second
			fmt.Printf("  rate limited fetching podcast page — waiting %v before retry (attempt %d/%d)...\n",
				wait, attempt+1, maxAttempts)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		listings, err := FetchPodcastEpisodes(ctx, client, pageURL)
		if err == nil {
			return listings, nil
		}

		var rl *RateLimitError
		if errors.As(err, &rl) {
			wait := rl.Wait
			if attempt == 0 {
				// First 429: use Retry-After, then continue to exponential backoff.
				fmt.Printf("  rate limited fetching podcast page — waiting %v...\n", wait)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(wait):
				}
			}
			lastErr = err
			continue
		}
		// Non-rate-limit error: give up immediately.
		return nil, err
	}
	return nil, lastErr
}

// findInOvercastIndex looks up an episode by pubDate+feedURL, then title+feedURL
// (unless strictFeedMatch is true, in which case only the exact-date strategy is tried).
// Returns the index entry and whether a match was found.
func findInOvercastIndex(index map[string]overcastIndexEntry, ep model.EpisodeState, strictFeedMatch bool) (overcastIndexEntry, bool) {
	// Normalise the Apple feed URL so it matches the normalised Overcast feed URL
	// stored in the index, bridging minor differences (http vs https, trailing slash).
	normFeed := normalizeFeedURL(ep.FeedURL)
	if !ep.PubDate.IsZero() && ep.FeedURL != "" {
		key := "feeddate:" + normFeed + "|" + ep.PubDate.UTC().Format(time.RFC3339)
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	// Title-based fallback: same feed URL, different pub date.  Skipped when
	// strictFeedMatch is true (caller wants only exact date+URL matches).
	if !strictFeedMatch && ep.FeedURL != "" && ep.Title != "" {
		key := "feedtitle:" + normFeed + "|" + strings.ToLower(strings.TrimSpace(ep.Title))
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	return overcastIndexEntry{}, false
}

// buildFeedToTitle returns a map from feed URL to lowercased podcast title, built
// from lib.Podcasts. Used to resolve which podcast an episode belongs to for filtering.
func buildFeedToTitle(lib *model.Library) map[string]string {
	m := make(map[string]string, len(lib.Podcasts))
	for _, pod := range lib.Podcasts {
		if pod.FeedURL != "" {
			m[pod.FeedURL] = strings.ToLower(strings.TrimSpace(pod.Title))
		}
	}
	return m
}

// filterEpisodesByPodcast returns the subset of episodes whose podcast title
// (looked up via feedToTitle) contains at least one of the filter strings
// (case-insensitive). If filters is empty, all episodes are returned unchanged.
func filterEpisodesByPodcast(episodes []model.EpisodeState, feedToTitle map[string]string, filters []string) []model.EpisodeState {
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
		title := feedToTitle[ep.FeedURL] // already lowercased
		for _, f := range lower {
			if f != "" && strings.Contains(title, f) {
				out = append(out, ep)
				break
			}
		}
	}
	return out
}

// normalizeFeedURL returns a canonical form of a podcast feed URL used as a
// matching key when comparing Apple Podcasts and Overcast feed URLs. It:
//   - lowercases scheme and host (RFC 3986 requires these to be case-insensitive)
//   - promotes http to https (treating the two schemes as equivalent for matching)
//   - strips a trailing slash from the path for canonical form
//   - drops the fragment (never meaningful for feed identity)
//
// Query parameters are preserved because some feeds use them as part of their
// identity (e.g. ?feed=rss2). Apple's cache-buster params (?t=...) are already
// stripped by cleanFeedURL before an Apple URL reaches this function.
//
// This function is used only for matching keys, never for making HTTP requests.
func normalizeFeedURL(raw string) string {
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

// overcastSkipReason returns the log status string when Overcast's current state
// for an episode already matches or exceeds what we want to write — either
// "already_played" (Overcast has it as completed) or "already_ahead" (in-progress
// position is at or beyond the desired position). Returns "" when we should write.
//
// These map directly to the statuses used by the Apple target writer so that
// --log-file produces consistent output regardless of direction.
func overcastSkipReason(desired model.EpisodeState, current overcastIndexEntry) string {
	switch desired.PlayState {
	case model.PlayStatePlayed:
		if current.currentState == model.PlayStatePlayed {
			return "already_played"
		}
	case model.PlayStateInProgress:
		if current.currentState == model.PlayStatePlayed {
			return "already_played" // Overcast is ahead
		}
		if current.currentState == model.PlayStateInProgress && current.currentPos >= desired.PlayPosition {
			return "already_ahead"
		}
	}
	return ""
}

// overcastAlreadySatisfied reports whether Overcast's current state for an episode
// already matches or exceeds what we want to write, making the set_progress call a no-op.
//
// Played beats everything. For in-progress, we skip if Overcast is already at
// or ahead of the desired position.
func overcastAlreadySatisfied(desired model.EpisodeState, current overcastIndexEntry) bool {
	return overcastSkipReason(desired, current) != ""
}
