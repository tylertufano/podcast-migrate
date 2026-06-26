package apple

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/tyler/podcast-migrate/internal/itunes"
	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// Provider implements provider.Provider for Apple Podcasts.
// It tries the local SQLite database first; if unavailable (e.g. permission
// denied or the path does not exist) it falls back to an OPML file.
//
// Reading:  SQLite (play state + subscriptions) with OPML fallback (subscriptions only).
// Writing:  Two modes:
//
//	Web API + KVS: Bearer token + media-user-token handle public-catalog episodes
//	  via amp-api; KVS handles private/subscriber-feed episodes.
//	KVS-only: When no web API credentials are provided but KVS credentials are
//	  present (APPLE_KVS_DSID + APPLE_KVS_COOKIES), all episodes sync via KVS.
//	  This requires Apple Podcasts to index each feed before episode
//	  metadataIdentifiers are available, so newly subscribed feeds wait for
//	  indexing. Pre-existing subscriptions resolve immediately from SQLite.
type Provider struct {
	sqlitePath    string
	opmlPath      string      // optional fallback; empty disables it
	webAPI        *WebAPIWriter
	kvsOnly       *KVSWriter  // set when using KVS without web API (KVS-only mode)
	kvsReader     *KVSWriter  // set by EnableLiveKVSRead; used as live overlay on SQLite
	kvsOnlyReader *KVSReader  // set when SQLite unavailable; reads entirely from KVS + RSS
	sinceTime     time.Time   // when set, only episodes modified after this time are read
}

// SetSinceTime restricts GetLibrary to episodes whose play state was modified
// after t (uses ZPLAYSTATELASTMODIFIEDDATE). A zero t reads all episodes.
// Only effective when reading from SQLite; the OPML fallback ignores it.
func (p *Provider) SetSinceTime(t time.Time) { p.sinceTime = t }

// NewProvider returns an Apple Podcasts provider.
// sqlitePath defaults to DefaultSQLitePath() when empty.
// opmlPath is optional; pass empty string to disable the fallback.
func NewProvider(sqlitePath, opmlPath string) *Provider {
	if sqlitePath == "" {
		sqlitePath = DefaultSQLitePath()
	}
	return &Provider{sqlitePath: sqlitePath, opmlPath: opmlPath}
}

// SetWebAPICredentials configures the provider to write play state via the
// Apple Podcasts web API instead of directly to SQLite. bearerToken is the
// Authorization: Bearer value and mediaUserToken is the media-user-token header
// value, both obtained from a logged-in podcasts.apple.com browser session.
//
// KVS sync for private/subscriber-feed episodes activates automatically when
// APPLE_KVS_DSID and APPLE_KVS_COOKIES are set (capture them once via
// scripts/capture-kvs-creds.sh). A warning is printed when credentials are
// absent; catalog episodes are unaffected.
func (p *Provider) SetWebAPICredentials(bearerToken, mediaUserToken string) {
	p.webAPI = NewWebAPIWriter(bearerToken, mediaUserToken)

	kvs, err := NewKVSWriter(p.sqlitePath)
	if err != nil {
		fmt.Printf("apple: KVS sync unavailable — %v\n  Private/subscriber-feed episodes will not be synced.\n  Set APPLE_KVS_DSID and APPLE_KVS_COOKIES (see scripts/capture-kvs-creds.sh).\n", err)
		return
	}
	p.webAPI.SetKVSFallback(kvs)
	fmt.Printf("apple: KVS sync enabled (DSID %s) — private-feed episodes will sync via bookkeeper.itunes.apple.com\n", kvs.dsid)
}

// SetKVSOnlyMode activates KVS-only mode: all episodes (public and private)
// are synced via bookkeeper.itunes.apple.com without using the web API.
// Requires APPLE_KVS_DSID and APPLE_KVS_COOKIES env vars.
//
// Trade-off vs web API mode: newly subscribed public feeds must be indexed by
// the Apple Podcasts app before episodes can sync (same as private feeds).
// Pre-existing subscriptions resolve immediately from the local SQLite DB.
func (p *Provider) SetKVSOnlyMode() error {
	kvs, err := NewKVSWriter(p.sqlitePath)
	if err != nil {
		return err
	}
	kvs.AllFeeds = true
	p.kvsOnly = kvs
	fmt.Printf("apple: KVS sync enabled (DSID %s) — all episodes will sync via bookkeeper.itunes.apple.com\n", kvs.dsid)
	return nil
}

// EnableLiveKVSRead activates live KVS reading for GetLibrary: instead of
// using local ZMTUPPMETADATA rows, GetLibrary will call getAll(com.apple.upp)
// and use the server-side play state for each episode. This gives a more
// authoritative result when the Mac SQLite cache is stale.
//
// Returns nil without doing anything when APPLE_KVS_COOKIES is unset.
// Returns an error if credentials are present but invalid.
func (p *Provider) EnableLiveKVSRead() error {
	if os.Getenv("APPLE_KVS_COOKIES") == "" {
		return nil
	}
	kvs, err := NewKVSWriter(p.sqlitePath)
	if err != nil {
		return err
	}
	p.kvsReader = kvs
	return nil
}

// EnableKVSOnlyRead activates the cross-platform KVS+RSS reader, which reads
// subscriptions and play state from Apple's iCloud KVS and fetches episode
// metadata (title, pub date, duration) from the RSS feeds directly.
//
// Call this explicitly when running on non-macOS, or when you want to bypass
// the local SQLite database entirely. Returns nil when APPLE_KVS_COOKIES is
// unset (the path stays inactive). Returns an error if credentials are present
// but invalid.
func (p *Provider) EnableKVSOnlyRead() error {
	if os.Getenv("APPLE_KVS_COOKIES") == "" {
		return nil
	}
	r, err := NewKVSReader()
	if err != nil {
		return err
	}
	p.kvsOnlyReader = r
	return nil
}

func (p *Provider) Name() string { return "Apple Podcasts" }

func (p *Provider) Capabilities() provider.Capabilities {
	var kvsReady bool
	if p.webAPI != nil {
		kvsReady = p.webAPI.kvsWriter != nil
	}
	return provider.Capabilities{
		ReadSubscriptions: true,
		ReadPlayState:     true,
		WritePlayState:    p.webAPI != nil || p.kvsOnly != nil,
		WriteSubscriptions: kvsReady || p.kvsOnly != nil,
	}
}

func (p *Provider) GetLibrary(ctx context.Context) (*model.Library, error) {
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat(p.sqlitePath); err == nil {
			r := NewSQLiteReader(p.sqlitePath)
			if !p.sinceTime.IsZero() {
				r.SetSinceTime(p.sinceTime)
				fmt.Printf("apple: delta mode — reading episodes modified since %s\n",
					p.sinceTime.Local().Format("2006-01-02 15:04:05"))
			}
			if p.kvsReader != nil {
				if sessErr := p.kvsReader.initSession(ctx); sessErr == nil {
					r.SetLiveKVSValues(p.kvsReader.serverRawValues)
					fmt.Printf("apple: live KVS active (DSID %s) — fetched %d records from server\n",
						p.kvsReader.dsid, len(p.kvsReader.serverRawValues))
				} else {
					fmt.Fprintf(os.Stderr, "apple: live KVS read failed (%v) — using local ZMTUPPMETADATA\n", sessErr)
				}
			}
			lib, err := r.Read(ctx)
			if err == nil && p.kvsReader != nil && r.LiveKVSMatched > 0 {
				fmt.Printf("apple: play state — %d episodes matched live server\n", r.LiveKVSMatched)
			}
			if err == nil {
				return lib, nil
			}
			// SQLite read failed — fall through to KVS-only read or OPML.
			fmt.Fprintf(os.Stderr, "apple: SQLite read failed (%v), trying KVS-only read\n", err)
		}
	}

	// KVS-only read: no SQLite required (cross-platform).
	// Activated when APPLE_KVS_COOKIES is set and SQLite is unavailable.
	if p.kvsOnlyReader != nil {
		if !p.sinceTime.IsZero() {
			p.kvsOnlyReader.SetSinceTime(p.sinceTime)
		}
		return p.kvsOnlyReader.Read(ctx)
	}

	if p.opmlPath == "" {
		if runtime.GOOS != "darwin" {
			return nil, errors.New("apple: Apple Podcasts reading requires macOS or KVS credentials " +
				"(set APPLE_KVS_DSID and APPLE_KVS_COOKIES, then re-run)")
		}
		return nil, errors.New("apple: SQLite database not accessible and no OPML fallback path provided")
	}
	return NewOPMLReader(p.opmlPath).Read(ctx)
}

// SetLibrary writes episode play state to Apple Podcasts.
// In web API mode, requires SetWebAPICredentials to have been called.
// In KVS-only mode, requires SetKVSOnlyMode to have been called.
// Subscriptions are auto-written via KVS in both modes when credentials allow.
func (p *Provider) SetLibrary(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	if opts.OnlySubscriptions {
		return p.setLibrarySubscriptionsOnly(ctx, lib, opts)
	}

	if p.webAPI == nil && p.kvsOnly == nil {
		return fmt.Errorf("apple: no write credentials configured\n" +
			"  Option 1 — web API (recommended for large libraries):\n" +
			"    Set --apple-bearer-token and --apple-media-user-token (or APPLE_BEARER_TOKEN /\n" +
			"    APPLE_MEDIA_USER_TOKEN). Obtain from podcasts.apple.com: open DevTools, mark\n" +
			"    any episode as played, copy the Authorization and media-user-token headers.\n" +
			"  Option 2 — KVS-only (simpler credentials, slower for newly subscribed feeds):\n" +
			"    Set APPLE_KVS_DSID and APPLE_KVS_COOKIES (see scripts/capture-kvs-creds.sh).\n" +
			"    All episodes sync via KVS; feeds not yet indexed by Apple Podcasts wait for\n" +
			"    the app to refresh before their episodes can be written.")
	}

	if runtime.GOOS != "darwin" {
		return errors.New("apple: Apple Podcasts is only available on macOS")
	}

	// KVS-only mode: all episodes go through KVS.
	if p.webAPI == nil && p.kvsOnly != nil {
		return p.setLibraryKVSOnly(ctx, lib, opts)
	}

	// Web API mode (with optional KVS fallback for private feeds).
	return p.setLibraryWebAPI(ctx, lib, opts)
}

// kvsSubscribeAll subscribes all unsubscribed podcasts in pods via kvs.
//
// Private feeds are sorted to the front so they are subscribed before their
// public counterparts, ensuring that when a coexistence case is detected the
// public subscription is already present in the writer's subscription list.
//
// Dedup rules:
//   - Private incoming: skip if exact URL is subscribed, or if any active
//     subscription with the same normalised title is itself private-type.
//     When a public subscription already exists for the same title, subscribe
//     both and print a coexistence message with navigation links.
//   - Public incoming: skip if exact URL is subscribed, or if any active
//     subscription (public or private) has the same normalised title.
func kvsSubscribeAll(ctx context.Context, kvs *KVSWriter, pods []model.Podcast, dryRun bool, addedOut *int) {
	itunesClient := &http.Client{Timeout: 15 * time.Second}

	// Stable-sort: private feeds first. Source order is preserved within each group.
	sorted := make([]model.Podcast, len(pods))
	copy(sorted, pods)
	sort.SliceStable(sorted, func(i, j int) bool {
		iPriv := sorted[i].IsPrivate || model.IsSubscriberFeed(sorted[i].Title, sorted[i].FeedURL)
		jPriv := sorted[j].IsPrivate || model.IsSubscriberFeed(sorted[j].Title, sorted[j].FeedURL)
		return iPriv && !jPriv
	})

	for _, pod := range sorted {
		if pod.FeedURL == "" {
			continue
		}

		normTitle := model.NormalizePlusTitle(pod.Title)
		isPrivate := pod.IsPrivate || model.IsSubscriberFeed(pod.Title, pod.FeedURL)

		if isPrivate {
			// Private feed: skip if exact URL already subscribed, or if we already
			// have a private-type subscription for this title.
			if kvs.IsSubscribed(pod.FeedURL) || kvs.IsSubscribedByAnyPrivate(normTitle) {
				continue
			}

			title := pod.Title
			if title == "" {
				title = pod.FeedURL
			}

			// Check for coexistence: public subscription already present.
			pub := kvs.FindPublicByTitle(normTitle)

			if dryRun {
				if pub != nil {
					fmt.Printf("  [dry-run] kvs: would add subscriber feed %q alongside existing public subscription\n", title)
				} else {
					fmt.Printf("  [dry-run] kvs: would subscribe to %q (private)\n", title)
				}
				if addedOut != nil {
					*addedOut++
				}
				continue
			}

			isNew, subErr := kvs.Subscribe(ctx, pod.FeedURL, title, 0)
			if subErr != nil {
				fmt.Printf("  kvs: subscribe %q failed: %v\n", title, subErr)
				continue
			}
			if isNew {
				if addedOut != nil {
					*addedOut++
				}
				if pub != nil {
					fmt.Printf("  kvs: note: %q — added subscriber feed alongside existing public subscription.\n", title)
					fmt.Printf("    Public:  https://podcasts.apple.com/podcast/id%d\n", pub.PodcastPID)
					fmt.Printf("    Private: %s\n", pod.FeedURL)
					fmt.Printf("    Episode history will sync to the subscriber feed. Open Apple Podcasts to manage both.\n")
				} else {
					fmt.Printf("  kvs: subscribed to %q (private)\n", title)
				}
			}
			continue
		}

		// Public feed: skip if already subscribed by URL or by any title match.
		if kvs.IsSubscribed(pod.FeedURL) || kvs.IsSubscribedByTitle(normTitle) {
			continue
		}

		// Resolve iTunes PID and canonical URL for public catalog podcasts.
		feedURL := pod.FeedURL
		var podcastPID int64
		if pod.ITunesID != "" {
			podcastPID, _ = strconv.ParseInt(pod.ITunesID, 10, 64)
		} else if pod.Title != "" {
			if result, err := itunes.FindByHints(ctx, itunesClient, pod.Title, pod.FeedURL, pod.Author); err == nil && result.CollectionID > 0 {
				podcastPID = result.CollectionID
				if result.FeedURL != "" && result.FeedURL != feedURL {
					feedURL = result.FeedURL
					if kvs.IsSubscribed(feedURL) {
						continue
					}
				}
			}
		}

		title := pod.Title
		if title == "" {
			title = feedURL
		}
		if dryRun {
			fmt.Printf("  [dry-run] kvs: would subscribe to %q\n", title)
			if addedOut != nil {
				*addedOut++
			}
			continue
		}
		isNew, subErr := kvs.Subscribe(ctx, feedURL, title, podcastPID)
		if subErr != nil {
			fmt.Printf("  kvs: subscribe %q failed: %v\n", title, subErr)
			continue
		}
		if isNew {
			if addedOut != nil {
				*addedOut++
			}
			fmt.Printf("  kvs: subscribed to %q\n", title)
		}
	}
}

// setLibraryWebAPI is the web API path (with optional KVS fallback for private feeds).
func (p *Provider) setLibraryWebAPI(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	// Subscribe any unsubscribed podcasts via KVS before the play state pass.
	if !opts.DryRun && !opts.OnlyPlayState {
		kvs := p.webAPI.kvsWriter
		if kvs != nil {
			if !kvs.podcastsDomainReady {
				if iErr := kvs.initPodcastsDomain(ctx); iErr != nil {
					fmt.Printf("  kvs: podcasts domain init failed: %v\n", iErr)
				}
			}
			if kvs.podcastsDomainReady {
				kvsSubscribeAll(ctx, kvs, lib.Podcasts, false, opts.SubscriptionsAddedOut)
			}
		}
	}

	fmt.Println("apple: writing play state via web API (syncs to all devices)")
	n, err := p.webAPI.Write(ctx, lib, opts)
	if err != nil {
		return err
	}
	prefix := ""
	if opts.DryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("%smarked %d episode(s) as played via Apple Podcasts web API\n", prefix, n)
	return nil
}

// setLibraryKVSOnly is the KVS-only path: all episodes (public and private)
// are synced via KVS. Subscribes all feeds first so newly subscribed ones
// enter the deferred retry loop alongside private feeds.
func (p *Provider) setLibraryKVSOnly(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	kvs := p.kvsOnly

	// Subscribe all unsubscribed podcasts before the play state pass.
	if !opts.DryRun && !opts.OnlyPlayState {
		if !kvs.podcastsDomainReady {
			if iErr := kvs.initPodcastsDomain(ctx); iErr != nil {
				fmt.Printf("  kvs: podcasts domain init failed: %v\n", iErr)
			}
		}
		if kvs.podcastsDomainReady {
			kvsSubscribeAll(ctx, kvs, lib.Podcasts, false, opts.SubscriptionsAddedOut)
		}
	}

	fmt.Println("apple: writing play state via KVS (syncs to all devices)")
	n, err := kvs.Write(ctx, lib, opts)
	if err != nil {
		return err
	}
	prefix := ""
	if opts.DryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("%smarked %d episode(s) via Apple KVS\n", prefix, n)
	return nil
}

// setLibrarySubscriptionsOnly runs only the KVS subscribe pass, adding any
// podcasts from lib that are not yet subscribed in Apple Podcasts. No play
// state is written. Requires KVS credentials (either via SetKVSOnlyMode or
// the KVS fallback attached to a web API writer).
func (p *Provider) setLibrarySubscriptionsOnly(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	// Resolve which KVS writer to use.
	var kvs *KVSWriter
	switch {
	case p.kvsOnly != nil:
		kvs = p.kvsOnly
	case p.webAPI != nil && p.webAPI.kvsWriter != nil:
		kvs = p.webAPI.kvsWriter
	default:
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write subscriptions standalone (requires KVS credentials: set APPLE_KVS_DSID and APPLE_KVS_COOKIES)",
		}
	}

	if !kvs.podcastsDomainReady {
		if iErr := kvs.initPodcastsDomain(ctx); iErr != nil {
			return fmt.Errorf("apple: podcasts domain init failed: %w", iErr)
		}
	}
	if !kvs.podcastsDomainReady {
		return fmt.Errorf("apple: podcasts domain not available — check KVS credentials")
	}

	added := 0
	kvsSubscribeAll(ctx, kvs, lib.Podcasts, opts.DryRun, &added)

	if opts.SubscriptionsAddedOut != nil {
		*opts.SubscriptionsAddedOut = added
	}

	prefix := ""
	if opts.DryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("%ssubscribed to %d podcast(s) via Apple KVS\n", prefix, added)
	return nil
}
