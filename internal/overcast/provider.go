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
	// 1. Read the Overcast extended OPML to build a (pubDate+feedURL → numericID) index.
	//    We match by pub date and feed URL rather than GUID because the overcastId in
	//    the OPML is Overcast's internal numeric ID, not the RSS <guid> value.
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

	// 3. For each episode with play state, look up its Overcast numeric ID and post
	//    set_progress. The numeric ID (overcastId) comes directly from the OPML and
	//    equals data-item-id — no per-episode page fetch needed.
	//    Failures on individual episodes are logged and skipped rather than aborting.

	// Pre-count so we can show X/total progress.
	toUpdate := 0
	for _, ep := range lib.Episodes {
		if ep.PlayState == model.PlayStateUnplayed {
			continue
		}
		if _, ok := findInOvercastIndex(index, ep); ok {
			toUpdate++
		}
	}
	fmt.Printf("overcast: writing play state for %d matched episode(s)...\n", toUpdate)

	updated := 0
	apiSkipped := 0

	for _, ep := range lib.Episodes {
		if ep.PlayState == model.PlayStateUnplayed {
			continue
		}

		numericID, ok := findInOvercastIndex(index, ep)
		if !ok {
			continue // unmatched — not in Overcast's history
		}

		pos := int(ep.PlayPosition.Seconds())
		if ep.PlayState == model.PlayStatePlayed {
			pos = PlayedSentinel
		}

		if err := SetProgress(ctx, httpClient, numericID, pos); err != nil {
			fmt.Printf("  [%d/%d] FAILED %q (id=%s): %v\n",
				updated+apiSkipped+1, toUpdate, ep.Title, numericID, err)
			apiSkipped++

			// If the first call already indicates an auth failure, abort immediately
			// rather than attempting the remaining ~1200 calls against a dead session.
			if updated == 0 && apiSkipped == 1 && strings.Contains(err.Error(), "login") {
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

		// Brief pause between requests — be polite to Overcast's servers and
		// avoid triggering any rate limiting on the unofficial endpoint.
		time.Sleep(80 * time.Millisecond)
	}

	if apiSkipped > 0 {
		fmt.Printf("overcast: %d episode(s) failed during write (see warnings above)\n", apiSkipped)
	}
	return updated, nil
}

// overcastIndexEntry holds the Overcast numeric ID needed to update an episode's progress.
// The numeric ID (overcastId from the OPML) equals the data-item-id HTML attribute and is
// used directly in POST /podcasts/set_progress/{numericID} — no page fetch required.
type overcastIndexEntry struct {
	numericID string // overcastId value, e.g. "2891974064154832"
}

// buildOvercastIndex creates a lookup map from match keys to Overcast numeric episode IDs.
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
		entry := overcastIndexEntry{numericID: ep.GUID}

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

// findInOvercastIndex looks up an episode by pubDate+feedURL then title+feedURL.
// Returns the Overcast numeric ID (for use in set_progress) and whether a match was found.
func findInOvercastIndex(index map[string]overcastIndexEntry, ep model.EpisodeState) (string, bool) {
	if !ep.PubDate.IsZero() && ep.FeedURL != "" {
		key := "feeddate:" + ep.FeedURL + "|" + ep.PubDate.UTC().Format(time.RFC3339)
		if entry, ok := index[key]; ok {
			return entry.numericID, true
		}
	}
	if ep.FeedURL != "" && ep.Title != "" {
		key := "feedtitle:" + ep.FeedURL + "|" + strings.ToLower(strings.TrimSpace(ep.Title))
		if entry, ok := index[key]; ok {
			return entry.numericID, true
		}
	}
	return "", false
}
