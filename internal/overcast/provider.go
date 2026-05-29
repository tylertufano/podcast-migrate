package overcast

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// DefaultRequestDelay is the pause between consecutive Overcast API requests
// when WriteOptions.RequestDelay is not set. 500 ms keeps the client well
// within Overcast's undocumented rate limit (2 requests/second).
const DefaultRequestDelay = 500 * time.Millisecond

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
type Provider struct {
	importOPMLPath string // path to existing Overcast OPML export (for reads + play state matching)
	exportOPMLPath string // destination path for generated import file (for subscription writes)
	email          string // Overcast account email (enables play state writes)
	password       string // Overcast account password
}

// NewProvider returns an Overcast provider without web API credentials.
// importOPMLPath is the path to an Overcast export file (for GetLibrary).
// exportOPMLPath is where the generated subscription import file will be written (for SetLibrary).
func NewProvider(importOPMLPath, exportOPMLPath string) *Provider {
	return &Provider{
		importOPMLPath: importOPMLPath,
		exportOPMLPath: exportOPMLPath,
	}
}

// NewProviderWithCredentials returns an Overcast provider that can also write episode
// play state using the unofficial Overcast web API. importOPMLPath must point to an
// Overcast extended OPML export (from overcast.fm/account/export_opml/extended) so the
// provider can map RSS episodes to their Overcast-specific URLs.
func NewProviderWithCredentials(importOPMLPath, exportOPMLPath, email, password string) *Provider {
	return &Provider{
		importOPMLPath: importOPMLPath,
		exportOPMLPath: exportOPMLPath,
		email:          email,
		password:       password,
	}
}

func (p *Provider) Name() string { return "Overcast" }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ReadSubscriptions:  p.importOPMLPath != "",
		ReadPlayState:      p.importOPMLPath != "",
		WriteSubscriptions: p.exportOPMLPath != "",
		// Play state writes require credentials and an extended OPML for episode matching.
		WritePlayState: p.email != "" && p.importOPMLPath != "",
	}
}

func (p *Provider) GetLibrary(ctx context.Context) (*model.Library, error) {
	if p.importOPMLPath == "" {
		return nil, &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "read (no import OPML path configured)",
		}
	}
	return NewOPMLReader(p.importOPMLPath).Read(ctx)
}

func (p *Provider) SetLibrary(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	writeSubscriptions := !opts.OnlyPlayState
	writePlayState := !opts.OnlySubscriptions && p.email != ""

	// When OnlyPlayState is explicitly requested but no credentials are configured,
	// return a clear error rather than silently doing nothing.
	if opts.OnlyPlayState && p.email == "" {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write play state (no credentials configured — set OVERCAST_EMAIL and OVERCAST_PASSWORD, or use --overcast-email/--overcast-password)",
		}
	}

	if writeSubscriptions {
		if p.exportOPMLPath == "" {
			return &provider.ErrCapabilityUnsupported{
				Provider:  p.Name(),
				Operation: "write subscriptions (no export OPML path configured)",
			}
		}
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
		if p.importOPMLPath == "" {
			return fmt.Errorf("overcast: writing play state requires an Overcast extended OPML export (use --overcast-export with a file from overcast.fm/account/export_opml/extended)")
		}
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

// doWritePlayState matches lib's episodes against the Overcast OPML, authenticates,
// then posts set_progress for each matched episode that has play state.
// Returns the number of episodes successfully updated.
func (p *Provider) doWritePlayState(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	// 1. Read the Overcast extended OPML to build a (pubDate+feedURL → numericID) index.
	//    We match by pub date and feed URL rather than GUID because the overcastId in
	//    the OPML is Overcast's internal numeric ID, not the RSS <guid> value.
	overcastLib, err := NewOPMLReader(p.importOPMLPath).Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("overcast: read OPML for play state matching: %w", err)
	}

	index := buildOvercastIndex(overcastLib)

	// 2. Authenticate.
	if opts.DryRun {
		// In dry-run mode, report what would be written without making any web requests.
		n := 0
		for _, ep := range lib.Episodes {
			if ep.PlayState == model.PlayStateUnplayed {
				continue
			}
			entry, ok := findInOvercastIndex(index, ep)
			if ok && !overcastAlreadySatisfied(ep, entry) {
				n++
			}
		}
		return n, nil
	}

	fmt.Printf("overcast: authenticating as %s...\n", p.email)
	httpClient, err := Login(ctx, p.email, p.password)
	if err != nil {
		return 0, fmt.Errorf("overcast: authentication failed: %w", err)
	}

	// 3. Resolve the request delay: honour the caller's preference, or fall
	//    back to the conservative default.
	requestDelay := opts.RequestDelay
	if requestDelay <= 0 {
		requestDelay = DefaultRequestDelay
	}

	// 4. Augment the index with episode IDs from Overcast podcast pages.
	//    This handles episodes in shared feeds that weren't in the OPML export
	//    (i.e. episodes the user listened to in Apple but never touched in Overcast).
	added := augmentIndexFromPodcastPages(ctx, httpClient, overcastLib, lib.Episodes, index, requestDelay)
	if added > 0 {
		fmt.Printf("overcast: extended matching added %d additional episode(s)\n", added)
	}

	// 5. For each episode with play state, look up its Overcast numeric ID and post
	//    set_progress. Failures on individual episodes are logged and skipped.
	fmt.Printf("overcast: request delay: %v between calls\n", requestDelay)

	// Pre-count: how many episodes need an API call vs. are already satisfied.
	toUpdate := 0
	alreadyDone := 0
	for _, ep := range lib.Episodes {
		if ep.PlayState == model.PlayStateUnplayed {
			continue
		}
		entry, ok := findInOvercastIndex(index, ep)
		if !ok {
			continue // unmatched — not in Overcast's history
		}
		if overcastAlreadySatisfied(ep, entry) {
			alreadyDone++
		} else {
			toUpdate++
		}
	}
	if alreadyDone > 0 {
		fmt.Printf("overcast: skipping %d already-satisfied episode(s) (Overcast state matches or exceeds source)\n", alreadyDone)
	}
	fmt.Printf("overcast: writing play state for %d episode(s)...\n", toUpdate)

	updated := 0
	apiSkipped := 0

	for _, ep := range lib.Episodes {
		if ep.PlayState == model.PlayStateUnplayed {
			continue
		}

		entry, ok := findInOvercastIndex(index, ep)
		if !ok {
			continue // unmatched — not in Overcast's history
		}

		// Skip episodes that Overcast already has in the desired state.
		// This dramatically reduces API calls on re-runs when most episodes
		// are already marked as played in Overcast.
		if overcastAlreadySatisfied(ep, entry) {
			continue
		}

		numericID := entry.numericID
		pos := int(ep.PlayPosition.Seconds())
		if ep.PlayState == model.PlayStatePlayed {
			pos = PlayedSentinel
		}

		var setErr error
		for attempt := 0; attempt < 4; attempt++ {
			setErr = SetProgress(ctx, httpClient, numericID, pos)
			if setErr == nil {
				break
			}
			var rl *RateLimitError
			if errors.As(setErr, &rl) {
				fmt.Printf("\n  rate limited (429) — pausing %v before retry...\n", rl.Wait)
				select {
				case <-ctx.Done():
					return updated, ctx.Err()
				case <-time.After(rl.Wait):
				}
				continue
			}
			break // non-rate-limit error — don't retry
		}
		if setErr != nil {
			fmt.Printf("  [%d/%d] FAILED %q (id=%s): %v\n",
				updated+apiSkipped+1, toUpdate, ep.Title, numericID, setErr)
			apiSkipped++

			// If the first call already indicates an auth failure, abort immediately.
			if updated == 0 && apiSkipped == 1 && strings.Contains(setErr.Error(), "login") {
				return 0, fmt.Errorf("overcast: aborting — first set_progress call redirected to login page.\n" +
					"This usually means the password is wrong or the account requires 2FA.\n" +
					"Verify your credentials and try again")
			}
			continue
		}
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

	if apiSkipped > 0 {
		fmt.Printf("overcast: %d episode(s) failed during write (see warnings above)\n", apiSkipped)
	}
	return updated, nil
}

// augmentIndexFromPodcastPages extends index with entries for Apple episodes whose
// Overcast numeric ID was not in the OPML export. It does this by:
//
//  1. Fetching /podcasts to map podcast titles → Overcast podcast page URLs.
//  2. For each shared podcast (in both Apple play history and Overcast), fetching
//     the podcast page to build a date → episode-hash map.
//  3. Resolving episode hashes to numeric IDs by fetching each episode page in
//     parallel (up to webWorkers concurrent requests).
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

	// Step 1: fetch /podcasts to get title → Overcast page path.
	fmt.Printf("overcast: fetching podcast list for extended episode matching...\n")
	podcastPaths, err := FetchSubscribedPodcasts(ctx, client)
	if err != nil {
		fmt.Printf("  warning: could not fetch /podcasts (%v) — skipping extended matching\n", err)
		return 0
	}

	// Build a lookup: feedURL → Overcast podcast page URL.
	// Match OPML podcasts to /podcasts page entries by normalised title.
	feedToPageURL := make(map[string]string)
	for _, pod := range overcastLib.Podcasts {
		norm := strings.ToLower(strings.TrimSpace(pod.Title))
		if path, ok := podcastPaths[norm]; ok {
			feedToPageURL[pod.FeedURL] = overcastBaseURL + path
		}
	}

	// Step 2: for each shared feed, fetch the podcast page and collect
	// (feedURL, dateStr "YYYY-MM-DD", episodeURL) triples where the episode
	// is not already in the index.
	type pendingFetch struct {
		feedURL     string
		dateRFC3339 string // used as index key
		episodeURL  string // "https://overcast.fm/+HASH"
	}
	var pending []pendingFetch

	// Count feeds that have episodes not yet in the index.
	unmatched := 0
	for feedURL, apEps := range appleByFeed {
		for _, ap := range apEps {
			key := "feeddate:" + feedURL + "|" + ap.PubDate.UTC().Format(time.RFC3339)
			if _, exists := index[key]; !exists {
				unmatched++
				break
			}
		}
	}
	if unmatched > 0 {
		fmt.Printf("overcast: extended matching: %d feed(s) have episodes not in OPML — fetching podcast pages...\n", unmatched)
	}

	matched := 0
	skippedFeeds := 0
	for feedURL, apEps := range appleByFeed {
		pageURL, ok := feedToPageURL[feedURL]
		if !ok {
			skippedFeeds++
			continue // podcast not found in Overcast subscription list
		}

		listings, err := fetchPodcastEpisodesWithRetry(ctx, client, pageURL, requestDelay)
		if err != nil {
			fmt.Printf("  warning: could not fetch podcast page %s: %v\n", pageURL, err)
			continue
		}
		time.Sleep(requestDelay)

		// Build date → episode URL map for this podcast page.
		// Key is "YYYY-MM-DD"; Overcast pages use day-level precision.
		dateToURL := make(map[string]string, len(listings))
		for _, l := range listings {
			if _, exists := dateToURL[l.DateStr]; !exists {
				dateToURL[l.DateStr] = l.OvercastURL
			}
		}

		for _, ap := range apEps {
			dateKey := "feeddate:" + feedURL + "|" + ap.PubDate.UTC().Format(time.RFC3339)
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
			if epURL == "" {
				continue
			}
			pending = append(pending, pendingFetch{feedURL, ap.PubDate.UTC().Format(time.RFC3339), epURL})
			matched++
		}
	}

	if skippedFeeds > 0 {
		fmt.Printf("overcast: extended matching: %d feed(s) not found in Overcast subscription list (not subscribed or title mismatch)\n", skippedFeeds)
	}
	if len(pending) == 0 {
		fmt.Printf("overcast: extended matching found no additional episodes\n")
		return 0
	}
	fmt.Printf("overcast: fetching numeric IDs for %d additional episode(s) via podcast pages...\n", len(pending))

	// Step 3: resolve hashes → numeric IDs sequentially with retry on 429.
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
			key := "feeddate:" + item.feedURL + "|" + item.dateRFC3339
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
		if !ep.PubDate.IsZero() && ep.FeedURL != "" {
			key := "feeddate:" + ep.FeedURL + "|" + ep.PubDate.UTC().Format(time.RFC3339)
			if _, exists := index[key]; !exists {
				index[key] = entry
			}
		}
		// Fallback key: normalized title + feedURL.
		if ep.FeedURL != "" && ep.Title != "" {
			key := "feedtitle:" + ep.FeedURL + "|" + strings.ToLower(strings.TrimSpace(ep.Title))
			if _, exists := index[key]; !exists {
				index[key] = entry
			}
		}
	}
	return index
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

// findInOvercastIndex looks up an episode by pubDate+feedURL then title+feedURL.
// Returns the index entry and whether a match was found.
func findInOvercastIndex(index map[string]overcastIndexEntry, ep model.EpisodeState) (overcastIndexEntry, bool) {
	if !ep.PubDate.IsZero() && ep.FeedURL != "" {
		key := "feeddate:" + ep.FeedURL + "|" + ep.PubDate.UTC().Format(time.RFC3339)
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	if ep.FeedURL != "" && ep.Title != "" {
		key := "feedtitle:" + ep.FeedURL + "|" + strings.ToLower(strings.TrimSpace(ep.Title))
		if entry, ok := index[key]; ok {
			return entry, true
		}
	}
	return overcastIndexEntry{}, false
}

// overcastAlreadySatisfied reports whether Overcast's current state for an episode
// already matches or exceeds what we want to write, making the set_progress call a no-op.
//
// Played beats everything. For in-progress, we skip if Overcast is already at
// or ahead of the desired position.
func overcastAlreadySatisfied(desired model.EpisodeState, current overcastIndexEntry) bool {
	switch desired.PlayState {
	case model.PlayStatePlayed:
		return current.currentState == model.PlayStatePlayed
	case model.PlayStateInProgress:
		if current.currentState == model.PlayStatePlayed {
			return true // already further
		}
		if current.currentState == model.PlayStateInProgress {
			return current.currentPos >= desired.PlayPosition
		}
	}
	return false
}
