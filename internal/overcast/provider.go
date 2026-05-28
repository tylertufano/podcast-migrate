package overcast

import (
	"context"
	"fmt"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// Provider implements provider.Provider for Overcast.
//
// Reading: parses the OPML export from overcast.fm/account/export_opml.
// Writing subscriptions: generates an OPML file the user imports via
//   Overcast > Settings > Import OPML.
// Writing play state: not yet supported (no public API).
type Provider struct {
	importOPMLPath string // path to existing Overcast OPML export (for reads)
	exportOPMLPath string // destination path for generated import file (for writes)
}

// NewProvider returns an Overcast provider.
// importOPMLPath is the path to an Overcast export file (for GetLibrary).
// exportOPMLPath is where the generated import file will be written (for SetLibrary).
func NewProvider(importOPMLPath, exportOPMLPath string) *Provider {
	return &Provider{
		importOPMLPath: importOPMLPath,
		exportOPMLPath: exportOPMLPath,
	}
}

func (p *Provider) Name() string { return "Overcast" }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ReadSubscriptions:  p.importOPMLPath != "",
		ReadPlayState:      p.importOPMLPath != "",
		WriteSubscriptions: p.exportOPMLPath != "",
		// No public API for writing play positions.
		WritePlayState: false,
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

func (p *Provider) SetLibrary(_ context.Context, lib *model.Library, opts provider.WriteOptions) error {
	if opts.OnlyPlayState {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write play state",
		}
	}
	if p.exportOPMLPath == "" {
		return &provider.ErrCapabilityUnsupported{
			Provider:  p.Name(),
			Operation: "write (no export OPML path configured)",
		}
	}
	if opts.DryRun {
		fmt.Printf("[dry-run] would write %d subscriptions to %s\n",
			len(lib.Podcasts), p.exportOPMLPath)
		return nil
	}
	return (&OPMLWriter{}).Write(lib, p.exportOPMLPath)
}
