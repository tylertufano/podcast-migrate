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
	// OPML fallback only supports subscriptions.
	// SQLite supports play state too; we report true and fail gracefully at
	// runtime if the DB is inaccessible.
	return provider.Capabilities{
		ReadSubscriptions: true,
		ReadPlayState:     true,
		// Apple Podcasts has no public write API.
		WriteSubscriptions: false,
		WritePlayState:     false,
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

func (p *Provider) SetLibrary(_ context.Context, _ *model.Library, _ provider.WriteOptions) error {
	return &provider.ErrCapabilityUnsupported{Provider: p.Name(), Operation: "write"}
}
