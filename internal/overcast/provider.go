package overcast

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tyler/podcast-migrate/internal/httputil"
	"github.com/tyler/podcast-migrate/internal/itunes"
	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// DefaultRequestDelayPlayState is the default pause between Overcast API calls
// during a play-state write. Subscribes are interspersed with episode fetches
// so the effective subscribe rate is low; 3 s gives comfortable headroom.
const DefaultRequestDelayPlayState = 3 * time.Second

// DefaultRequestDelaySubscriptions is the default pause between subscribe
// operations during an --only-subscriptions run. Each subscribe makes two
// back-to-back Overcast requests (GET listing page + POST add); a 5 s gap
// provides enough spacing to avoid Overcast's bulk-subscribe rate limiter.
const DefaultRequestDelaySubscriptions = 5 * time.Second

// DefaultRequestDelay is the fallback when neither mode-specific constant
// applies (e.g. OPML export paths that don't enter the subscribe loop).
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
	sourceOPMLPath       string // path to Overcast OPML export used by GetLibrary (--overcast-source-opml)
	matchOPMLPath        string // path to OPML used for write-side episode matching (--overcast-match-opml, optional)
	exportOPMLPath       string // destination path for generated import file (for subscription writes)
	skippedOPMLPath      string // if non-empty, write skipped-private-feeds OPML here after a destination write
	email                string // Overcast account email (enables play state writes and auto-fetch)
	password             string // Overcast account password
	clearSourceOPMLCache bool   // if true, bypass the on-disk cache and re-fetch source OPML from the live account
	saveSourceOPMLPath   string // if non-empty, save a copy of the fetched source OPML to this path after download
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

// SetClearSourceOPMLCache configures GetLibrary to bypass the on-disk source OPML cache
// and re-fetch from the live account even if a fresh cached copy exists.
func (p *Provider) SetClearSourceOPMLCache(clear bool) { p.clearSourceOPMLCache = clear }

// SetSkippedOPMLPath configures the path where an OPML of skipped private feeds is written
// after a destination write. The file is only created when at least one podcast could not be
// subscribed (no iTunes ID in Overcast search). Pass "" to disable (the default).
// The default output path when the flag is given without a value is "skipped-private-feeds.opml".
func (p *Provider) SetSkippedOPMLPath(path string) { p.skippedOPMLPath = path }

// writeSkippedOPML writes an OPML file containing feeds that could not be subscribed
// automatically (typically private or custom feeds with no iTunes ID). Prints a
// human-readable summary of what was written and how to import it in Overcast.
func (p *Provider) writeSkippedOPML(pods []model.Podcast, dryRun bool) {
	outPath := p.skippedOPMLPath
	if outPath == "" {
		outPath = "skipped-private-feeds.opml"
	}
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("\novercast: %d podcast(s) could not be subscribed automatically (no iTunes ID):\n", len(pods))
	for _, pod := range pods {
		fmt.Printf("  • %s (%s)\n", pod.Title, pod.FeedURL)
	}
	if dryRun {
		fmt.Printf("%sWould write skipped-feeds OPML to %s\n", prefix, outPath)
		fmt.Printf("  To subscribe: Overcast → Settings → Import OPML\n")
		return
	}
	lib := &model.Library{Podcasts: pods}
	if err := (&OPMLWriter{}).Write(lib, outPath); err != nil {
		fmt.Printf("overcast: warning: could not write skipped-feeds OPML: %v\n", err)
		return
	}
	fmt.Printf("overcast: skipped-feeds OPML written to %s\n", outPath)
	fmt.Printf("  To subscribe: Overcast → Settings → Import OPML\n")
}

// SetSaveSourceOPMLPath configures GetLibrary to write a copy of the fetched source OPML
// to the given path after downloading it. Has no effect when --overcast-source-opml is set
// (explicit file is used directly, no download occurs).
func (p *Provider) SetSaveSourceOPMLPath(path string) { p.saveSourceOPMLPath = path }

func (p *Provider) Name() string { return "Overcast" }

func (p *Provider) Capabilities() provider.Capabilities {
	// Reading is possible from an explicit OPML file or by auto-fetching when credentials
	// are available (GetLibrary handles both paths).
	canRead := p.sourceOPMLPath != "" || p.email != ""
	return provider.Capabilities{
		ReadSubscriptions:  canRead,
		ReadPlayState:      canRead,
		WriteSubscriptions: p.exportOPMLPath != "",
		// Play state writes require credentials; the matching OPML is either provided
		// explicitly via SetMatchOPMLPath or auto-fetched from the live account after login.
		WritePlayState: p.email != "",
	}
}

func (p *Provider) GetLibrary(ctx context.Context) (*model.Library, error) {
	// Explicit file path — read directly, no auth needed.
	if p.sourceOPMLPath != "" {
		return NewOPMLReader(p.sourceOPMLPath).Read(ctx)
	}

	// No explicit path — credentials required for auto-fetch.
	if p.email == "" || p.password == "" {
		return nil, &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "read (no source OPML — use --overcast-source-opml, or set OVERCAST_EMAIL and OVERCAST_PASSWORD for automatic fetch)",
		}
	}

	// Check the on-disk cache first (skip when clearSourceOPMLCache is set).
	if hit, _ := loadCachedSourceOPML(ctx, p.clearSourceOPMLCache); hit != nil {
		age := hit.age.Round(time.Minute)
		fmt.Printf("overcast: using cached source OPML (%s old)\n", age)
		return hit.lib, nil
	}

	// Login and fetch a fresh copy.
	fmt.Printf("overcast: authenticating as %s...\n", p.email)
	httpClient, err := Login(ctx, p.email, p.password)
	if err != nil {
		return nil, fmt.Errorf("overcast: authentication failed: %w", err)
	}

	fmt.Printf("overcast: fetching extended OPML from live account...\n")
	rawOPML, err := FetchRawExtendedOPML(ctx, httpClient)
	if err != nil {
		return nil, fmt.Errorf("overcast: fetch source OPML: %w", err)
	}

	// Persist to local cache so subsequent runs within 24h skip the download.
	cachePath := sourceOPMLCachePath()
	if err := saveRawSourceOPML(rawOPML); err != nil {
		fmt.Printf("overcast: warning: could not cache source OPML (%v); parsing from memory\n", err)
		return parseOPMLBytes(rawOPML)
	}

	// Optionally save a copy to the user-specified path.
	if p.saveSourceOPMLPath != "" {
		if err := os.WriteFile(p.saveSourceOPMLPath, rawOPML, 0644); err != nil {
			fmt.Printf("overcast: warning: could not save OPML copy to %s: %v\n", p.saveSourceOPMLPath, err)
		} else {
			fmt.Printf("overcast: saved OPML copy to %s\n", p.saveSourceOPMLPath)
		}
	}

	lib, err := NewOPMLReader(cachePath).Read(ctx)
	if err != nil {
		return parseOPMLBytes(rawOPML)
	}
	fmt.Printf("overcast: fetched %d podcast(s), %d episode(s) from live account\n",
		len(lib.Podcasts), len(lib.Episodes))
	return lib, nil
}

func (p *Provider) SetLibrary(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	// Subscription writes have two paths:
	//   - OPML export (--overcast-out): always available, no credentials needed.
	//   - API subscribe (credentials set, no --overcast-out): programmatically
	//     subscribes each podcast via the /itunes{ID} or search_autocomplete path.
	//     This is the right path for --only-subscriptions when the user has Overcast
	//     credentials configured and does not want a manual OPML import step.
	writeSubscriptionsOPML := !opts.OnlyPlayState && p.exportOPMLPath != ""
	writeSubscriptionsAPI  := !opts.OnlyPlayState && p.email != "" && p.exportOPMLPath == ""
	writeSubscriptions := writeSubscriptionsOPML || writeSubscriptionsAPI
	writePlayState := !opts.OnlySubscriptions && p.email != "" && p.exportOPMLPath == ""

	// Explicit mode guards: return clear errors when the requested mode has no path.
	if opts.OnlyPlayState && p.email == "" {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write play state (no credentials configured — set OVERCAST_EMAIL and OVERCAST_PASSWORD, or use --overcast-email/--overcast-password)",
		}
	}
	if opts.OnlySubscriptions && !writeSubscriptions {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write subscriptions (set OVERCAST_EMAIL and OVERCAST_PASSWORD for API subscribe, or provide --overcast-out for OPML export)",
		}
	}

	// Guard against a no-op call: nothing to write.
	if !writeSubscriptions && !writePlayState {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write anything (no export OPML path and no credentials configured)",
		}
	}

	if writeSubscriptionsOPML {
		if opts.DryRun {
			fmt.Printf("[dry-run] would write %d subscriptions to %s\n",
				len(lib.Podcasts), p.exportOPMLPath)
		} else {
			if err := (&OPMLWriter{}).Write(lib, p.exportOPMLPath); err != nil {
				return err
			}
		}
	}

	if writeSubscriptionsAPI {
		if err := p.doWriteSubscriptionsAPI(ctx, lib, opts); err != nil {
			return err
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

// doWriteSubscriptionsAPI subscribes to each podcast in lib via the Overcast web API.
// It is the credentials-based path for --only-subscriptions when --overcast-out is not set.
//
// For each podcast in lib:
//   - Already subscribed on Overcast (matched by normalised title from /podcasts) → skip
//   - Private/subscriber-edition (IsPrivate or model.IsSubscriberFeed) → skipped-feeds OPML
//   - iTunes ID available → GET /itunes{ID} (SubscribeToPodcast)
//   - Otherwise → search_autocomplete → AddPodcast
func (p *Provider) doWriteSubscriptionsAPI(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	requestDelay := opts.RequestDelay
	if requestDelay <= 0 {
		requestDelay = DefaultRequestDelaySubscriptions
	}

	fmt.Printf("overcast: authenticating as %s...\n", p.email)
	client, err := Login(ctx, p.email, p.password)
	if err != nil {
		return fmt.Errorf("overcast: authentication failed: %w", err)
	}
	time.Sleep(requestDelay)

	// Fetch current subscriptions to build an already-subscribed index.
	pageURLByNormTitle := make(map[string]string)
	subs, subsErr := FetchSubscribedPodcasts(ctx, client)
	if subsErr != nil {
		fmt.Printf("  warning: could not fetch /podcasts (%v) — will attempt to subscribe all\n", subsErr)
	} else {
		for _, s := range subs {
			norm := strings.ToLower(strings.TrimSpace(s.Title))
			if _, exists := pageURLByNormTitle[norm]; !exists {
				pageURLByNormTitle[norm] = s.PageURL
			}
			if base := model.NormalizePlusTitle(s.Title); base != norm {
				if _, exists := pageURLByNormTitle[base]; !exists {
					pageURLByNormTitle[base] = s.PageURL
				}
			}
		}
		fmt.Printf("overcast: %d podcast(s) already subscribed\n", len(subs))
		time.Sleep(requestDelay)
	}

	// Build private-flag and iTunes-ID maps from the source library.
	feedIsPrivate := make(map[string]bool, len(lib.Podcasts))
	feedToITunesID := make(map[string]string, len(lib.Podcasts))
	for _, pod := range lib.Podcasts {
		if pod.FeedURL == "" {
			continue
		}
		if pod.IsPrivate || model.IsSubscriberFeed(pod.Title, pod.FeedURL) {
			feedIsPrivate[pod.FeedURL] = true
		} else if pod.ITunesID != "" {
			feedToITunesID[pod.FeedURL] = pod.ITunesID
		}
	}

	var skippedPods []model.Podcast
	added := 0
	for _, pod := range lib.Podcasts {
		if pod.FeedURL == "" || pod.Title == "" {
			continue
		}
		normTitle := strings.ToLower(strings.TrimSpace(pod.Title))

		// Skip if already subscribed (exact or Plus-normalised title match).
		if _, found := pageURLByNormTitle[normTitle]; found {
			continue
		}
		if base := model.NormalizePlusTitle(pod.Title); base != normTitle {
			if _, found := pageURLByNormTitle[base]; found {
				continue
			}
		}

		// Private/subscriber feeds cannot be subscribed via iTunes page.
		if feedIsPrivate[pod.FeedURL] {
			fmt.Printf("  note: %q is a private/subscriber feed — will include in skipped-feeds OPML for manual import\n", pod.Title)
			skippedPods = append(skippedPods, pod)
			continue
		}

		// Resolve iTunes ID: from source library, then FindByHints.
		resolvedITunesID := feedToITunesID[pod.FeedURL]
		if resolvedITunesID == "" {
			if result, findErr := itunes.FindByHints(ctx, client, pod.Title, pod.FeedURL, pod.Author); findErr == nil && result.CollectionID > 0 {
				resolvedITunesID = fmt.Sprintf("%d", result.CollectionID)
			}
		}

		// When the iTunes ID is known, fetch the /itunes{ID} listing page and let
		// SubscribeToPodcast extract the Overcast internal ID from the page HTML.
		// The GET+POST are one logical operation; sleep once after it returns.
		// For podcasts with no iTunes ID, fall back to search_autocomplete.
		if resolvedITunesID != "" {
			if opts.DryRun {
				fmt.Printf("  [dry-run] would subscribe to %q\n", pod.Title)
				added++
				continue
			}
			fmt.Printf("  subscribing to %q...\n", pod.Title)
			pageURL := overcastBaseURL + "/itunes" + resolvedITunesID
			var subErr error
			for {
				subErr = SubscribeToPodcast(ctx, client, pageURL, requestDelay)
				if subErr == nil {
					break
				}
				var rl *httputil.RateLimitError
				if !errors.As(subErr, &rl) {
					break
				}
				if !promptRateLimit(rl, &requestDelay) {
					p.writeSkippedOPML(skippedPods, false)
					return nil
				}
			}
			if subErr != nil {
				fmt.Printf("  warning: could not subscribe to %q: %v\n", pod.Title, subErr)
			} else {
				fmt.Printf("  subscribed to %q\n", pod.Title)
				added++
			}
			time.Sleep(requestDelay)
			continue
		}

		// No iTunes ID — search_autocomplete fallback.
		if opts.DryRun {
			fmt.Printf("  [dry-run] would search and subscribe to %q\n", pod.Title)
			added++
			continue
		}
		searchResult, searchErr := searchPodcastWithRetry(ctx, client, normTitle, "", requestDelay)
		if searchErr != nil || searchResult.OvercastID == "" {
			fmt.Printf("  warning: %q not found in Overcast search — will include in skipped-feeds OPML\n", pod.Title)
			skippedPods = append(skippedPods, pod)
			continue
		}
		fmt.Printf("  subscribing to %q (search)...\n", pod.Title)
		time.Sleep(requestDelay)
		var subErr error
		for {
			subErr = AddPodcast(ctx, client, searchResult.OvercastID)
			if subErr == nil {
				break
			}
			var rl *httputil.RateLimitError
			if !errors.As(subErr, &rl) {
				break
			}
			if !promptRateLimit(rl, &requestDelay) {
				p.writeSkippedOPML(skippedPods, false)
				return nil
			}
		}
		if subErr != nil {
			fmt.Printf("  warning: could not subscribe to %q: %v\n", pod.Title, subErr)
		} else {
			fmt.Printf("  subscribed to %q\n", pod.Title)
			added++
		}
		time.Sleep(requestDelay)
	}

	prefix := ""
	if opts.DryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("%sovercast: subscriptions: %d added, %d private/skipped\n", prefix, added, len(skippedPods))
	if len(skippedPods) > 0 {
		p.writeSkippedOPML(skippedPods, opts.DryRun)
	}
	return nil
}

// promptRateLimit is called when repeated HTTP 429 responses exhaust the automatic
// retry budget. It prints a message, waits the Retry-After period, then prompts the
// user interactively: continue (optionally increasing requestDelay) or abort.
// Returns true if the caller should retry, false to abort.
func promptRateLimit(rl *httputil.RateLimitError, requestDelay *time.Duration) bool {
	fmt.Printf("\n  overcast: persistent rate limiting — still getting 429 after retries.\n")
	fmt.Printf("  Waiting %v as requested by server...\n", rl.Wait)
	time.Sleep(rl.Wait)

	suggested := *requestDelay + 500*time.Millisecond
	fmt.Printf("  Continue? [Y/n] (current delay %v; press Enter to also increase to %v): ",
		*requestDelay, suggested)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	line := strings.TrimSpace(strings.ToLower(scanner.Text()))

	if line == "n" || line == "no" {
		return false
	}
	if line == "" || line == "y" || line == "yes" {
		// Empty Enter = accept the suggested delay increase.
		if line == "" {
			*requestDelay = suggested
			fmt.Printf("  request delay increased to %v\n", *requestDelay)
		}
		return true
	}
	// Any other input: treat as "yes, no delay change".
	return true
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

	// 2. Resolve the request delay up front so it is available for post-login
	//    pauses and is consistent across all steps.
	requestDelay := opts.RequestDelay
	if requestDelay <= 0 {
		requestDelay = DefaultRequestDelayPlayState
	}

	// 3. Resolve the matching library:
	//    - Explicit matchOPMLPath: read from file (no login needed yet).
	//    - No matchOPMLPath: login and auto-fetch the live account library.
	var (
		matchLib   *model.Library
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
		// Pause after login before the first content request.
		time.Sleep(requestDelay)
		fmt.Printf("overcast: fetching current library from account for episode matching...\n")
		matchLib, err = FetchExtendedOPML(ctx, httpClient)
		if err != nil {
			return 0, fmt.Errorf("overcast: fetch live OPML for matching: %w", err)
		}
		fmt.Printf("overcast: fetched %d podcast(s), %d episode(s) from live account\n",
			len(matchLib.Podcasts), len(matchLib.Episodes))
		// Pause after OPML fetch so that the /podcasts request inside
		// augmentIndexFromPodcastPages (step 6) doesn't immediately follow
		// a live-OPML request back-to-back (login → OPML → /podcasts).
		time.Sleep(requestDelay)
	}

	// 4. Build the episode-ID index from the resolved matching library.
	index := buildOvercastIndex(matchLib)

	// 5. Authenticate (if not already done during OPML auto-fetch above).
	//    This runs for both dry-run and live — augmentIndexFromPodcastPages (step 6)
	//    is read-only and must run in both modes to give an accurate dry-run preview.
	if !loginDone {
		fmt.Printf("overcast: authenticating as %s...\n", p.email)
		var err error
		httpClient, err = Login(ctx, p.email, p.password)
		if err != nil {
			return 0, fmt.Errorf("overcast: authentication failed: %w", err)
		}
		// Pause after login before the first content request.
		time.Sleep(requestDelay)
	}

	// 6. Augment the index with episode IDs from Overcast podcast pages.
	//    augmentIndexFromPodcastPages fetches /podcasts, then up to one listing
	//    page per subscribed feed. The requestDelay is threaded through so it
	//    can pace all requests consistently.
	//    This handles episodes absent from the matching OPML (e.g. episodes the
	//    user listened to in Apple but never opened in Overcast). Runs in both
	//    dry-run and live mode: the page fetches are read-only GETs, so running
	//    them in dry-run gives an accurate preview of what the live run will do.
	//
	//    The episode ID cache is loaded here so that written-state records (set
	//    by the write loop below) are persisted alongside the numeric IDs after
	//    the run completes. The cache is passed into augmentation so that
	//    previously-written states populate currentState/currentPos for
	//    extended-matching entries — enabling the skip check to fire correctly
	//    when the same episode is synced a second time.
	idCache := loadEpisodeIDCache()
	if opts.ClearEpisodeCache {
		n := idCache.clear()
		if n > 0 {
			fmt.Printf("overcast: episode ID cache cleared (%d entries discarded)\n", n)
		}
	}
	defer idCache.save()
	// Build feedURL → private flag and feedURL → iTunes ID maps from the source
	// library. Private/subscriber feeds are flagged so augmentIndexFromPodcastPages
	// can skip the iTunes subscription path entirely — subscribing the public
	// counterpart in their place would be wrong. Their iTunes IDs are also excluded
	// from feedToITunesID for the same reason.
	feedIsPrivate := make(map[string]bool, len(lib.Podcasts))
	feedToITunesID := make(map[string]string, len(lib.Podcasts))
	for _, pod := range lib.Podcasts {
		if pod.FeedURL == "" {
			continue
		}
		if pod.IsPrivate || model.IsSubscriberFeed(pod.Title, pod.FeedURL) {
			feedIsPrivate[pod.FeedURL] = true
			continue // don't expose the iTunes ID — it belongs to the public feed
		}
		if pod.ITunesID != "" {
			feedToITunesID[pod.FeedURL] = pod.ITunesID
		}
	}

	added, skippedPrivate := augmentIndexFromPodcastPages(ctx, httpClient, matchLib, episodes, index, requestDelay, feedToTitle, feedToITunesID, feedIsPrivate, opts.StrictFeedMatch, opts.SubscribedOnly, opts.DryRun, opts.EpisodeCacheMaxAge, opts.ClearEpisodeCache, idCache)
	if added > 0 {
		fmt.Printf("overcast: extended matching added %d additional episode(s)\n", added)
	}
	if len(skippedPrivate) > 0 {
		p.writeSkippedOPML(skippedPrivate, opts.DryRun)
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

		var setErr error
		for {
			setErr = httputil.RetryFunc(ctx, func() error {
				return SetProgress(ctx, httpClient, numericID, pos)
			}, httputil.RetryOptions{})
			var rl *httputil.RateLimitError
			if !errors.As(setErr, &rl) {
				break // success or non-rate-limit error — exit inner loop
			}
			// RetryFunc exhausted its 429 budget. Prompt the user.
			if !promptRateLimit(rl, &requestDelay) {
				return updated, fmt.Errorf("overcast: aborted due to persistent rate limiting")
			}
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

		writeLogLine(opts.LogWriter, "updated", podTitle, ep.Title, ep.PubDate,
			playStateLabel(ep.PlayState, ep.PlayPosition),
			playStateLabel(entry.currentState, entry.currentPos), "")
		updated++

		// Record what we just wrote so subsequent runs skip re-writing the same state.
		// Only applicable for extended-matching entries (those found via podcast listing
		// pages / episode hash fetches); OPML-sourced entries already carry live state.
		if entry.episodeURL != "" {
			idCache.setWrittenState(entry.episodeURL, ep.PlayState, ep.PlayPosition)
		}

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
	feedToTitle map[string]string,    // Apple feedURL → lowercased podcast title (for title-based fallback)
	feedToITunesID map[string]string, // Apple feedURL → iTunes Store ID (skips search_autocomplete when set)
	feedIsPrivate map[string]bool,    // Apple feedURL → true when source pod is private/subscriber edition
	strictFeedMatch bool,
	subscribedOnly bool,
	dryRun bool,
	episodeCacheMaxAge time.Duration,
	clearEpisodeCache bool,
	idCache *episodeIDCache,
) (int, []model.Podcast) {
	// Guard against a zero/negative delay — callers such as tests may pass 0.
	if requestDelay <= 0 {
		requestDelay = DefaultRequestDelay
	}

	// skippedPods collects podcasts that couldn't be subscribed because Overcast
	// search returned no iTunes ID (typically private/custom feeds).
	var skippedPods []model.Podcast

	// Build per-feed Apple episode set (only episodes with play state, by feed).
	appleByFeed := make(map[string][]model.EpisodeState)
	for _, ep := range episodes {
		if ep.PlayState != model.PlayStateUnplayed && ep.FeedURL != "" && !ep.PubDate.IsZero() {
			appleByFeed[ep.FeedURL] = append(appleByFeed[ep.FeedURL], ep)
		}
	}
	if len(appleByFeed) == 0 {
		return 0, nil
	}

	// Build a numeric-ID → play-state index from the OPML matching library so that
	// extended-match entries can inherit their currentState even when the episode
	// ID cache is empty (first run). Episodes in the OPML that fail to match by
	// feed URL (source and destination use different feed URLs for the same podcast)
	// are found here instead, preventing unnecessary writes on subsequent runs.
	opmlByNumericID := buildOPMLNumericIDIndex(overcastLib)

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

	// Overcast→Overcast short-circuit: when the source is an Overcast OPML export,
	// each episode's GUID is already the Overcast numeric ID needed for SetProgress.
	// Pre-seed the index so those episodes don't trigger listing-page fetches.
	preSeeded := 0
	for feedURL, eps := range appleByFeed {
		normFeed := normalizeFeedURL(feedURL)
		for _, ep := range eps {
			if !isOvercastNumericID(ep.GUID) {
				continue
			}
			entry := overcastIndexEntry{numericID: ep.GUID}
			if !ep.PubDate.IsZero() {
				key := "feeddate:" + normFeed + "|" + ep.PubDate.UTC().Format(time.RFC3339)
				if _, exists := index[key]; !exists {
					index[key] = entry
					preSeeded++
				}
			}
			if ep.Title != "" {
				key := "feedtitle:" + normFeed + "|" + migrate.FuzzyNormalizeTitle(ep.Title)
				if _, exists := index[key]; !exists {
					index[key] = entry
				}
			}
		}
	}
	if preSeeded > 0 {
		fmt.Printf("overcast: %d episode(s) pre-matched from source Overcast IDs (no listing-page fetches needed)\n", preSeeded)
	}

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
		return 0, nil
	}
	fmt.Printf("overcast: extended matching: %d feed(s) have unmatched episodes — resolving via /podcasts page...\n", unmatched)

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
		// Pause after fetching /podcasts before the first podcast listing-page
		// request. Without this pause the listing-page loop starts immediately
		// after two back-to-back requests (login + /podcasts), making it easy
		// to trigger Overcast's rate limiter on the very first listing page.
		time.Sleep(requestDelay)
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

		// Step B: not on /podcasts — podcast is not subscribed at the destination.
		// When --subscribed-only is set, skip rather than searching and subscribing.
		if subscribedOnly {
			skippedFeeds++
			continue
		}

		// Step B1: skip private/subscriber-edition feeds — they have no iTunes page
		// on Overcast and subscribing to their public counterpart would be wrong.
		// Add them to the skipped-feeds OPML so the user can import the private URL
		// manually via Add Feed → URL.
		//
		// Detection uses two signals:
		//   - feedIsPrivate: set explicitly by source readers (e.g. Apple KVS when
		//     the KVS URL differs from the iTunes canonical, or PodcastPID == 0).
		//   - model.IsSubscriberFeed: heuristic for sources without an IsPrivate
		//     flag (Overcast source) based on title markers and platform domains.
		if feedIsPrivate[feedURL] || model.IsSubscriberFeed(appleTitle, feedURL) {
			if !subscribedOnly {
				fmt.Printf("  note: %q is a private/subscriber feed — will include in skipped-feeds OPML for manual import\n", appleTitle)
				skippedPods = append(skippedPods, model.Podcast{FeedURL: feedURL, Title: appleTitle})
				skippedFeeds++
			}
			continue
		}

		// Step B2: resolve the iTunes ID for this podcast so we can go directly
		// to its Overcast page (/itunes{ID}) without a search_autocomplete round-trip.
		// Priority: (1) iTunes ID from source library (KVSReader populates this for
		// Apple sources); (2) iTunes title search (covers Overcast/PC sources that
		// don't carry an iTunes ID); (3) fall through to search_autocomplete.
		resolvedITunesID := feedToITunesID[feedURL]
		if resolvedITunesID == "" && appleTitle != "" {
			if result, err := itunes.FindByHints(ctx, client, appleTitle, feedURL, ""); err == nil && result.CollectionID > 0 {
				resolvedITunesID = fmt.Sprintf("%d", result.CollectionID)
			}
		}
		if resolvedITunesID != "" {
			// Fetch the /itunes{ID} listing page, extract the Overcast internal
			// podcast ID from the embedded /podcasts/add/{id} path, and POST to
			// AddPodcast. The GET+POST are one logical operation; sleep once after.
			pageURL := overcastBaseURL + "/itunes" + resolvedITunesID
			if dryRun {
				fmt.Printf("  [dry-run] would subscribe to %q (iTunes ID %s)\n", appleTitle, resolvedITunesID)
			} else {
				fmt.Printf("  subscribing to %q (iTunes ID %s)...\n", appleTitle, resolvedITunesID)
				var subErr error
				for {
					subErr = SubscribeToPodcast(ctx, client, pageURL, requestDelay)
					if subErr == nil {
						break
					}
					var rl *httputil.RateLimitError
					if !errors.As(subErr, &rl) {
						break
					}
					if !promptRateLimit(rl, &requestDelay) {
						return 0, skippedPods
					}
				}
				if subErr != nil {
					fmt.Printf("  warning: could not subscribe to %q: %v — play state may not persist\n", appleTitle, subErr)
				} else {
					fmt.Printf("  subscribed to %q\n", appleTitle)
				}
				time.Sleep(requestDelay)
			}
			feedToPageURL[feedURL] = pageURL
			continue
		}

		// Fall back to search_autocomplete for private/unindexed feeds (no iTunes ID).
		// Use the overcastID from the OPML as a hint to verify the search result
		// (empty string is fine — search will fall back to title matching).
		hintOvercastID := ""
		if info, ok := opmlByNormFeed[normFeed]; ok {
			hintOvercastID = info.overcastID
		} else if info, ok := opmlByTitle[appleTitle]; ok {
			hintOvercastID = info.overcastID
		}

		var searchResult PodcastSearchResult
		searchSkip := false
		for {
			var err error
			searchResult, err = searchPodcastWithRetry(ctx, client, appleTitle, hintOvercastID, requestDelay)
			if err == nil {
				break
			}
			var rl *httputil.RateLimitError
			if !errors.As(err, &rl) {
				fmt.Printf("  warning: search failed for %q: %v\n", appleTitle, err)
				searchSkip = true
				break
			}
			if !promptRateLimit(rl, &requestDelay) {
				return 0, skippedPods
			}
		}
		if searchSkip {
			skippedFeeds++
			continue
		}
		if searchResult.ITunesID == "" {
			// Podcast not found in Overcast search — no iTunes ID means we cannot
			// construct a page URL or subscribe programmatically. Collect it for the
			// skipped-private-feeds OPML so the user can import it manually.
			fmt.Printf("  warning: %q not found in Overcast search (no iTunes ID) — will include in skipped-feeds OPML\n", appleTitle)
			skippedFeeds++
			skippedPods = append(skippedPods, model.Podcast{FeedURL: feedURL, Title: appleTitle})
			continue
		}
		pageURL := overcastBaseURL + "/itunes" + searchResult.ITunesID
		time.Sleep(requestDelay)

		// Step C: subscribe before writing play state — Overcast discards set_progress
		// calls for podcasts the account is not subscribed to. AddPodcast posts
		// directly to /podcasts/add/{overcastID}, bypassing the podcast listing page
		// and its caching bug (stale Delete button on unsubscribed podcasts).
		// In dry-run mode, log intent without posting.
		if dryRun {
			fmt.Printf("  [dry-run] would subscribe to %q\n", appleTitle)
		} else {
			fmt.Printf("  subscribing to %q...\n", appleTitle)
			if err := AddPodcast(ctx, client, searchResult.OvercastID); err != nil {
				fmt.Printf("  warning: could not subscribe to %q: %v — play state may not persist\n",
					appleTitle, err)
			} else {
				fmt.Printf("  subscribed to %q\n", appleTitle)
			}
			time.Sleep(requestDelay)
		}
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

	// podPageCache deduplicates podcast page fetches keyed by Overcast page URL.
	// Multiple Apple feed URLs (e.g. public feed + Plus feed) may resolve to the
	// same Overcast podcast page via Plus-normalised title matching; without this
	// cache the same page would be fetched once per Apple feed URL.
	type podPageResult struct {
		listings []PodcastEpisodeListing
		failed   bool // true = fetch returned an error
	}
	podPageCache := make(map[string]podPageResult)

	// added counts entries written into index (either directly from listing NumericID
	// in step 3, or resolved via per-episode page fetch in step 4).
	added := 0
	for feedURL, apEps := range appleByFeed {
		normFeed := normalizeFeedURL(feedURL)
		pageURL, ok := feedToPageURL[feedURL]
		if !ok {
			continue // not subscribed or search failed
		}

		var listings []PodcastEpisodeListing
		if cached, hit := podPageCache[pageURL]; hit {
			// Re-use a previously fetched result; skip this feed if the page
			// failed or returned no episode cells (JS-rendered).
			if cached.failed || len(cached.listings) == 0 {
				continue
			}
			listings = cached.listings
		} else {
			var fetched []PodcastEpisodeListing
			for {
				var err error
				fetched, err = fetchPodcastEpisodesWithRetry(ctx, client, pageURL, requestDelay)
				if err == nil {
					break
				}
				var rl *httputil.RateLimitError
				if !errors.As(err, &rl) {
					fmt.Printf("  warning: could not fetch podcast page %s: %v\n", pageURL, err)
					podPageCache[pageURL] = podPageResult{failed: true}
					fetched = nil
					break
				}
				if !promptRateLimit(rl, &requestDelay) {
					// User aborted; return what we have so far.
					return 0, skippedPods
				}
			}
			if fetched == nil {
				continue
			}
			time.Sleep(requestDelay)

			if len(fetched) == 0 {
				// No episode cells — page is almost certainly JavaScript-rendered.
				emptyPages = append(emptyPages, pageURL)
				podPageCache[pageURL] = podPageResult{} // mark as attempted, empty
				continue
			}
			listings = fetched
			podPageCache[pageURL] = podPageResult{listings: listings}
		}

		// Build date → listings and URL → listing maps for this podcast page.
		// Key is "YYYY-MM-DD"; Overcast pages use day-level precision.
		// Multiple episodes can share a date (e.g. a podcast that publishes two
		// episodes in the same day) so we store a slice per date.
		dateToListings := make(map[string][]PodcastEpisodeListing, len(listings))
		// urlToListing allows O(1) NumericID lookup once we have a URL match.
		urlToListing := make(map[string]PodcastEpisodeListing, len(listings))
		// Also build a normalised-title list for fallback matching when date matching fails.
		// The cell body HTML may contain the podcast name before the episode title, so we
		// use strings.Contains (case-insensitive) rather than an exact map key lookup.
		type titleEntry struct {
			normTitle string
			url       string
		}
		var titleEntries []titleEntry
		for _, l := range listings {
			dateToListings[l.DateStr] = append(dateToListings[l.DateStr], l)
			urlToListing[l.OvercastURL] = l
			if l.Title != "" {
				titleEntries = append(titleEntries, titleEntry{
					normTitle: migrate.FuzzyNormalizeTitle(l.Title),
					url:       l.OvercastURL,
				})
			}
		}

		// pickByDate tries all listings on a given date and returns the one whose
		// title is compatible with ap (or the first entry when titles are empty).
		// This handles podcasts that publish multiple episodes on the same day.
		pickByDate := func(dateStr string, ap model.EpisodeState) (PodcastEpisodeListing, bool) {
			candidates := dateToListings[dateStr]
			if len(candidates) == 0 {
				return PodcastEpisodeListing{}, false
			}
			if ap.Title == "" {
				return candidates[0], true
			}
			normAp := migrate.FuzzyNormalizeTitle(ap.Title)
			for _, c := range candidates {
				if c.Title == "" {
					continue
				}
				normC := migrate.FuzzyNormalizeTitle(c.Title)
				if strings.Contains(normAp, normC) || strings.Contains(normC, normAp) {
					return c, true
				}
			}
			return PodcastEpisodeListing{}, false
		}

		for _, ap := range apEps {
			dateKey := "feeddate:" + normFeed + "|" + ap.PubDate.UTC().Format(time.RFC3339)
			if _, exists := index[dateKey]; exists {
				continue // already have the numeric ID from OPML
			}
			// Try to find the episode by the date portion of its UTC pubDate (±1 day tolerance).
			// pickByDate checks all episodes on a given date and picks the one whose
			// title is compatible, handling podcasts that publish multiple episodes per day.
			apDate := ap.PubDate.Format("2006-01-02")
			var matched PodcastEpisodeListing
			if l, ok := pickByDate(apDate, ap); ok {
				matched = l
			} else if l, ok := pickByDate(ap.PubDate.AddDate(0, 0, -1).Format("2006-01-02"), ap); ok {
				matched = l
			} else if l, ok := pickByDate(ap.PubDate.AddDate(0, 0, 1).Format("2006-01-02"), ap); ok {
				matched = l
			}
			// Fallback: title-based matching when date matching fails.
			// This handles episodes where the pubDate stored in Apple Podcasts doesn't
			// align with the date Overcast shows on the podcast page (e.g. timezone
			// differences in the RSS feed's pubDate field).
			// Skipped when strictFeedMatch is true: only date-anchored results allowed.
			if matched.OvercastURL == "" && !strictFeedMatch && ap.Title != "" {
				normAppleTitle := migrate.FuzzyNormalizeTitle(ap.Title)
				// Exact fuzzy match first (handles season-marker variants like "S01").
				for _, te := range titleEntries {
					if te.normTitle == normAppleTitle {
						matched = urlToListing[te.url]
						break
					}
				}
				// Broader contains-match: cell text may include podcast name prefix.
				if matched.OvercastURL == "" {
					for _, te := range titleEntries {
						if strings.Contains(te.normTitle, normAppleTitle) {
							matched = urlToListing[te.url]
							break
						}
					}
				}
				if matched.OvercastURL != "" {
					fmt.Printf("  title match: %q (date match failed)\n", ap.Title)
				}
			}
			if matched.OvercastURL == "" {
				continue
			}
			// If the listing page already provided data-item-id, add to index directly —
			// no per-episode player-page fetch needed.
			if matched.NumericID != "" {
				key := "feeddate:" + normFeed + "|" + ap.PubDate.UTC().Format(time.RFC3339)
				if _, exists := index[key]; !exists {
					// Retrieve any previously written state (maxAge=0: written state
					// is always valid regardless of how old the ID cache entry is).
					_, writtenState, writtenPos := idCache.getEntry(matched.OvercastURL, 0)
					// Prefer the live OPML state over the written-state cache: the OPML
					// reflects current Overcast state, while the cache records what this
					// tool last wrote. Take whichever represents more listening progress.
					currState, currPos := furthestPlayState(
						writtenState, writtenPos,
						opmlByNumericID[matched.NumericID].state,
						opmlByNumericID[matched.NumericID].pos,
					)
					index[key] = overcastIndexEntry{
						numericID:    matched.NumericID,
						episodeURL:   matched.OvercastURL,
						currentState: currState,
						currentPos:   currPos,
					}
					added++
				}
				continue
			}
			pending = append(pending, pendingFetch{normFeed, ap.PubDate.UTC().Format(time.RFC3339), matched.OvercastURL})
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
		if subscribedOnly {
			fmt.Printf("overcast: extended matching: %d feed(s) skipped — not subscribed at destination (--subscribed-only)\n", skippedFeeds)
		} else {
			fmt.Printf("overcast: extended matching: %d feed(s) not found in Overcast or search (not subscribed / no iTunes ID)\n", skippedFeeds)
		}
	}
	if len(pending) == 0 {
		if added == 0 {
			fmt.Printf("overcast: extended matching found no additional episodes\n")
		}
		return added, skippedPods
	}

	// Step 4: resolve hashes → numeric IDs.
	//
	// Strategy:
	//  1. Check the persistent on-disk cache (idCache, managed by the caller).
	//     Episodes resolved in prior runs are served instantly with no HTTP request.
	//     Written-state records from prior writes also populate currentState/currentPos
	//     so the skip check in the write loop fires on repeated runs.
	//  2. Fetch remaining (cache misses) with a bounded pool of concurrent GETs.
	//
	// On the first sync the cache is empty and all episodes need fetching.
	// On every subsequent sync only new episodes require fetches.
	const (
		maxFetchRetries = 4
		maxFetchWorkers = 5  // concurrent episode-page GETs
		maxFetchErrors  = 10 // abort if this many goroutine errors accumulate
	)

	// Pass A: serve cache hits immediately.
	// Also populate currentState/currentPos from the written-state cache so the
	// write-side skip check fires on subsequent runs (episodes found via extended
	// matching have no OPML-sourced state, but after a successful write the cache
	// records what was written — preventing re-writes of the same play state).
	cacheHits := 0
	for _, item := range pending {
		id, writtenState, writtenPos := idCache.getEntry(item.episodeURL, episodeCacheMaxAge)
		if id == "" {
			continue
		}
		key := "feeddate:" + item.normFeedURL + "|" + item.dateRFC3339
		if _, exists := index[key]; !exists {
			currState, currPos := furthestPlayState(
				writtenState, writtenPos,
				opmlByNumericID[id].state, opmlByNumericID[id].pos,
			)
			index[key] = overcastIndexEntry{
				numericID:    id,
				episodeURL:   item.episodeURL,
				currentState: currState,
				currentPos:   currPos,
			}
			added++
			cacheHits++
		}
	}

	// Build the miss list (episodes absent from cache or with stale entries).
	var misses []pendingFetch
	for _, item := range pending {
		if idCache.get(item.episodeURL, episodeCacheMaxAge) == "" {
			misses = append(misses, item)
		}
	}

	if idCache.size() > 0 && !clearEpisodeCache {
		if cacheHits > 0 {
			fmt.Printf("overcast: episode ID cache: %d hits (no fetch needed), %d misses\n",
				cacheHits, len(misses))
		} else {
			fmt.Printf("overcast: episode ID cache: %d entries loaded, %d misses\n",
				idCache.size(), len(misses))
		}
	}

	if len(misses) == 0 {
		return added, skippedPods
	}

	fmt.Printf("overcast: fetching numeric IDs for %d episode(s) via episode pages (%d workers)...\n",
		len(misses), maxFetchWorkers)

	// Pass B: fetch cache misses concurrently.
	type fetchResult struct {
		key        string
		numericID  string
		episodeURL string // preserved for written-state cache updates
	}

	results := make(chan fetchResult, len(misses))
	sem := make(chan struct{}, maxFetchWorkers)
	var wg sync.WaitGroup

	var errMu sync.Mutex
	errCount := 0

	// Sub-context so we can cancel all workers if too many errors accumulate.
	fetchCtx, cancelFetch := context.WithCancel(ctx)
	defer cancelFetch()

	// Rate limiter: one request per requestDelay across all workers.
	// Episode-page GETs are serialised at the same cadence as listing-page
	// fetches in step 3 to avoid triggering Overcast's rate limiter, which
	// can lock out access for many hours once tripped.
	rateTicker := time.NewTicker(requestDelay)
	defer rateTicker.Stop()

	for _, item := range misses {
		item := item // capture loop variable
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Acquire a worker slot; respect cancellation while waiting.
			select {
			case sem <- struct{}{}:
			case <-fetchCtx.Done():
				results <- fetchResult{}
				return
			}
			defer func() { <-sem }()

			key := "feeddate:" + item.normFeedURL + "|" + item.dateRFC3339
			var numericID string

			for attempt := 0; attempt < maxFetchRetries; attempt++ {
				if attempt > 0 {
					// Exponential back-off between retry attempts: 30 s, 60 s, 120 s.
					wait := time.Duration(1<<uint(attempt)) * 30 * time.Second
					select {
					case <-fetchCtx.Done():
						results <- fetchResult{key: key}
						return
					case <-time.After(wait):
					}
				}

				// Acquire a rate-limit slot before every HTTP request — including
				// retries. This serialises all concurrent workers to one request
				// per requestDelay, matching the pacing of step 3's listing-page
				// fetches and avoiding Overcast's long-duration rate-limit lockout.
				select {
				case <-rateTicker.C:
				case <-fetchCtx.Done():
					results <- fetchResult{key: key}
					return
				}

				id, err := FetchEpisodeNumericID(fetchCtx, client, item.episodeURL)
				if err != nil {
					var rl *httputil.RateLimitError
					if errors.As(err, &rl) {
						select {
						case <-fetchCtx.Done():
							results <- fetchResult{key: key}
							return
						case <-time.After(rl.Wait):
						}
						continue // retry after Retry-After delay
					}
					// 5xx / network-level transient error — retry with the
					// exponential backoff already applied at the top of the loop
					// (30 s, 60 s, 120 s for attempts 1-3).
					var te *httputil.TransientError
					if errors.As(err, &te) {
						if attempt < maxFetchRetries-1 {
							fmt.Printf("  transient error fetching %s (%v) — will retry\n", item.episodeURL, err)
						}
						continue
					}
					// Permanent error: log the first few and abort if persistent.
					errMu.Lock()
					errCount++
					n := errCount
					errMu.Unlock()
					if n <= 3 {
						fmt.Printf("  error fetching %s: %v\n", item.episodeURL, err)
					}
					if n > maxFetchErrors {
						fmt.Printf("  too many errors (%d), stopping extended matching\n", n)
						cancelFetch()
					}
					results <- fetchResult{key: key}
					return
				}
				numericID = id
				break
			}

			if numericID != "" {
				idCache.set(item.episodeURL, numericID)
			}
			results <- fetchResult{key, numericID, item.episodeURL}
		}()
	}

	// Close results channel once all workers finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results on the main goroutine (index map is not thread-safe).
	resolved := 0
	for r := range results {
		resolved++
		if r.numericID != "" && r.key != "" {
			if _, exists := index[r.key]; !exists {
				// Retrieve any previously written state from the cache so the
				// skip check in the write loop fires on subsequent runs.
				_, writtenState, writtenPos := idCache.getEntry(r.episodeURL, 0)
				currState, currPos := furthestPlayState(
					writtenState, writtenPos,
					opmlByNumericID[r.numericID].state, opmlByNumericID[r.numericID].pos,
				)
				index[r.key] = overcastIndexEntry{
					numericID:    r.numericID,
					episodeURL:   r.episodeURL,
					currentState: currState,
					currentPos:   currPos,
				}
				added++
			}
		}
		if resolved%200 == 0 {
			fmt.Printf("  resolved %d/%d episode IDs (%d added to index)\n",
				resolved, len(misses), added-cacheHits)
		}
	}

	return added, skippedPods
}

// overcastIndexEntry holds the data needed to update an episode's progress in Overcast.
// currentState is populated from the OPML export (or written-state cache for episodes
// found via extended matching) so we can skip no-op API calls.
type overcastIndexEntry struct {
	numericID    string            // overcastId value, e.g. "2891974064154832"
	currentState model.PlayState   // play state already in Overcast (from OPML or cache)
	currentPos   time.Duration     // current playback position in Overcast (from OPML or cache)
	episodeURL   string            // "https://overcast.fm/+HASH" — set for extended-matching entries
}

// furthestPlayState returns the (state, pos) pair representing the furthest listening
// progress between two candidates. Played is always furthest; between two InProgress
// states the higher position wins; Unplayed is the least advanced.
func furthestPlayState(s1 model.PlayState, p1 time.Duration, s2 model.PlayState, p2 time.Duration) (model.PlayState, time.Duration) {
	switch {
	case s1 == model.PlayStatePlayed:
		return s1, p1
	case s2 == model.PlayStatePlayed:
		return s2, p2
	case s1 == model.PlayStateInProgress && s2 == model.PlayStateInProgress:
		if p1 >= p2 {
			return s1, p1
		}
		return s2, p2
	case s1 == model.PlayStateInProgress:
		return s1, p1
	case s2 == model.PlayStateInProgress:
		return s2, p2
	default:
		return s1, p1
	}
}

// overcastOPMLState holds play state extracted from the matching OPML, keyed by numeric ID.
type overcastOPMLState struct {
	state model.PlayState
	pos   time.Duration
}

// buildOPMLNumericIDIndex returns a map from Overcast numeric episode ID → play state/pos,
// extracted from the OPML-sourced matching library.
func buildOPMLNumericIDIndex(lib *model.Library) map[string]overcastOPMLState {
	if lib == nil {
		return nil
	}
	m := make(map[string]overcastOPMLState, len(lib.Episodes))
	for _, ep := range lib.Episodes {
		if ep.GUID != "" {
			m[ep.GUID] = overcastOPMLState{state: ep.PlayState, pos: ep.PlayPosition}
		}
	}
	return m
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
		// Fallback key: fuzzy-normalised title + feedURL.
		// FuzzyNormalizeTitle strips season markers (S01, Season 1, …) and
		// punctuation so that title variants across subscriber and public feeds
		// ("The Retrievals - Ep. 4" vs "The Retrievals S01 - Ep. 4") resolve to
		// the same key and are recognised as the same episode.
		if ep.FeedURL != "" && ep.Title != "" {
			key := "feedtitle:" + normalizeFeedURL(ep.FeedURL) + "|" + migrate.FuzzyNormalizeTitle(ep.Title)
			if _, exists := index[key]; !exists {
				index[key] = entry
			}
		}
	}
	return index
}

// searchPodcastWithRetry calls SearchPodcast with a single 429 retry.
// On rate-limit, it waits the Retry-After period then tries once more.
func searchPodcastWithRetry(ctx context.Context, client *http.Client, title, overcastID string, requestDelay time.Duration) (PodcastSearchResult, error) {
	r, err := SearchPodcast(ctx, client, title, overcastID)
	if err == nil {
		return r, nil
	}
	var rl *httputil.RateLimitError
	if !errors.As(err, &rl) {
		return PodcastSearchResult{}, err
	}
	wait := rl.Wait
	if wait < requestDelay {
		wait = requestDelay
	}
	fmt.Printf("  rate limited searching for %q — waiting %v...\n", title, wait)
	select {
	case <-ctx.Done():
		return PodcastSearchResult{}, ctx.Err()
	case <-time.After(wait):
	}
	return SearchPodcast(ctx, client, title, overcastID)
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

		var rl *httputil.RateLimitError
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

// findInOvercastIndex matches ep against the Overcast episode index.
//
// Implemented strategies (migrate.MatchStrategy), in priority order:
//   - MatchByFeedDate  — feed URL + exact pub date.
//   - MatchByFeedTitle — feed URL + fuzzy title; skipped when strictFeedMatch is true.
//
// Absent strategies and rationale:
//   - MatchByGUID: Overcast does not expose episode GUIDs in its web or OPML API.
//   - MatchByTitleDate, MatchByPodDate, MatchByPodTitle: cross-feed matching is
//     impractical because Overcast episode IDs are numeric and only discoverable
//     by scraping the podcast page for a specific feed URL.
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
	// Uses FuzzyNormalizeTitle to match across season-marker variants
	// ("The Retrievals - Ep. 4" ↔ "The Retrievals S01 - Ep. 4").
	if !strictFeedMatch && ep.FeedURL != "" && ep.Title != "" {
		key := "feedtitle:" + normFeed + "|" + migrate.FuzzyNormalizeTitle(ep.Title)
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	return overcastIndexEntry{}, false
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

// isOvercastNumericID reports whether s looks like an Overcast episode numeric ID:
// a non-empty string of ASCII digits. Overcast IDs are large integers (e.g.
// "606972979413"). This is used to detect episodes that came from an Overcast OPML
// export, where ep.GUID is already the SetProgress numeric ID.
func isOvercastNumericID(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// overcastSkipReason returns "already_played", "already_ahead", or "" based on
// whether Overcast's current state already satisfies the desired state.
func overcastSkipReason(desired model.EpisodeState, current overcastIndexEntry) string {
	return migrate.SkipReason(desired.PlayState, desired.PlayPosition, current.currentState, current.currentPos)
}

// overcastAlreadySatisfied reports whether Overcast already satisfies the
// desired state (convenience wrapper used by tests).
func overcastAlreadySatisfied(desired model.EpisodeState, current overcastIndexEntry) bool {
	return overcastSkipReason(desired, current) != ""
}

