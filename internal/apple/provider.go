package apple

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// Provider implements provider.Provider for Apple Podcasts.
// It tries the local SQLite database first; if unavailable (e.g. permission
// denied or the path does not exist) it falls back to an OPML file.
//
// Reading:  SQLite (play state + subscriptions) with OPML fallback (subscriptions only).
// Writing:  SQLite play state writes are supported when the database is accessible.
//           Subscription writes are not supported (Apple Podcasts has no public write API
//           for subscriptions; use the GUI to subscribe).
type Provider struct {
	sqlitePath string
	opmlPath   string // optional fallback; empty disables it
}

// NewProvider returns an Apple Podcasts provider.
// sqlitePath defaults to DefaultSQLitePath() when empty.
// opmlPath is optional; pass empty string to disable the fallback.
func NewProvider(sqlitePath, opmlPath string) *Provider {
	if sqlitePath == "" {
		sqlitePath = DefaultSQLitePath()
	}
	return &Provider{sqlitePath: sqlitePath, opmlPath: opmlPath}
}

func (p *Provider) Name() string { return "Apple Podcasts" }

func (p *Provider) Capabilities() provider.Capabilities {
	// SQLite supports play state reads and writes; we report true and fail
	// gracefully at runtime if the DB is inaccessible.
	// OPML fallback only supports subscription reads (no play state, no writes).
	sqliteAccessible := false
	if p.sqlitePath != "" {
		if _, err := os.Stat(p.sqlitePath); err == nil {
			sqliteAccessible = true
		}
	}
	return provider.Capabilities{
		ReadSubscriptions: true,
		ReadPlayState:     true,
		// Play state writes require the live SQLite database.
		WritePlayState: sqliteAccessible,
		// Apple Podcasts has no public subscription write API.
		WriteSubscriptions: false,
	}
}

func (p *Provider) GetLibrary(ctx context.Context) (*model.Library, error) {
	if _, err := os.Stat(p.sqlitePath); err == nil {
		lib, err := NewSQLiteReader(p.sqlitePath).Read(ctx)
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

// SetLibrary writes episode play state to the Apple Podcasts SQLite database.
// Subscription writes are not supported.
func (p *Provider) SetLibrary(ctx context.Context, lib *model.Library, opts provider.WriteOptions) error {
	// Subscription writes are not supported regardless of options.
	if opts.OnlySubscriptions {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write subscriptions (Apple Podcasts has no public subscription write API)",
		}
	}

	// Play state writes require the SQLite database.
	if _, err := os.Stat(p.sqlitePath); err != nil {
		return fmt.Errorf("apple: SQLite database not accessible at %s: %w\n"+
			"  Play state writes require the live Apple Podcasts database.\n"+
			"  Ensure Apple Podcasts has been opened at least once on this Mac.", p.sqlitePath, err)
	}

	n, err := NewSQLiteWriter(p.sqlitePath).Write(ctx, lib, opts)
	if err != nil {
		return err
	}

	prefix := ""
	if opts.DryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("%supdated play state for %d episode(s) in Apple Podcasts\n", prefix, n)
	return nil
}
