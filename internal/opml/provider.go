package opml

import (
	"context"
	"fmt"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// Provider implements provider.Provider for generic OPML files.
//
// As a source (GetLibrary): reads the OPML file at sourcePath, returning
// subscriptions and — if the file contains episode outlines — play state.
//
// As a destination (SetLibrary): writes the library to outputPath.
// Episodes (play state) are included when the provider was created with
// extended=true and opts.OnlySubscriptions is not set.
type Provider struct {
	sourcePath string // read path for GetLibrary
	outputPath string // write path for SetLibrary
	extended   bool   // write extended OPML with episode play state
}

// NewSourceProvider returns a Provider that reads from path.
func NewSourceProvider(path string) *Provider {
	return &Provider{sourcePath: path}
}

// NewOutputProvider returns a Provider that writes to path.
// When extended is true, episode play state is included in the output.
func NewOutputProvider(path string, extended bool) *Provider {
	return &Provider{outputPath: path, extended: extended}
}

func (p *Provider) Name() string { return "OPML" }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ReadSubscriptions:  p.sourcePath != "",
		ReadPlayState:      p.sourcePath != "",
		WriteSubscriptions: p.outputPath != "",
		WritePlayState:     p.outputPath != "" && p.extended,
	}
}

// GetLibrary reads the OPML file and returns the library.
// Both standard OPML (subscriptions only) and extended OPML (with play state) are supported.
func (p *Provider) GetLibrary(_ context.Context) (*model.Library, error) {
	if p.sourcePath == "" {
		return nil, &provider.ErrCapabilityUnsupported{Provider: "OPML", Operation: "read (no source path configured)"}
	}
	return NewReader(p.sourcePath).Read()
}

// SetLibrary writes the library as an OPML file.
// Play state (episode outlines) is included when the provider was created with
// extended=true and opts.OnlySubscriptions is not set.
func (p *Provider) SetLibrary(_ context.Context, lib *model.Library, opts provider.WriteOptions) error {
	if p.outputPath == "" {
		return &provider.ErrCapabilityUnsupported{Provider: "OPML", Operation: "write (no output path configured)"}
	}

	writeEpisodes := p.extended && !opts.OnlySubscriptions

	if opts.DryRun {
		episodeCount := 0
		if writeEpisodes {
			for _, ep := range lib.Episodes {
				if !ep.FromDestination && ep.PlayState != model.PlayStateUnplayed {
					episodeCount++
				}
			}
		}
		if writeEpisodes {
			fmt.Printf("opml: dry-run — would write %d podcast(s) and %d played/in-progress episode(s) to %s\n",
				len(lib.Podcasts), episodeCount, p.outputPath)
		} else {
			fmt.Printf("opml: dry-run — would write %d podcast(s) (subscriptions only) to %s\n",
				len(lib.Podcasts), p.outputPath)
		}
		return nil
	}

	w := &Writer{Extended: writeEpisodes}
	if err := w.Write(lib, p.outputPath); err != nil {
		return err
	}

	if writeEpisodes {
		episodeCount := 0
		for _, ep := range lib.Episodes {
			if !ep.FromDestination && ep.PlayState != model.PlayStateUnplayed {
				episodeCount++
			}
		}
		fmt.Printf("opml: wrote %d podcast(s) and %d played/in-progress episode(s) to %s\n",
			len(lib.Podcasts), episodeCount, p.outputPath)
	} else {
		fmt.Printf("opml: wrote %d podcast(s) (subscriptions only) to %s\n",
			len(lib.Podcasts), p.outputPath)
	}
	return nil
}
