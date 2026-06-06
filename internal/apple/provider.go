package apple

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// Provider implements provider.Provider for Apple Podcasts.
// It tries the local SQLite database first; if unavailable (e.g. permission
// denied or the path does not exist) it falls back to an OPML file.
//
// Reading:  SQLite (play state + subscriptions) with OPML fallback (subscriptions only).
// Writing:  Play state is written via the amp-api.podcasts.apple.com web API, which
//           syncs to all devices (iPhone, iPad, Mac). Web API credentials must be
//           provided via SetWebAPICredentials before calling SetLibrary.
//           Subscription writes are not supported (Apple Podcasts has no public write API
//           for subscriptions; use the GUI to subscribe).
type Provider struct {
	sqlitePath string
	opmlPath   string // optional fallback; empty disables it
	webAPI     *WebAPIWriter
	sinceTime  time.Time // when set, only episodes modified after this time are read
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
func (p *Provider) SetWebAPICredentials(bearerToken, mediaUserToken string) {
	p.webAPI = NewWebAPIWriter(bearerToken, mediaUserToken)
}

func (p *Provider) Name() string { return "Apple Podcasts" }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ReadSubscriptions: true,
		ReadPlayState:     true,
		// Play state writes require web API credentials.
		WritePlayState: p.webAPI != nil,
		// Apple Podcasts has no public subscription write API.
		WriteSubscriptions: false,
	}
}

func (p *Provider) GetLibrary(ctx context.Context) (*model.Library, error) {
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

	if p.opmlPath == "" {
		return nil, errors.New("apple: SQLite database not accessible and no OPML fallback path provided")
	}
	return NewOPMLReader(p.opmlPath).Read(ctx)
}

// SetLibrary writes episode play state to Apple Podcasts via the web API.
// Web API credentials must be set via SetWebAPICredentials before calling.
// Subscription writes are not supported.
func (p *Provider) SetLibrary(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	if opts.OnlySubscriptions {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write subscriptions (Apple Podcasts has no public subscription write API)",
		}
	}

	if p.webAPI == nil {
		return fmt.Errorf("apple: web API credentials required for play state writes\n" +
			"  Set --apple-bearer-token and --apple-media-user-token (or APPLE_BEARER_TOKEN /\n" +
			"  APPLE_MEDIA_USER_TOKEN env vars). Obtain them from podcasts.apple.com:\n" +
			"  open DevTools, mark any episode as played, copy the Authorization and\n" +
			"  media-user-token headers from the network request.")
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
