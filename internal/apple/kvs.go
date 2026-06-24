package apple

import (
	"bytes"
	"compress/flate"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

const (
	kvsEndpoint    = "https://bookkeeper.itunes.apple.com/WebObjects/MZBookkeeper.woa/wa/putAll"
	kvsGetEndpoint = "https://bookkeeper.itunes.apple.com/WebObjects/MZBookkeeper.woa/wa/getAll"
	kvsDomain      = "com.apple.upp"
)

// KVSWriter pushes episode play state to Apple's UPP key-value store via
// bookkeeper.itunes.apple.com/putAll.
//
// By default it handles only private/subscriber-feed episodes (ZSTORETRACKID=0)
// that are not indexed in the Apple catalog. When AllFeeds is true it handles
// all episodes regardless of catalog status — used when no web API credentials
// are provided (KVS-only mode).
//
// Auth uses iTunes Store session cookies. These are sourced from the
// APPLE_KVS_COOKIES env var (preferred) or scanned from known binarycookies
// files on disk. The APPLE_KVS_DSID env var supplies the DSID when it cannot
// be extracted from the cookie string.
// kvsServerState is the decoded play state for one episode from the server's
// getAll(com.apple.upp) response. Used to skip episodes that are already at
// the desired play state (idempotency check).
type kvsServerState struct {
	HasBeenPlayed   bool
	BookmarkTimeSec float64
}

type KVSWriter struct {
	sqlitePath      string
	cookieHdr       string // full Cookie: header value
	dsid            string // iTunes Store account DSID
	httpClient      *http.Client
	sessionReady    bool              // true after getAll(com.apple.upp) has been called
	serverVersions   map[string]int             // metadataIdentifier → current server version, populated by getAll
	serverRawValues  map[string][]byte          // metadataIdentifier → DEFLATE-compressed inner plist bytes
	serverPlayStates map[string]kvsServerState  // lazily decoded per-episode play states

	// AllFeeds enables KVS-only mode: episode lookup is not restricted to
	// ZSTORETRACKID=0 (private/subscriber) episodes. Set by SetKVSOnlyMode.
	AllFeeds bool

	// com.apple.podcasts domain state — populated by initPodcastsDomain.
	podcastsDomainReady bool
	podcastsFeeds       map[string]*playStateFeed // feedURL → play state feed
	subscriptions       []podcastSubscription
	subVersion          string // version of podcastSubscriptions-2012-09-04

	// newlySubscribed tracks feeds subscribed during this run (feedURL → time).
	// Used by WriteBatch to defer episode lookups and retry after Apple indexes.
	newlySubscribed map[string]time.Time
}

// binaryCookiePaths is the list of paths tried when APPLE_KVS_COOKIES is unset.
// Checked in order; first one that yields a valid DSID wins.
var binaryCookiePaths = []string{
	// Sandboxed Podcasts container — present on older macOS builds.
	"Library/Containers/com.apple.podcasts/Data/Library/Cookies/Cookies.binarycookies",
	// System-level HTTPStorages — TV and Music share the iTunes Store session.
	"Library/HTTPStorages/com.apple.TV.binarycookies",
	"Library/HTTPStorages/com.apple.Music.binarycookies",
}

// NewKVSWriter constructs a KVSWriter ready to call putAll.
//
// Cookie auth is resolved in this order:
//  1. APPLE_KVS_COOKIES env var  (set from a Proxyman capture)
//  2. binarycookies files at known paths
//
// APPLE_KVS_DSID overrides the DSID when it cannot be parsed from the cookies.
//
// The HTTP client uses a cookie jar pre-populated with the initial session
// cookies. Apple's KVS server rotates tokens via Set-Cookie on each response;
// the jar handles this automatically so subsequent putAll calls in the same
// session use the updated tokens rather than the stale originals.
func NewKVSWriter(sqlitePath string) (*KVSWriter, error) {
	dsid, cookieHdr, err := resolveCookies()
	if err != nil {
		return nil, err
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("kvs: cookie jar: %w", err)
	}
	kvsURL, _ := url.Parse("https://bookkeeper.itunes.apple.com")
	jar.SetCookies(kvsURL, parseCookieHeader(cookieHdr))

	if sqlitePath == "" {
		sqlitePath = DefaultSQLitePath()
	}
	return &KVSWriter{
		sqlitePath: sqlitePath,
		cookieHdr:  cookieHdr,
		dsid:       dsid,
		httpClient: &http.Client{Timeout: 30 * time.Second, Jar: jar},
	}, nil
}

// parseCookieHeader splits a Cookie: header value into individual http.Cookie
// values suitable for seeding a cookiejar.
func parseCookieHeader(header string) []*http.Cookie {
	var cookies []*http.Cookie
	for _, part := range strings.Split(header, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		cookies = append(cookies, &http.Cookie{
			Name:  strings.TrimSpace(kv[0]),
			Value: strings.TrimSpace(kv[1]),
		})
	}
	return cookies
}

// resolveCookies finds the Cookie header and DSID for KVS requests.
func resolveCookies() (dsid, cookieHdr string, err error) {
	// Env var takes precedence — populate from a Proxyman capture.
	if envCookies := os.Getenv("APPLE_KVS_COOKIES"); envCookies != "" {
		cookieHdr = envCookies
		dsid = os.Getenv("APPLE_KVS_DSID")
		if dsid == "" {
			dsid = extractDSIDFromCookies(cookieHdr)
		}
		if dsid == "" {
			return "", "", fmt.Errorf("kvs: APPLE_KVS_COOKIES is set but no DSID found — also set APPLE_KVS_DSID")
		}
		return dsid, cookieHdr, nil
	}

	// Fall back to known binarycookies paths.
	home, _ := os.UserHomeDir()
	for _, rel := range binaryCookiePaths {
		p := filepath.Join(home, rel)
		d, ch, readErr := parsePodcastCookies(p)
		if readErr == nil && d != "" {
			return d, ch, nil
		}
	}

	return "", "", fmt.Errorf("kvs: no iTunes Store session found\n" +
		"  Set APPLE_KVS_COOKIES to the Cookie: header value from a Proxyman capture of\n" +
		"  bookkeeper.itunes.apple.com while Apple Podcasts is open and signed in.\n" +
		"  Also set APPLE_KVS_DSID to your numeric DSID (visible as X-Dsid= in the cookie).")
}

// extractDSIDFromCookies parses the X-Dsid value from a Cookie header string.
func extractDSIDFromCookies(cookies string) string {
	for _, part := range strings.Split(cookies, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "X-Dsid") {
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

// kvsItemWithMeta pairs a kvsItem with the source episode state for logging.
type kvsItemWithMeta struct {
	item     kvsItem
	ep       model.EpisodeState
	podTitle string
}

// applyPlayState fills item's play-state fields from ep.
func applyPlayState(ep model.EpisodeState, item *kvsItem, nowSec float64) {
	item.HasBeenPlayed = ep.PlayState == model.PlayStatePlayed
	item.BookmarkTimeSec = 0
	if ep.PlayState == model.PlayStateInProgress {
		item.BookmarkTimeSec = ep.PlayPosition.Seconds()
	}
	if ep.PlayState == model.PlayStatePlayed && item.PlayCount < 1 {
		item.PlayCount = 1
	}
	item.TimestampSec = nowSec
}

// Write is the provider.Writer interface implementation. It finds episode
// matches in the local Apple Podcasts DB and pushes all of them via putAll.
//
// When AllFeeds is false (default), only private/subscriber-feed episodes
// (ZSTORETRACKID=0) are handled; catalog episodes are left to WebAPIWriter.
// When AllFeeds is true (KVS-only mode), all episodes are handled here.
//
// All matched episodes are batched into one HTTP call to work around the
// server's one-time-use session token: a second putAll with a spent token
// returns status 1198 regardless of cookie rotation.
func (w *KVSWriter) Write(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	db, err := sql.Open("sqlite", "file:"+w.sqlitePath+"?mode=ro&_journal=off")
	if err != nil {
		return 0, fmt.Errorf("kvs: open sqlite: %w", err)
	}
	defer db.Close()

	feedToTitle := migrate.BuildFeedToTitle(lib)
	episodes := migrate.FilterEpisodesByPodcast(lib.Episodes, feedToTitle, opts.PodcastFilter)

	migrate.WriteLogHeader(opts.LogWriter)

	// Eagerly fetch current server state so we can skip already-synced episodes.
	if !opts.DryRun {
		if iErr := w.initSession(ctx); iErr != nil {
			fmt.Printf("apple/kvs: session init failed (will write without skip check): %v\n", iErr)
		}
	}

	now := time.Since(coreDataEpoch).Seconds()
	var pending []kvsItemWithMeta
	dryRunCount := 0

	for _, ep := range episodes {
		if ep.FromDestination {
			continue
		}
		if ep.PlayState != model.PlayStatePlayed && ep.PlayState != model.PlayStateInProgress {
			continue
		}
		if opts.SubscribedOnly && !w.IsSubscribed(ep.FeedURL) {
			continue
		}

		item, found, err := w.lookupPrivateEpisode(ctx, db, ep)
		if err != nil {
			fmt.Printf("  kvs: lookup failed for %q: %v\n", ep.Title, err)
			migrate.WriteLogLine(opts.LogWriter, "error", feedToTitle[ep.FeedURL], ep.Title, ep.PubDate,
				migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—", err.Error())
			continue
		}
		if !found {
			continue
		}

		podTitle := feedToTitle[ep.FeedURL]

		// Skip episodes already at the desired play state on the server.
		if !opts.ForceUpdate {
			if serverState, ok := w.checkServerPlayState(ctx, item.MetadataIdentifier); ok {
				if serverStateCoversDesired(ep, serverState) {
					migrate.WriteLogLine(opts.LogWriter, "skipped", podTitle, ep.Title, ep.PubDate,
						migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—", "already synced via KVS")
					continue
				}
			}
		}

		applyPlayState(ep, &item, now)

		if opts.DryRun {
			fmt.Printf("  [dry-run] kvs: would putAll %q — %q (key=%s)\n",
				podTitle, ep.Title, item.MetadataIdentifier)
			migrate.WriteLogLine(opts.LogWriter, "would_update", podTitle, ep.Title, ep.PubDate,
				migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—", w.kvsLogLabel())
			dryRunCount++
			continue
		}

		pending = append(pending, kvsItemWithMeta{item, ep, podTitle})
	}

	if opts.DryRun {
		return dryRunCount, nil
	}
	if len(pending) == 0 {
		return 0, nil
	}

	// Deduplicate by metadataIdentifier before putAll — two items with the same
	// key in a single request returns HTTP 500. Keep the higher-priority play state.
	playPriority := func(ep model.EpisodeState) int {
		switch ep.PlayState {
		case model.PlayStatePlayed:
			return 2
		case model.PlayStateInProgress:
			return 1
		default:
			return 0
		}
	}
	type dedupSlot struct {
		idx int
		pri int
	}
	dedupSeen := make(map[string]dedupSlot)
	var unique []kvsItemWithMeta
	for _, pm := range pending {
		pri := playPriority(pm.ep)
		if slot, exists := dedupSeen[pm.item.MetadataIdentifier]; exists {
			if pri > slot.pri {
				unique[slot.idx] = pm
				dedupSeen[pm.item.MetadataIdentifier] = dedupSlot{slot.idx, pri}
			}
		} else {
			dedupSeen[pm.item.MetadataIdentifier] = dedupSlot{len(unique), pri}
			unique = append(unique, pm)
		}
	}
	pending = unique

	// Single putAll for all matched episodes — one token, one round-trip.
	items := make([]kvsItem, len(pending))
	for i, p := range pending {
		items[i] = p.item
	}
	conflicts, err := w.putAll(ctx, items)
	if err != nil {
		for _, p := range pending {
			migrate.WriteLogLine(opts.LogWriter, "error", p.podTitle, p.ep.Title, p.ep.PubDate,
				migrate.PlayStateLabel(p.ep.PlayState, p.ep.PlayPosition), "—", err.Error())
		}
		return 0, err
	}

	count := 0
	var needsRetry []kvsItemWithMeta
	for _, p := range pending {
		if _, wasConflict := conflicts[p.item.MetadataIdentifier]; wasConflict {
			// Server already has this key at a newer version (status=1198).
			// Check whether its current state covers our desired state, unless
			// ForceUpdate is set (in which case always retry with the correct
			// base-version to ensure the server overwrites its current state).
			if !opts.ForceUpdate {
				if serverState, ok := w.checkServerPlayState(ctx, p.item.MetadataIdentifier); ok &&
					serverStateCoversDesired(p.ep, serverState) {
					migrate.WriteLogLine(opts.LogWriter, "skipped", p.podTitle, p.ep.Title, p.ep.PubDate,
						migrate.PlayStateLabel(p.ep.PlayState, p.ep.PlayPosition), "—", "already synced via KVS")
					continue
				}
			}
			needsRetry = append(needsRetry, p)
			continue
		}
		fmt.Printf("  kvs: synced %q — %q\n", p.podTitle, p.ep.Title)
		migrate.WriteLogLine(opts.LogWriter, "updated", p.podTitle, p.ep.Title, p.ep.PubDate,
			migrate.PlayStateLabel(p.ep.PlayState, p.ep.PlayPosition), "—", w.kvsLogLabel())
		count++
	}

	if len(needsRetry) > 0 {
		retryItems := make([]kvsItem, len(needsRetry))
		for i, p := range needsRetry {
			retryItems[i] = p.item
		}
		if _, retryErr := w.putAll(ctx, retryItems); retryErr != nil {
			for _, p := range needsRetry {
				migrate.WriteLogLine(opts.LogWriter, "error", p.podTitle, p.ep.Title, p.ep.PubDate,
					migrate.PlayStateLabel(p.ep.PlayState, p.ep.PlayPosition), "—", retryErr.Error())
			}
		} else {
			for _, p := range needsRetry {
				fmt.Printf("  kvs: synced %q — %q\n", p.podTitle, p.ep.Title)
				migrate.WriteLogLine(opts.LogWriter, "updated", p.podTitle, p.ep.Title, p.ep.PubDate,
					migrate.PlayStateLabel(p.ep.PlayState, p.ep.PlayPosition), "—", w.kvsLogLabel())
				count++
			}
		}
	}

	return count, nil
}

// kvsLogLabel returns the sync-method label for log lines.
func (w *KVSWriter) kvsLogLabel() string {
	if w.AllFeeds {
		return "kvs"
	}
	return "kvs (private feed)"
}

// WriteBatch resolves and syncs a slice of episodes via a single putAll request.
// Called by WebAPIWriter as the fallback for private-feed episodes that cannot
// be resolved via the Apple catalog (CatalogPodcastNotInCatalog). Batching is
// critical: the server's session token is single-use, so a second sequential
// putAll with the same token returns status 1198.
//
// Lookup order for each episode:
//  1. com.apple.podcasts play state cache (no SQLite required)
//  2. Local SQLite (GUID, then FeedURL+PubDate, then FeedURL+Title)
//
// Episodes from feeds subscribed during this same run are deferred to a retry
// pass: Apple Podcasts must fetch and index the feed before episode
// metadataIdentifiers appear in KVS or SQLite.
func (w *KVSWriter) WriteBatch(ctx context.Context, episodes []model.EpisodeState, feedToTitle map[string]string, opts provider.WriteOptions) (int, error) {
	// Auto-subscribe any unsubscribed private feeds before the write pass.
	// This ensures metadataIdentifiers exist in the KVS for future lookups.
	if w.podcastsDomainReady && !opts.DryRun {
		seen := make(map[string]bool)
		for _, ep := range episodes {
			if seen[ep.FeedURL] {
				continue
			}
			seen[ep.FeedURL] = true
			if !w.IsSubscribed(ep.FeedURL) {
				title := feedToTitle[ep.FeedURL]
				if title == "" {
					title = ep.FeedURL
				}
				isNew, subErr := w.Subscribe(ctx, ep.FeedURL, title)
				if subErr != nil {
					fmt.Printf("  kvs: subscribe %q failed: %v\n", title, subErr)
				} else if isNew {
					if opts.SubscriptionsAddedOut != nil {
						*opts.SubscriptionsAddedOut++
					}
					fmt.Printf("  kvs: subscribed to %q\n", title)
				}
			}
		}
	}

	db, err := sql.Open("sqlite", "file:"+w.sqlitePath+"?mode=ro&_journal=off")
	if err != nil {
		return 0, fmt.Errorf("kvs: open sqlite: %w", err)
	}
	defer db.Close()

	// Eagerly fetch current server state so resolveEpisodes can skip already-synced episodes.
	if !opts.DryRun {
		if iErr := w.initSession(ctx); iErr != nil {
			fmt.Printf("apple/kvs: session init failed (will write without skip check): %v\n", iErr)
		}
	}

	now := time.Since(coreDataEpoch).Seconds()

	// resolveEpisodes categorises each episode into:
	//   pending  — lookup succeeded; ready to write
	//   deferred — newly subscribed feed; not yet indexed; will retry
	// Episodes that are neither are logged and dropped here.
	resolveEpisodes := func(eps []model.EpisodeState) (pending []kvsItemWithMeta, deferred []model.EpisodeState, dryCount int) {
		for _, ep := range eps {
			if opts.SubscribedOnly && !w.IsSubscribed(ep.FeedURL) {
				continue
			}
			podTitle := feedToTitle[ep.FeedURL]
			item, found, lookupErr := w.lookupPrivateEpisode(ctx, db, ep)
			if lookupErr != nil {
				fmt.Printf("  kvs: lookup failed for %q: %v\n", ep.Title, lookupErr)
				migrate.WriteLogLine(opts.LogWriter, "error", podTitle, ep.Title, ep.PubDate,
					migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—", lookupErr.Error())
				continue
			}
			if !found {
				if _, isNew := w.newlySubscribed[ep.FeedURL]; isNew && !opts.DryRun {
					deferred = append(deferred, ep)
				} else {
					migrate.WriteLogLine(opts.LogWriter, "no_apple_id", podTitle, ep.Title, ep.PubDate,
						migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—",
						"not in catalog; not in local DB or KVS — open Apple Podcasts to index this feed")
				}
				continue
			}

			// Skip episodes already at the desired play state on the server.
			if !opts.ForceUpdate && !opts.DryRun {
				if serverState, ok := w.checkServerPlayState(ctx, item.MetadataIdentifier); ok {
					if serverStateCoversDesired(ep, serverState) {
						migrate.WriteLogLine(opts.LogWriter, "skipped", podTitle, ep.Title, ep.PubDate,
							migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—", "already synced via KVS")
						continue
					}
				}
			}

			applyPlayState(ep, &item, now)
			if opts.DryRun {
				fmt.Printf("  [dry-run] kvs: would putAll %q — %q (key=%s)\n",
					podTitle, ep.Title, item.MetadataIdentifier)
				migrate.WriteLogLine(opts.LogWriter, "would_update", podTitle, ep.Title, ep.PubDate,
					migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—", w.kvsLogLabel())
				dryCount++
				continue
			}
			pending = append(pending, kvsItemWithMeta{item, ep, podTitle})
		}
		return
	}

	// flushPending sends a batch via putAll and logs each outcome.
	flushPending := func(p []kvsItemWithMeta) (int, error) {
		if len(p) == 0 {
			return 0, nil
		}

		// Deduplicate by metadataIdentifier: sending two items with the same key
		// in a single putAll returns HTTP 500. This happens when the KVS play-state
		// lookup (pub-date based) maps two source episodes to the same Apple entry.
		// Keep the one with the higher play state; drop the other.
		playPriority := func(ep model.EpisodeState) int {
			switch ep.PlayState {
			case model.PlayStatePlayed:
				return 2
			case model.PlayStateInProgress:
				return 1
			default:
				return 0
			}
		}
		type dedupSlot struct {
			idx int
			pri int
		}
		seen := make(map[string]dedupSlot)
		var unique []kvsItemWithMeta
		for _, pm := range p {
			pri := playPriority(pm.ep)
			if slot, exists := seen[pm.item.MetadataIdentifier]; exists {
				if pri > slot.pri {
					unique[slot.idx] = pm
					seen[pm.item.MetadataIdentifier] = dedupSlot{slot.idx, pri}
				}
				// else: drop the lower-priority duplicate
			} else {
				seen[pm.item.MetadataIdentifier] = dedupSlot{len(unique), pri}
				unique = append(unique, pm)
			}
		}
		p = unique

		items := make([]kvsItem, len(p))
		for i, pm := range p {
			items[i] = pm.item
		}
		conflicts, putErr := w.putAll(ctx, items)
		if putErr != nil {
			for _, pm := range p {
				migrate.WriteLogLine(opts.LogWriter, "error", pm.podTitle, pm.ep.Title, pm.ep.PubDate,
					migrate.PlayStateLabel(pm.ep.PlayState, pm.ep.PlayPosition), "—", putErr.Error())
			}
			return 0, fmt.Errorf("kvs: putAll batch failed: %w", putErr)
		}
		count := 0
		var needsRetry []kvsItemWithMeta
		for _, pm := range p {
			if _, wasConflict := conflicts[pm.item.MetadataIdentifier]; wasConflict {
				if !opts.ForceUpdate {
					if serverState, ok := w.checkServerPlayState(ctx, pm.item.MetadataIdentifier); ok &&
						serverStateCoversDesired(pm.ep, serverState) {
						migrate.WriteLogLine(opts.LogWriter, "skipped", pm.podTitle, pm.ep.Title, pm.ep.PubDate,
							migrate.PlayStateLabel(pm.ep.PlayState, pm.ep.PlayPosition), "—", "already synced via KVS")
						continue
					}
				}
				needsRetry = append(needsRetry, pm)
				continue
			}
			fmt.Printf("  kvs: synced %q — %q\n", pm.podTitle, pm.ep.Title)
			migrate.WriteLogLine(opts.LogWriter, "updated", pm.podTitle, pm.ep.Title, pm.ep.PubDate,
				migrate.PlayStateLabel(pm.ep.PlayState, pm.ep.PlayPosition), "—", w.kvsLogLabel())
			count++
		}
		if len(needsRetry) > 0 {
			retryItems := make([]kvsItem, len(needsRetry))
			for i, pm := range needsRetry {
				retryItems[i] = pm.item
			}
			if _, retryErr := w.putAll(ctx, retryItems); retryErr != nil {
				for _, pm := range needsRetry {
					migrate.WriteLogLine(opts.LogWriter, "error", pm.podTitle, pm.ep.Title, pm.ep.PubDate,
						migrate.PlayStateLabel(pm.ep.PlayState, pm.ep.PlayPosition), "—", retryErr.Error())
				}
			} else {
				for _, pm := range needsRetry {
					fmt.Printf("  kvs: synced %q — %q\n", pm.podTitle, pm.ep.Title)
					migrate.WriteLogLine(opts.LogWriter, "updated", pm.podTitle, pm.ep.Title, pm.ep.PubDate,
						migrate.PlayStateLabel(pm.ep.PlayState, pm.ep.PlayPosition), "—", w.kvsLogLabel())
					count++
				}
			}
		}
		return count, nil
	}

	// --- First pass ---
	pending, deferred, dryRunCount := resolveEpisodes(episodes)

	if opts.DryRun {
		return dryRunCount, nil
	}

	count, err := flushPending(pending)
	if err != nil {
		return count, err
	}
	if len(pending) > 0 {
		// Reset session so the deferred putAll (if needed) gets a fresh UPP token.
		w.sessionReady = false
	}

	if len(deferred) == 0 {
		return count, nil
	}

	// --- Retry pass for newly subscribed feeds ---
	// Apple Podcasts must fetch the RSS feed and write episode metadata to both
	// com.apple.podcasts (for metadataIdentifier lookup) AND com.apple.upp
	// (to establish the base-version for putAll) before we can sync play state.
	// We bring Podcasts to the foreground to trigger a background sync, then poll
	// both domains until the episodes are ready to write.
	const (
		retryAttempts = 24
		retryInterval = 5 * time.Second
	)

	feedTitle := func(feedURL string) string {
		if t := feedToTitle[feedURL]; t != "" {
			return t
		}
		return feedURL
	}

	deferredFeeds := make(map[string]struct{})
	for _, ep := range deferred {
		deferredFeeds[ep.FeedURL] = struct{}{}
	}
	fmt.Printf("  kvs: %d episode(s) from %d newly subscribed feed(s) deferred — waiting for Apple Podcasts to index...\n",
		len(deferred), len(deferredFeeds))

	// Bring Podcasts to the foreground and trigger a full feed refresh (Cmd+R).
	// Without focus the app rarely syncs; without the refresh it won't fetch
	// newly subscribed feeds until its next background poll.
	refreshScript := `tell application "Podcasts" to activate
delay 0.5
tell application "System Events"
	tell process "Podcasts"
		keystroke "r" using command down
	end tell
end tell`
	if out, oErr := exec.Command("osascript", "-e", refreshScript).CombinedOutput(); oErr != nil {
		fmt.Printf("  kvs: could not refresh Podcasts app: %v (%s)\n", oErr, strings.TrimSpace(string(out)))
		fmt.Println("  kvs: open Apple Podcasts manually and press Cmd+R to refresh feeds")
	} else {
		fmt.Println("  kvs: opened Apple Podcasts and triggered feed refresh (Cmd+R)")
	}

	// pendingFlush accumulates resolved episodes across outer iterations.
	// After a putAll failure we keep items here and retry on the next tick —
	// the outer loop refreshes both KVS domains (including serverVersions for
	// UPP base-versions) before each attempt so versions are always current.
	var pendingFlush []kvsItemWithMeta
	stillDeferred := deferred
	noProgressStreak := 0 // ticks with resolved==0 and pendingFlush==0
	gaveUpEarly := false  // true when we broke out due to no-progress streak
	everResolved := false // true after at least one episode was resolved

	for attempt := 1; attempt <= retryAttempts; attempt++ {
		fmt.Printf("  kvs: retry %d/%d (waiting %s)...\n", attempt, retryAttempts, retryInterval)
		select {
		case <-ctx.Done():
			goto deferredDone
		case <-time.After(retryInterval):
		}

		// Reset to original cookies at the start of each tick so that any
		// Set-Cookie state from a prior failed putAll doesn't carry forward.
		if jar, jarErr := cookiejar.New(nil); jarErr == nil {
			kvsURL, _ := url.Parse("https://bookkeeper.itunes.apple.com")
			jar.SetCookies(kvsURL, parseCookieHeader(w.cookieHdr))
			w.httpClient.Jar = jar
		}

		// Refresh podcasts domain for episode lookups.
		w.podcastsDomainReady = false
		if iErr := w.initPodcastsDomain(ctx); iErr != nil {
			fmt.Printf("  kvs: podcasts domain refresh failed: %v\n", iErr)
			continue
		}

		resolved, nextDeferred, _ := resolveEpisodes(stillDeferred)
		stillDeferred = nextDeferred

		if len(resolved) > 0 {
			noProgressStreak = 0
			everResolved = true
			indexed := make(map[string]int)
			for _, pm := range resolved {
				indexed[pm.ep.FeedURL]++
			}
			for feedURL, n := range indexed {
				fmt.Printf("  kvs: %q indexed — %d episode(s) resolved\n", feedTitle(feedURL), n)
			}
			pendingFlush = append(pendingFlush, resolved...)
		} else if len(pendingFlush) == 0 {
			if everResolved {
				fmt.Printf("  kvs: %d episode(s) not found in Apple Podcasts index — may no longer be in the RSS feed\n",
					len(stillDeferred))
			} else {
				fmt.Printf("  kvs: feed not yet indexed in com.apple.podcasts — waiting...\n")
			}
		}

		if len(pendingFlush) > 0 {
			// Re-init the UPP session immediately before each putAll attempt.
			// The podcasts-domain getAll above already rotated the cookies;
			// passing that state into getAll(UPP) gives the server a coherent
			// podcasts→UPP→putAll session chain.
			w.sessionReady = false
			if iErr := w.initSession(ctx); iErr != nil {
				fmt.Printf("  kvs: session refresh failed before flush: %v\n", iErr)
				continue
			}

			n, ferr := flushPending(pendingFlush)
			count += n
			if ferr == nil {
				pendingFlush = nil
			} else {
				fmt.Printf("  kvs: putAll failed — retrying next tick: %v\n", ferr)
			}
		}

		if len(stillDeferred) == 0 && len(pendingFlush) == 0 {
			break
		}

		// If we've made at least one successful flush but haven't resolved any
		// new episodes for several ticks, the remaining episodes are likely no
		// longer in the feed's RSS and won't appear in the Apple Podcasts index.
		if count > 0 && len(resolved) == 0 && len(pendingFlush) == 0 {
			noProgressStreak++
			if noProgressStreak >= 3 {
				gaveUpEarly = true
				break
			}
		}
	}

deferredDone:
	// Episodes that resolved but whose putAll never succeeded after all retries.
	if len(pendingFlush) > 0 {
		unwrittenFeeds := make(map[string]struct{})
		for _, pm := range pendingFlush {
			unwrittenFeeds[pm.ep.FeedURL] = struct{}{}
		}
		fmt.Printf("\nkvs: %d episode(s) resolved but could not be written — UPP entries may still be propagating.\n",
			len(pendingFlush))
		fmt.Println("Re-run with:")
		for feedURL := range unwrittenFeeds {
			fmt.Printf("  --podcast %q\n", feedTitle(feedURL))
		}
		fmt.Println()
	}

	// Log episodes that still couldn't be resolved.
	if len(stillDeferred) > 0 {
		unindexedFeeds := make(map[string]struct{})
		for _, ep := range stillDeferred {
			unindexedFeeds[ep.FeedURL] = struct{}{}
		}
		if gaveUpEarly {
			// We already synced some episodes from this feed, but the remaining
			// ones couldn't be matched. They're likely no longer in the RSS.
			for _, ep := range stillDeferred {
				podTitle := feedTitle(ep.FeedURL)
				migrate.WriteLogLine(opts.LogWriter, "no_apple_id", podTitle, ep.Title, ep.PubDate,
					migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—",
					"not found in Apple Podcasts index — may no longer be in the RSS feed")
			}
			fmt.Printf("\nkvs: %d episode(s) skipped — not found in Apple Podcasts (likely removed from the RSS feed).\n",
				len(stillDeferred))
		} else {
			for _, ep := range stillDeferred {
				podTitle := feedTitle(ep.FeedURL)
				migrate.WriteLogLine(opts.LogWriter, "no_apple_id", podTitle, ep.Title, ep.PubDate,
					migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—",
					"newly subscribed feed not yet indexed — re-run after opening Apple Podcasts")
			}
			fmt.Printf("\nkvs: %d episode(s) from %d feed(s) not yet indexed by Apple Podcasts.\n",
				len(stillDeferred), len(unindexedFeeds))
			fmt.Println("Open Apple Podcasts and wait for it to refresh, then re-run with:")
			for feedURL := range unindexedFeeds {
				fmt.Printf("  --podcast %q\n", feedTitle(feedURL))
			}
			fmt.Println()
		}
	}

	return count, nil
}

// WriteEpisode looks up a single episode and pushes its play state to the KVS.
// Returns (true, nil) when the push succeeded, (false, nil) when the episode is
// not found in the local DB as a private-feed episode.
//
// Prefer WriteBatch when syncing multiple episodes — the session token is
// single-use so sequential WriteEpisode calls will fail after the first.
func (w *KVSWriter) WriteEpisode(ctx context.Context, ep model.EpisodeState) (bool, error) {
	db, err := sql.Open("sqlite", "file:"+w.sqlitePath+"?mode=ro&_journal=off")
	if err != nil {
		return false, fmt.Errorf("kvs: open sqlite: %w", err)
	}
	defer db.Close()

	item, found, err := w.lookupPrivateEpisode(ctx, db, ep)
	if err != nil || !found {
		return found, err
	}

	now := time.Since(coreDataEpoch).Seconds()
	applyPlayState(ep, &item, now)

	_, err = w.putAll(ctx, []kvsItem{item})
	return err == nil, err
}

// kvsItem holds the data needed to build one entry in a putAll request.
type kvsItem struct {
	MetadataIdentifier string  // ZMTEPISODE.ZMETADATAIDENTIFIER = the KVS key
	UPPVersion         int     // ZMTUPPMETADATA.Z_OPT = "base-version"
	BookmarkTimeSec    float64 // bookmark position (0 = fully played)
	HasBeenPlayed      bool
	PlayCount          int
	TimestampSec       float64 // CoreData epoch seconds
}

// lookupPrivateEpisodeFromKVS checks the cached com.apple.podcasts play state
// for the episode's metadataIdentifier. Returns a kvsItem with UPPVersion=1
// (overridden later from serverVersions if the episode exists in com.apple.upp).
func (w *KVSWriter) lookupPrivateEpisodeFromKVS(ep model.EpisodeState) (kvsItem, bool) {
	metaID, ok := w.lookupEpisodeViaPlayState(ep.FeedURL, ep.GUID)
	if !ok {
		return kvsItem{}, false
	}
	return kvsItem{
		MetadataIdentifier: metaID,
		UPPVersion:         0, // 0 = new entry; overridden by serverVersions if already in com.apple.upp
	}, true
}

// lookupPrivateEpisode finds the Apple Podcasts episode that matches ep and
// returns its KVS metadata. Only private-feed episodes (ZSTORETRACKID=0) with
// a non-empty ZMETADATAIDENTIFIER are returned.
//
// Implemented strategies (migrate.MatchStrategy), in priority order:
//   - MatchByGUID     — KVS play state cache (fast, no SQLite), then SQLite GUID column.
//   - MatchByFeedDate — SQLite: feed URL prefix + pub date within ±24 hours.
//   - MatchByFeedTitle — SQLite: feed URL prefix + case-insensitive exact title.
//
// Absent strategies and rationale:
//   - MatchByTitleDate, MatchByPodDate, MatchByPodTitle: cross-feed matching is
//     not needed here because private-feed episodes are always tied to a specific
//     feed URL in SQLite. Catalog episodes (public feeds) are matched separately
//     by CatalogClient.FindEpisode which implements the full 4-strategy cascade.
//
// Note: MatchByFeedTitle uses SQL LOWER(TRIM(...)) = LOWER(TRIM(?)) rather than
// migrate.FuzzyNormalizeTitle, so season-marker variants ("Ep. 4" vs "S01 Ep. 4")
// may not match. This is a known accuracy gap; GUID and feeddate cover the vast
// majority of episodes so the impact is small in practice.
func (w *KVSWriter) lookupPrivateEpisode(ctx context.Context, db *sql.DB, ep model.EpisodeState) (kvsItem, bool, error) {
	// Fast path: check the KVS play state we already fetched.
	if item, ok := w.lookupPrivateEpisodeFromKVS(ep); ok {
		return item, true, nil
	}

	// In KVS-only mode all episodes are in scope; otherwise restrict to
	// private/subscriber-feed episodes (ZSTORETRACKID=0).
	trackFilter := "AND (e.ZSTORETRACKID IS NULL OR e.ZSTORETRACKID = 0)\n\t\t\t"
	if w.AllFeeds {
		trackFilter = ""
	}

	// Fall back to local SQLite. GUID match first (exact, fast).
	if ep.GUID != "" {
		item, found, err := scanKVSRow(db.QueryRowContext(ctx, `
			SELECT e.ZMETADATAIDENTIFIER,
			       COALESCE(u.Z_OPT, 1),
			       COALESCE(u.ZBOOKMARKTIME, 0.0),
			       COALESCE(u.ZHASBEENPLAYED, 0),
			       COALESCE(u.ZPLAYCOUNT, 0),
			       COALESCE(u.ZTIMESTAMP, 0.0)
			FROM ZMTEPISODE e
			JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
			LEFT JOIN ZMTUPPMETADATA u ON u.ZMETADATAIDENTIFIER = e.ZMETADATAIDENTIFIER
			WHERE e.ZMETADATAIDENTIFIER IS NOT NULL
			  `+trackFilter+`AND p.ZSUBSCRIBED = 1
			  AND e.ZGUID = ?
			LIMIT 1`, ep.GUID))
		if err != nil {
			return kvsItem{}, false, fmt.Errorf("lookup by GUID: %w", err)
		}
		if found {
			return item, true, nil
		}
	}

	// Fall back to FeedURL + PubDate (within 24 hours).
	if ep.FeedURL != "" && !ep.PubDate.IsZero() {
		pubDateSec := ep.PubDate.Sub(coreDataEpoch).Seconds()
		item, found, err := scanKVSRow(db.QueryRowContext(ctx, `
			SELECT e.ZMETADATAIDENTIFIER,
			       COALESCE(u.Z_OPT, 1),
			       COALESCE(u.ZBOOKMARKTIME, 0.0),
			       COALESCE(u.ZHASBEENPLAYED, 0),
			       COALESCE(u.ZPLAYCOUNT, 0),
			       COALESCE(u.ZTIMESTAMP, 0.0)
			FROM ZMTEPISODE e
			JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
			LEFT JOIN ZMTUPPMETADATA u ON u.ZMETADATAIDENTIFIER = e.ZMETADATAIDENTIFIER
			WHERE e.ZMETADATAIDENTIFIER IS NOT NULL
			  `+trackFilter+`AND p.ZSUBSCRIBED = 1
			  AND p.ZFEEDURL LIKE ? || '%'
			  AND ABS(COALESCE(e.ZPUBDATE, 0) - ?) < 86400
			LIMIT 1`, ep.FeedURL, pubDateSec))
		if err != nil {
			return kvsItem{}, false, fmt.Errorf("lookup by feed+date: %w", err)
		}
		if found {
			return item, true, nil
		}
	}

	// Final fallback: FeedURL + title (case-insensitive).
	if ep.FeedURL != "" && ep.Title != "" {
		item, found, err := scanKVSRow(db.QueryRowContext(ctx, `
			SELECT e.ZMETADATAIDENTIFIER,
			       COALESCE(u.Z_OPT, 1),
			       COALESCE(u.ZBOOKMARKTIME, 0.0),
			       COALESCE(u.ZHASBEENPLAYED, 0),
			       COALESCE(u.ZPLAYCOUNT, 0),
			       COALESCE(u.ZTIMESTAMP, 0.0)
			FROM ZMTEPISODE e
			JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
			LEFT JOIN ZMTUPPMETADATA u ON u.ZMETADATAIDENTIFIER = e.ZMETADATAIDENTIFIER
			WHERE e.ZMETADATAIDENTIFIER IS NOT NULL
			  `+trackFilter+`AND p.ZSUBSCRIBED = 1
			  AND p.ZFEEDURL LIKE ? || '%'
			  AND LOWER(TRIM(e.ZTITLE)) = LOWER(TRIM(?))
			LIMIT 1`, ep.FeedURL, ep.Title))
		if err != nil {
			return kvsItem{}, false, fmt.Errorf("lookup by feed+title: %w", err)
		}
		if found {
			return item, true, nil
		}
	}

	return kvsItem{}, false, nil
}

func scanKVSRow(row *sql.Row) (kvsItem, bool, error) {
	var (
		metaID  string
		zopt    int
		bktm    float64
		hbplInt int
		plct    int
		tstm    float64
	)
	err := row.Scan(&metaID, &zopt, &bktm, &hbplInt, &plct, &tstm)
	if errors.Is(err, sql.ErrNoRows) {
		return kvsItem{}, false, nil
	}
	if err != nil {
		return kvsItem{}, false, err
	}
	return kvsItem{
		MetadataIdentifier: metaID,
		UPPVersion:         zopt,
		BookmarkTimeSec:    bktm,
		HasBeenPlayed:      hbplInt != 0,
		PlayCount:          plct,
		TimestampSec:       tstm,
	}, true, nil
}

// setKVSHeaders applies the standard iTunes Store KVS headers to req.
func (w *KVSWriter) setKVSHeaders(req *http.Request) {
	req.Header.Set("iCloud-DSID", w.dsid)
	req.Header.Set("X-DSID", w.dsid)
	req.Header.Set("X-Apple-Store-Front", "143441-1,42 t:podcasts1")
	req.Header.Set("X-Apple-Client-Application", "com.apple.podcasts")
	req.Header.Set("X-Apple-I-Locale", "en_US")
	req.Header.Set("X-Apple-I-Client-Time", time.Now().UTC().Format(time.RFC3339))
	req.Header.Set("Content-Type", "application/x-plist")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("User-Agent", "Podcasts/1.1.0 (Macintosh; OS X 27.0; 26A5353q) AppleWebKit/2625.1.18.11.5 AMS/1 (dt:1)")
}

// initSession calls getAll for both KVS domains:
//   - com.apple.upp: fetches current server-side version for every episode key.
//     These versions replace stale local SQLite Z_OPT values; without them,
//     putAll returns status=1198 when another device has synced since the last
//     Mac DB update.
//   - com.apple.podcasts: fetches per-feed episode play state (including
//     metadataIdentifier for each episode) and the subscription list. This
//     allows episode lookup without reading the local SQLite database.
func (w *KVSWriter) initSession(ctx context.Context) error {
	if w.sessionReady {
		return nil
	}
	// Best-effort: populate com.apple.podcasts state for SQLite-free episode
	// lookup. Log but do not fail if this call errors.
	if err := w.initPodcastsDomain(ctx); err != nil {
		fmt.Printf("apple/kvs: podcasts domain init failed (will fall back to SQLite): %v\n", err)
	}

	bodyXML := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n<dict>\n" +
		"\t<key>domain</key>\n\t<string>" + kvsDomain + "</string>\n" +
		"</dict>\n</plist>\n"

	body, err := xmlToBinaryPlist(bodyXML)
	if err != nil {
		return fmt.Errorf("getAll body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kvsGetEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("getAll request: %w", err)
	}
	w.setKVSHeaders(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("getAll: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("getAll HTTP %d", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	w.serverVersions, w.serverRawValues, _ = parseServerState(ctx, respBody)
	w.serverPlayStates = make(map[string]kvsServerState) // clear decoded cache on each refresh
	w.sessionReady = true
	return nil
}


// parseServerState extracts metadataIdentifier→version and metadataIdentifier→rawValue
// pairs from a getAll(com.apple.upp) binary plist response. Returns empty maps on failure.
func parseServerState(ctx context.Context, body []byte) (versions map[string]int, rawValues map[string][]byte, domainVersion int) {
	versions = make(map[string]int)
	rawValues = make(map[string][]byte)
	if len(body) == 0 {
		return
	}
	s, err := bplistToXML(ctx, body)
	if err != nil {
		return
	}

	// Narrow to the values array to avoid false matches on top-level keys.
	// Extract domain-version (global sequence counter for this domain).
	if dvStr := xmlIntegerFieldAfter(s, "<key>domain-version</key>"); dvStr != "" {
		if dv, err := strconv.Atoi(dvStr); err == nil {
			domainVersion = dv
		}
	}

	const valuesKey = "<key>values</key>"
	vi := strings.Index(s, valuesKey)
	if vi == -1 {
		return
	}
	s = s[vi+len(valuesKey):]
	arrayStart := strings.Index(s, "<array>")
	arrayEnd := strings.Index(s, "</array>")
	if arrayStart == -1 || arrayEnd == -1 || arrayEnd <= arrayStart {
		return
	}
	s = s[arrayStart+len("<array>") : arrayEnd]

	// Parse each <dict> block: extract key, version, and value.
	for {
		dictStart := strings.Index(s, "<dict>")
		dictEnd := strings.Index(s, "</dict>")
		if dictStart == -1 || dictEnd == -1 || dictEnd <= dictStart {
			break
		}
		block := s[dictStart+len("<dict>") : dictEnd]
		s = s[dictEnd+len("</dict>"):]

		metaID := xmlStringAfter(block, "<key>key</key>")
		if metaID == "" {
			continue
		}
		verStr := xmlStringAfter(block, "<key>version</key>")
		if v, err := strconv.Atoi(verStr); err == nil {
			versions[metaID] = v
		}
		if raw := xmlDataAfter(block, "<key>value</key>"); len(raw) > 0 {
			rawValues[metaID] = raw
		}
	}
	return
}

// xmlIntegerFieldAfter returns the content of the first <integer>…</integer>
// element that follows the literal tag within s (used for top-level plist integers).
func xmlIntegerFieldAfter(s, tag string) string {
	i := strings.Index(s, tag)
	if i == -1 {
		return ""
	}
	after := strings.TrimSpace(s[i+len(tag):])
	after = strings.TrimPrefix(after, "<integer>")
	if after == strings.TrimSpace(s[i+len(tag):]) {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(after, "<", 2)[0])
}

// xmlStringAfter returns the content of the first <string>…</string> element
// that follows the literal tag within s.
func xmlStringAfter(s, tag string) string {
	i := strings.Index(s, tag)
	if i == -1 {
		return ""
	}
	after := strings.TrimSpace(s[i+len(tag):])
	after = strings.TrimPrefix(after, "<string>")
	if after == s[i+len(tag):] { // no <string> prefix found
		return ""
	}
	return strings.SplitN(after, "<", 2)[0]
}

// decodeServerValue decodes a DEFLATE-compressed binary plist (as stored in
// serverRawValues) into a kvsServerState. The input is the already-base64-decoded
// compressed bytes returned by xmlDataAfter.
func decodeServerValue(ctx context.Context, compressed []byte) (kvsServerState, error) {
	fr := flate.NewReader(bytes.NewReader(compressed))
	inner, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		return kvsServerState{}, fmt.Errorf("deflate: %w", err)
	}
	s, err := bplistToXML(ctx, inner)
	if err != nil {
		return kvsServerState{}, fmt.Errorf("plist decode: %w", err)
	}
	var state kvsServerState
	if idx := strings.Index(s, "<key>hbpl</key>"); idx != -1 {
		after := strings.TrimSpace(s[idx+len("<key>hbpl</key>"):])
		state.HasBeenPlayed = strings.HasPrefix(after, "<true/>")
	}
	if idx := strings.Index(s, "<key>bktm</key>"); idx != -1 {
		after := strings.TrimSpace(s[idx+len("<key>bktm</key>"):])
		after = strings.TrimPrefix(after, "<real>")
		if valStr := strings.SplitN(after, "<", 2)[0]; valStr != after {
			if f, pErr := strconv.ParseFloat(valStr, 64); pErr == nil {
				state.BookmarkTimeSec = f
			}
		}
	}
	return state, nil
}

// checkServerPlayState returns the server-side play state for the given
// metadataIdentifier, lazily decoding the raw plist value on first access.
// Returns false if no server entry exists for this episode.
func (w *KVSWriter) checkServerPlayState(ctx context.Context, metaID string) (kvsServerState, bool) {
	if w.serverPlayStates == nil {
		return kvsServerState{}, false
	}
	if state, ok := w.serverPlayStates[metaID]; ok {
		return state, true
	}
	compressed, ok := w.serverRawValues[metaID]
	if !ok {
		return kvsServerState{}, false
	}
	state, err := decodeServerValue(ctx, compressed)
	if err != nil {
		return kvsServerState{}, false
	}
	w.serverPlayStates[metaID] = state
	return state, true
}

// serverStateCoversDesired reports whether the server's current play state
// already satisfies what we want to write, making the write a no-op.
func serverStateCoversDesired(ep model.EpisodeState, state kvsServerState) bool {
	switch ep.PlayState {
	case model.PlayStatePlayed:
		return state.HasBeenPlayed
	case model.PlayStateInProgress:
		desired := ep.PlayPosition.Seconds()
		return desired > 0 && state.BookmarkTimeSec >= desired-5 && state.BookmarkTimeSec > 0
	}
	return false
}

// kvsBatchSize is the maximum number of episodes per putAll request.
// The native app sends one episode per call; the protocol supports arrays but
// the server's payload limit is undocumented. 25 is a conservative default
// that keeps each request well under any reasonable limit while still being
// far more efficient than one-at-a-time for bulk migrations.
const kvsBatchSize = 25

// putAll sends all items to the KVS, chunked into groups of kvsBatchSize.
// It calls initSession (getAll) first to obtain current server-side versions,
// then overwrites each item's UPPVersion with the server value before sending.
// conflictKeys contains metadataIdentifiers whose writes were rejected with
// status=1198 (version conflict); the server's current state for those keys is
// merged into w.serverVersions and w.serverRawValues before returning.
func (w *KVSWriter) putAll(ctx context.Context, items []kvsItem) (conflictKeys map[string]struct{}, err error) {
	if err := w.initSession(ctx); err != nil {
		fmt.Printf("apple/kvs: session init (getAll) failed: %v\n", err)
	}
	// Use server-side versions as base-version to avoid 1198 conflicts.
	// The local SQLite Z_OPT is stale whenever another device has synced.
	for i := range items {
		if v, ok := w.serverVersions[items[i].MetadataIdentifier]; ok {
			items[i].UPPVersion = v
		}
	}
	for len(items) > 0 {
		n := kvsBatchSize
		if n > len(items) {
			n = len(items)
		}
		chunkConflicts, chunkErr := w.sendChunk(ctx, items[:n])
		if chunkErr != nil {
			return conflictKeys, chunkErr
		}
		if len(chunkConflicts) > 0 {
			if conflictKeys == nil {
				conflictKeys = make(map[string]struct{})
			}
			for k := range chunkConflicts {
				conflictKeys[k] = struct{}{}
			}
		}
		items = items[n:]
	}
	return conflictKeys, nil
}

// sendChunk sends a single putAll HTTP request for the given items (≤ kvsBatchSize).
// On status=1198 it merges the server's current state into w.serverVersions /
// w.serverRawValues and returns the conflicting keys (not an error).
func (w *KVSWriter) sendChunk(ctx context.Context, items []kvsItem) (conflictKeys map[string]struct{}, err error) {
	body, err := buildPutAllBody(items)
	if err != nil {
		return nil, fmt.Errorf("build body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kvsEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	w.setKVSHeaders(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("putAll HTTP: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("putAll HTTP %d: %s", resp.StatusCode, respBody)
	}

	s, xmlErr := bplistToXML(ctx, respBody)
	if xmlErr != nil {
		// response may not be a plist (unexpected but non-fatal).
		return nil, nil
	}
	statusIdx := strings.Index(s, "<key>status</key>")
	if statusIdx == -1 {
		return nil, nil
	}
	after := strings.TrimSpace(s[statusIdx+len("<key>status</key>"):])
	if !strings.HasPrefix(after, "<integer>") {
		return nil, nil
	}
	statusStr := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(after, "<integer>"), "<", 2)[0])

	switch statusStr {
	case "0":
		return nil, nil
	case "1198":
		// Version conflict: the server has these keys at a newer version than our
		// base-version=0. The response body contains the current server values —
		// parse them so the caller can check if the server state covers the desired
		// state (and skip the write) or needs a retry with the correct base-version.
		conflictVersions, conflictRaws, _ := parseServerState(ctx, respBody)
		if w.serverVersions == nil {
			w.serverVersions = make(map[string]int)
		}
		if w.serverRawValues == nil {
			w.serverRawValues = make(map[string][]byte)
		}
		conflicts := make(map[string]struct{}, len(conflictVersions))
		for k, v := range conflictVersions {
			w.serverVersions[k] = v
			conflicts[k] = struct{}{}
		}
		for k, v := range conflictRaws {
			w.serverRawValues[k] = v
		}
		w.serverPlayStates = make(map[string]kvsServerState) // invalidate decoded cache
		return conflicts, nil
	default:
		return nil, fmt.Errorf("putAll returned status=%s (session may have expired — reopen Apple Podcasts)", statusStr)
	}
}

// ---------------------------------------------------------------------------
// Plist body construction
// ---------------------------------------------------------------------------

// buildPutAllBody builds the binary plist body for a putAll request.
// Uses plutil to convert XML → binary (avoids a library dependency).
func buildPutAllBody(items []kvsItem) ([]byte, error) {
	var entries []string
	for _, item := range items {
		value, err := buildItemValue(item)
		if err != nil {
			return nil, fmt.Errorf("item %s: %w", item.MetadataIdentifier, err)
		}
		entries = append(entries, fmt.Sprintf(
			"\t\t<dict>\n"+
				"\t\t\t<key>base-version</key>\n"+
				"\t\t\t<string>%d</string>\n"+
				"\t\t\t<key>key</key>\n"+
				"\t\t\t<string>%s</string>\n"+
				"\t\t\t<key>value</key>\n"+
				"\t\t\t<data>%s</data>\n"+
				"\t\t</dict>",
			item.UPPVersion,
			item.MetadataIdentifier,
			base64.StdEncoding.EncodeToString(value),
		))
	}

	xmlPlist := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n" +
		"<dict>\n" +
		"\t<key>domain</key>\n" +
		"\t<string>" + kvsDomain + "</string>\n" +
		"\t<key>keys</key>\n" +
		"\t<array>\n" +
		strings.Join(entries, "\n") + "\n" +
		"\t</array>\n" +
		"</dict>\n" +
		"</plist>\n"

	return xmlToBinaryPlist(xmlPlist)
}

// buildItemValue builds the DEFLATE-compressed binary plist value for one item.
func buildItemValue(item kvsItem) ([]byte, error) {
	hbplTag := "<false/>"
	if item.HasBeenPlayed {
		hbplTag = "<true/>"
	}
	// Keys must be alphabetically sorted for a binary plist dict.
	xmlPlist := fmt.Sprintf(
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"+
			"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" "+
			"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n"+
			"<plist version=\"1.0\">\n"+
			"<dict>\n"+
			"\t<key>bktm</key>\n"+
			"\t<real>%.17g</real>\n"+
			"\t<key>hbpl</key>\n"+
			"\t%s\n"+
			"\t<key>plct</key>\n"+
			"\t<integer>%d</integer>\n"+
			"\t<key>tstm</key>\n"+
			"\t<real>%.17g</real>\n"+
			"</dict>\n"+
			"</plist>\n",
		item.BookmarkTimeSec, hbplTag, item.PlayCount, item.TimestampSec,
	)

	innerBinary, err := xmlToBinaryPlist(xmlPlist)
	if err != nil {
		return nil, fmt.Errorf("inner plist: %w", err)
	}

	// Raw DEFLATE — no zlib header, matching the native Podcasts app's encoding.
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("deflate init: %w", err)
	}
	if _, err := fw.Write(innerBinary); err != nil {
		fw.Close()
		return nil, fmt.Errorf("deflate write: %w", err)
	}
	if err := fw.Close(); err != nil {
		return nil, fmt.Errorf("deflate close: %w", err)
	}
	return buf.Bytes(), nil
}

// xmlToBinaryPlist converts an XML property list string to binary plist format
// using plutil, which is always available on macOS.
func xmlToBinaryPlist(xmlContent string) ([]byte, error) {
	cmd := exec.Command("plutil", "-convert", "binary1", "-o", "-", "-")
	cmd.Stdin = strings.NewReader(xmlContent)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("plutil: %s", exitErr.Stderr)
		}
		return nil, fmt.Errorf("plutil: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Cookie store parsing
// ---------------------------------------------------------------------------

// parsePodcastCookies reads an NSHTTPCookieStorage binarycookies file and
// returns the iTunes Store DSID and a Cookie: header value for
// bookkeeper.itunes.apple.com requests.
//
// The binarycookies format is Apple's custom binary cookie storage:
//
//	"cook" magic (4 bytes)
//	num_pages uint32 BE
//	page_sizes []uint32 BE
//	pages (each contains cookie records)
func parsePodcastCookies(path string) (dsid, cookieHeader string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	if len(data) < 8 || string(data[:4]) != "cook" {
		n := 4
		if len(data) < n {
			n = len(data)
		}
		return "", "", fmt.Errorf("not a binarycookies file (magic=%q)", data[:n])
	}

	numPages := int(binary.BigEndian.Uint32(data[4:8]))
	if len(data) < 8+numPages*4 {
		return "", "", fmt.Errorf("binarycookies truncated")
	}

	pageSizes := make([]int, numPages)
	for i := range pageSizes {
		pageSizes[i] = int(binary.BigEndian.Uint32(data[8+i*4 : 12+i*4]))
	}

	type cookie struct{ name, value, domain string }
	var cookies []cookie

	offset := 8 + numPages*4
	for _, pageSize := range pageSizes {
		if offset+pageSize > len(data) {
			break
		}
		page := data[offset : offset+pageSize]
		offset += pageSize

		if len(page) < 8 {
			continue
		}
		numCookies := int(binary.LittleEndian.Uint32(page[4:8]))

		for i := 0; i < numCookies; i++ {
			offIdx := 8 + i*4
			if offIdx+4 > len(page) {
				break
			}
			ckOffset := int(binary.LittleEndian.Uint32(page[offIdx : offIdx+4]))
			// Cookie record layout (little-endian):
			//   +0  size (4 bytes)
			//   +4  unknown (4 bytes)
			//   +8  flags (4 bytes)
			//   +12 unknown (4 bytes)
			//   +16 domain_offset from record start (4 bytes)
			//   +20 name_offset   from record start (4 bytes)
			//   +24 path_offset   from record start (4 bytes)
			//   +28 value_offset  from record start (4 bytes)
			//   +32 end marker    (8 bytes)
			//   +40 expiry        (float64 LE, CoreData epoch)
			//   +48 creation      (float64 LE, CoreData epoch)
			//   +56+ null-terminated strings
			if ckOffset+32 > len(page) {
				continue
			}
			ck := page[ckOffset:]

			domainOff := int(binary.LittleEndian.Uint32(ck[16:20]))
			nameOff := int(binary.LittleEndian.Uint32(ck[20:24]))
			valueOff := int(binary.LittleEndian.Uint32(ck[28:32]))

			domain := nullTermString(ck, domainOff)
			name := nullTermString(ck, nameOff)
			value := nullTermString(ck, valueOff)

			if strings.Contains(strings.ToLower(domain), "apple.com") {
				cookies = append(cookies, cookie{name: name, value: value, domain: domain})
			}
		}
	}

	var parts []string
	for _, c := range cookies {
		parts = append(parts, c.name+"="+c.value)
		if c.name == "X-Dsid" {
			dsid = c.value
		}
	}
	return dsid, strings.Join(parts, "; "), nil
}

func nullTermString(data []byte, offset int) string {
	if offset >= len(data) {
		return ""
	}
	end := bytes.IndexByte(data[offset:], 0)
	if end < 0 {
		return string(data[offset:])
	}
	return string(data[offset : offset+end])
}
