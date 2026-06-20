package apple

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

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
	sqlitePath string
	opmlPath   string // optional fallback; empty disables it
	webAPI     *WebAPIWriter
	kvsOnly    *KVSWriter // set when using KVS without web API (KVS-only mode)
	sinceTime  time.Time  // when set, only episodes modified after this time are read
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
			lib, err := r.Read(ctx)
			if err == nil {
				return lib, nil
			}
			// SQLite read failed — log and fall through to OPML if available.
			fmt.Fprintf(os.Stderr, "apple: SQLite read failed (%v), falling back to OPML\n", err)
		}
	}

	if p.opmlPath == "" {
		if runtime.GOOS != "darwin" {
			return nil, errors.New("apple: Apple Podcasts is only available on macOS")
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
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write subscriptions standalone (subscriptions are auto-written during play state migration when KVS credentials are set; --only-subscriptions is not yet supported for Apple Podcasts)",
		}
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
				for _, pod := range lib.Podcasts {
					if pod.FeedURL == "" || kvs.IsSubscribed(pod.FeedURL) {
						continue
					}
					title := pod.Title
					if title == "" {
						title = pod.FeedURL
					}
					isNew, subErr := kvs.Subscribe(ctx, pod.FeedURL, title)
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
			for _, pod := range lib.Podcasts {
				if pod.FeedURL == "" || kvs.IsSubscribed(pod.FeedURL) {
					continue
				}
				title := pod.Title
				if title == "" {
					title = pod.FeedURL
				}
				isNew, subErr := kvs.Subscribe(ctx, pod.FeedURL, title)
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
