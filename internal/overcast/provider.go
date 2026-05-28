package overcast

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

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
		n, err := p.doWritePlayState(ctx, lib, opts.DryRun)
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
func (p *Provider) doWritePlayState(ctx context.Context, lib *model.Library, dryRun bool) (int, error) {
	// 1. Read the Overcast OPML to build a (pubDate+feedURL → overcastURL) index.
	//    We deliberately don't use overcastId as the GUID key here because it won't
	//    match the RSS GUIDs in lib (which came from Apple Podcasts or another source).
	overcastLib, err := NewOPMLReader(p.importOPMLPath).Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("overcast: read OPML for play state matching: %w", err)
	}

	index := buildOvercastIndex(overcastLib)

	// 2. Authenticate.
	if dryRun {
		// In dry-run mode, report what would be written without making any web requests.
		n := 0
		for _, ep := range lib.Episodes {
			if ep.PlayState == model.PlayStateUnplayed {
				continue
			}
			if _, ok := findInOvercastIndex(index, ep); ok {
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

	// 3. For each episode with play state, find its Overcast URL, fetch the numeric
	//    ID, and post set_progress. Failures on individual episodes are logged and
	//    skipped rather than aborting the whole run.
	numericIDCache := map[string]string{} // overcastURL → numericID
	updated := 0
	skipped := 0

	for i, ep := range lib.Episodes {
		if ep.PlayState == model.PlayStateUnplayed {
			continue
		}

		overcastURL, ok := findInOvercastIndex(index, ep)
		if !ok {
			skipped++
			continue
		}

		// Fetch numeric ID (cached to avoid re-fetching the same page).
		numericID, cached := numericIDCache[overcastURL]
		if !cached {
			id, err := FetchEpisodeNumericID(ctx, httpClient, overcastURL)
			if err != nil {
				fmt.Printf("  warning: episode %d/%d (%q): %v — skipping\n",
					i+1, len(lib.Episodes), ep.Title, err)
				skipped++
				continue
			}
			numericIDCache[overcastURL] = id
			numericID = id
		}

		pos := int(ep.PlayPosition.Seconds())
		if ep.PlayState == model.PlayStatePlayed {
			pos = PlayedSentinel
		}

		if err := SetProgress(ctx, httpClient, numericID, pos); err != nil {
			fmt.Printf("  warning: episode %d/%d (%q): set_progress failed: %v — skipping\n",
				i+1, len(lib.Episodes), ep.Title, err)
			skipped++
			continue
		}
		updated++
	}

	if skipped > 0 {
		fmt.Printf("overcast: %d episode(s) could not be matched or updated (unmatched or network error)\n", skipped)
	}
	return updated, nil
}

// overcastIndexEntry holds a resolved Overcast episode URL alongside its index keys.
type overcastIndexEntry struct {
	overcastURL string // "https://overcast.fm/+XXXXXXXX"
}

// buildOvercastIndex creates a lookup map from match keys to Overcast episode URLs.
// Each Overcast episode is indexed by pubDate+feedURL and title+feedURL (as fallback).
// The GUID key is intentionally omitted because the overcastId ≠ RSS GUID.
func buildOvercastIndex(lib *model.Library) map[string]overcastIndexEntry {
	index := make(map[string]overcastIndexEntry)
	for _, ep := range lib.Episodes {
		if ep.GUID == "" {
			continue // need overcastId to construct the URL
		}
		entry := overcastIndexEntry{
			overcastURL: overcastBaseURL + "/+" + ep.GUID,
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

// findInOvercastIndex looks up an episode using pubDate+feedURL then title+feedURL.
func findInOvercastIndex(index map[string]overcastIndexEntry, ep model.EpisodeState) (string, bool) {
	if !ep.PubDate.IsZero() && ep.FeedURL != "" {
		key := "feeddate:" + ep.FeedURL + "|" + ep.PubDate.UTC().Format(time.RFC3339)
		if entry, ok := index[key]; ok {
			return entry.overcastURL, true
		}
	}
	if ep.FeedURL != "" && ep.Title != "" {
		key := "feedtitle:" + ep.FeedURL + "|" + strings.ToLower(strings.TrimSpace(ep.Title))
		if entry, ok := index[key]; ok {
			return entry.overcastURL, true
		}
	}
	return "", false
}
