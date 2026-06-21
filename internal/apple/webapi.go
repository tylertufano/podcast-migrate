package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// DefaultRequestDelay is the pause between consecutive Apple Podcasts web API
// requests when WriteOptions.RequestDelay is not set. 500 ms matches the
// Overcast default and keeps the client well within observed rate limits.
const DefaultRequestDelay = 500 * time.Millisecond

// WebAPIWriter marks episodes as played (or in-progress) via the Apple Podcasts
// web API (amp-api.podcasts.apple.com). This approach writes play state directly
// to Apple's backend, which then syncs to all devices (iPhone, iPad, Mac) via
// the PodcastContentService — unlike SQLite writes, which cannot cross the
// CoreData object graph boundary to trigger a sync push.
//
// Episode ID resolution uses the Apple catalog API (no local SQLite required):
//   - iTunes Search API (itunes.apple.com) to map podcast feed URL → collectionId
//   - amp-api catalog with full pagination to index all episodes for a podcast
//
// Two tokens are required, both obtainable by opening podcasts.apple.com in a
// browser, marking any episode as played, and inspecting the network request in
// DevTools:
//
//   - BearerToken:    the Authorization: Bearer <jwt> header value (app-level,
//     valid ~90 days, same for all users)
//   - MediaUserToken: the media-user-token header value (user-specific,
//     identifies the Apple Account)
//
// When kvsWriter is set, private/subscriber-feed episodes that cannot be
// resolved via the catalog fall back to the KVS putAll path.
type WebAPIWriter struct {
	bearerToken    string
	mediaUserToken string
	httpClient     *http.Client
	kvsWriter      *KVSWriter // optional; handles private-feed episodes
}

// SetKVSFallback registers a KVSWriter to handle private/subscriber-feed
// episodes (ZSTORETRACKID=0) that cannot be resolved via the Apple catalog.
func (w *WebAPIWriter) SetKVSFallback(kvs *KVSWriter) { w.kvsWriter = kvs }

// NewWebAPIWriter returns a writer that uses the Apple Podcasts web API.
// bearerToken and mediaUserToken are obtained from a logged-in podcasts.apple.com
// browser session (DevTools → mark any episode played → network request headers).
func NewWebAPIWriter(bearerToken, mediaUserToken string) *WebAPIWriter {
	return &WebAPIWriter{
		bearerToken:    bearerToken,
		mediaUserToken: mediaUserToken,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// Write iterates over lib's played and in-progress episodes, resolves each to
// an Apple catalog episode ID via the catalog API, checks current server state,
// and calls the playback-positions endpoint when an update is needed.
// Returns the number of episodes successfully written.
func (w *WebAPIWriter) Write(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	catalog := NewCatalogClient(w.bearerToken)

	// Resolve request delay: honour the caller's preference, or fall back to
	// the Apple default. Mirrors the same pattern used by the Overcast writer.
	requestDelay := opts.RequestDelay
	if requestDelay == 0 {
		requestDelay = DefaultRequestDelay
	}

	feedToTitle := migrate.BuildFeedToTitle(lib)
	if len(opts.PodcastFilter) > 0 {
		fmt.Printf("apple/webapi: podcast filter active — limiting to podcasts matching %q\n", opts.PodcastFilter)
	}
	fmt.Printf("apple/webapi: request delay: %v between calls\n", requestDelay)
	episodes := migrate.FilterEpisodesByPodcast(lib.Episodes, feedToTitle, opts.PodcastFilter)

	migrate.WriteLogHeader(opts.LogWriter)

	marked := 0
	skippedPlayed := 0 // server already has completed=true
	skippedAhead := 0  // server in-progress position >= source position
	notInCatalog := 0  // podcast not found in Apple catalog
	notMatched := 0    // podcast found but episode not matched

	// Private-feed episodes (CatalogPodcastNotInCatalog) are collected here and
	// sent in a single WriteBatch call after the catalog loop. The KVS session
	// token is single-use, so batching is required — sequential WriteEpisode
	// calls with the same token fail after the first with status 1198.
	var kvsEpisodes []model.EpisodeState

	for _, ep := range episodes {
		if ep.FromDestination {
			continue
		}
		if ep.PlayState != model.PlayStatePlayed && ep.PlayState != model.PlayStateInProgress {
			continue
		}
		if ep.FeedURL == "" {
			continue
		}

		podTitle := feedToTitle[ep.FeedURL]

		appleID, status, err := catalog.FindEpisode(ctx, ep, feedToTitle, opts.TitleMatchDateTolerance, opts.StrictFeedMatch)
		if err != nil {
			fmt.Printf("  warning: catalog lookup failed for %q — %q: %v\n", podTitle, ep.Title, err)
			migrate.WriteLogLine(opts.LogWriter, "error", podTitle, ep.Title, ep.PubDate,
				migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—", err.Error())
			continue
		}

		switch status {
		case CatalogPodcastNotInCatalog:
			// Private or subscriber feed — queue for KVS batch write below.
			// All private-feed episodes are batched into one putAll call because
			// the session token is single-use: sequential calls fail with 1198.
			if w.kvsWriter != nil {
				kvsEpisodes = append(kvsEpisodes, ep)
			} else {
				notInCatalog++
				migrate.WriteLogLine(opts.LogWriter, "no_apple_id", podTitle, ep.Title, ep.PubDate,
					migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—",
					"podcast not found in Apple catalog (private or unindexed feed)")
			}
			continue

		case CatalogEpisodeNotMatched:
			notMatched++
			migrate.WriteLogLine(opts.LogWriter, "not_found", podTitle, ep.Title, ep.PubDate,
				migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—",
				"episode not matched in Apple catalog")
			continue
		}

		// CatalogFound — appleID is valid.

		// Determine what we want to write.
		wantCompleted := ep.PlayState == model.PlayStatePlayed
		wantPositionMs := ep.PlayPosition.Milliseconds() // 0 when fully played

		if opts.DryRun {
			fmt.Printf("  [dry-run] would set position via web API: %q — %q (id=%d, completed=%v, pos=%dms)\n",
				podTitle, ep.Title, appleID, wantCompleted, wantPositionMs)
			migrate.WriteLogLine(opts.LogWriter, "would_update", podTitle, ep.Title, ep.PubDate,
				migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—", "")
			marked++
			continue
		}

		// preWriteLabel captures the Apple server state before any write, so the
		// log's target_state column is consistent with Overcast and Pocket Casts
		// (which also record the pre-write state). Falls back to "—" when
		// ForceUpdate skips the server check or when the check itself fails.
		preWriteLabel := "—"

		// Check current server state and skip if Apple is already at or beyond
		// the position we want to write. Skipped when ForceUpdate is set.
		if !opts.ForceUpdate {
			srvPos, err := w.getServerPosition(ctx, appleID)
			if err != nil {
				// Treat check failure as unknown — proceed with the write.
				fmt.Printf("  warning: could not check play status for %q (id=%d): %v — will update anyway\n",
					ep.Title, appleID, err)
			} else {
				switch {
				case srvPos.completed:
					preWriteLabel = "played"
				case srvPos.recorded && srvPos.positionMs > 0:
					preWriteLabel = fmt.Sprintf("in_progress(%s)", (time.Duration(srvPos.positionMs)*time.Millisecond).Round(time.Second))
				default:
					preWriteLabel = "unplayed"
				}

				if srvPos.completed {
					// Server already has the episode marked as played — nothing to do
					// regardless of whether we wanted to mark it played or in-progress.
					skippedPlayed++
					migrate.WriteLogLine(opts.LogWriter, "already_played", podTitle, ep.Title, ep.PubDate,
						migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "played", "")
					continue
				}
				if !wantCompleted && srvPos.recorded && srvPos.positionMs >= wantPositionMs {
					// We want to set an in-progress position but the server is already
					// at least as far along — skip to avoid rewinding.
					skippedAhead++
					migrate.WriteLogLine(opts.LogWriter, "already_ahead", podTitle, ep.Title, ep.PubDate,
						migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition),
						fmt.Sprintf("in_progress(%s)", (time.Duration(srvPos.positionMs)*time.Millisecond).Round(time.Second)),
						"server position is at or ahead of source")
					continue
				}
			}
		}

		if err := w.markPosition(ctx, appleID, wantPositionMs, wantCompleted); err != nil {
			fmt.Printf("  warning: web API call failed for %q (id=%d): %v\n",
				ep.Title, appleID, err)
			migrate.WriteLogLine(opts.LogWriter, "error", podTitle, ep.Title, ep.PubDate,
				migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), "—", err.Error())
			continue
		}

		migrate.WriteLogLine(opts.LogWriter, "updated", podTitle, ep.Title, ep.PubDate,
			migrate.PlayStateLabel(ep.PlayState, ep.PlayPosition), preWriteLabel, "")
		marked++

		select {
		case <-ctx.Done():
			return marked, ctx.Err()
		case <-time.After(requestDelay):
		}
	}

	// Flush private-feed episodes via a single KVS putAll.
	if len(kvsEpisodes) > 0 {
		kvsMarked, err := w.kvsWriter.WriteBatch(ctx, kvsEpisodes, feedToTitle, opts)
		if err != nil {
			fmt.Printf("apple/kvs: batch write failed: %v\n", err)
		}
		marked += kvsMarked
	}

	if skippedPlayed > 0 {
		fmt.Printf("apple/webapi: %d episode(s) skipped — already marked as played on server\n", skippedPlayed)
	}
	if skippedAhead > 0 {
		fmt.Printf("apple/webapi: %d episode(s) skipped — server position already at or ahead of source\n", skippedAhead)
	}
	if notInCatalog > 0 {
		fmt.Printf("apple/webapi: %d episode(s) skipped — podcast not found in Apple catalog (private/unindexed feed)\n", notInCatalog)
	}
	if notMatched > 0 {
		fmt.Printf("apple/webapi: %d episode(s) not matched in Apple catalog\n", notMatched)
	}

	return marked, nil
}

// serverPosition holds the play state returned by the GET playback-positions
// endpoint for a single episode.
type serverPosition struct {
	recorded   bool  // false when the server has no record (404)
	completed  bool
	positionMs int64 // 0 when not started or fully played
}

// getServerPosition calls GET /v1/me/playback/positions/podcast-episodes/{id}
// and returns the server's current play state for that episode.
//
// A 404 means no position has been recorded (never started); the returned
// serverPosition has recorded=false. Any other HTTP error is returned as an
// error; the caller should proceed with the write rather than silently skipping.
func (w *WebAPIWriter) getServerPosition(ctx context.Context, appleEpisodeID int64) (serverPosition, error) {
	url := fmt.Sprintf(
		"https://amp-api.podcasts.apple.com/v1/me/playback/positions/podcast-episodes/%d?with=entitlements%%2ChlsVideo&l=en-US",
		appleEpisodeID,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return serverPosition{}, fmt.Errorf("get position: build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+w.bearerToken)
	req.Header.Set("media-user-token", w.mediaUserToken)
	req.Header.Set("Origin", "https://podcasts.apple.com")
	req.Header.Set("Referer", "https://podcasts.apple.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return serverPosition{}, fmt.Errorf("get position: %w", err)
	}
	defer resp.Body.Close()

	// 404 means no position recorded at all.
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return serverPosition{recorded: false}, nil
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return serverPosition{}, fmt.Errorf("get position: HTTP %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Data []struct {
			Attributes struct {
				Completed              bool  `json:"completed"`
				PositionInMilliseconds int64 `json:"positionInMilliseconds"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		// Treat a parse error as unknown — proceed with the write.
		return serverPosition{recorded: false}, nil
	}

	if len(result.Data) == 0 {
		return serverPosition{recorded: false}, nil
	}

	a := result.Data[0].Attributes
	return serverPosition{
		recorded:   true,
		completed:  a.Completed,
		positionMs: a.PositionInMilliseconds,
	}, nil
}

// markPosition calls PUT /v1/me/playback/positions/podcast-episodes/{id} to
// set the episode's play state on Apple's backend, which syncs to all devices.
//
// Use completed=true with positionMs=0 to mark as played.
// Use completed=false with positionMs>0 to set an in-progress position.
//
// 5xx responses are retried up to maxMarkRetries times with exponential backoff
// since Apple's Podcast Cloud Library service occasionally returns transient
// upstream errors (code 50001).
func (w *WebAPIWriter) markPosition(ctx context.Context, appleEpisodeID, positionMs int64, completed bool) error {
	const maxRetries = 3
	const retryBaseDelay = 2 * time.Second

	type attributes struct {
		PositionInMilliseconds int64  `json:"positionInMilliseconds"`
		RecordedAtTimestamp    string `json:"recordedAtTimestamp"`
		Completed              bool   `json:"completed"`
	}
	type payload struct {
		Type       string     `json:"type"`
		Attributes attributes `json:"attributes"`
	}

	url := fmt.Sprintf(
		"https://amp-api.podcasts.apple.com/v1/me/playback/positions/podcast-episodes/%d?with=entitlements%%2ChlsVideo&l=en-US",
		appleEpisodeID,
	)

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := retryBaseDelay * (1 << (attempt - 1)) // 2s, 4s, 8s
			fmt.Printf("    retrying in %v (attempt %d/%d)...\n", delay, attempt, maxRetries)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		body, err := json.Marshal(payload{
			Type: "playback-positions",
			Attributes: attributes{
				PositionInMilliseconds: positionMs,
				RecordedAtTimestamp:    time.Now().UTC().Format(time.RFC3339Nano),
				Completed:              completed,
			},
		})
		if err != nil {
			return fmt.Errorf("marshal payload: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+w.bearerToken)
		req.Header.Set("media-user-token", w.mediaUserToken)
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		req.Header.Set("Origin", "https://podcasts.apple.com")
		req.Header.Set("Referer", "https://podcasts.apple.com/")
		req.Header.Set("x-apple-client-version", "2622.3.0-external")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15")

		resp, err := w.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP PUT: %w", err)
			continue // network error — retry
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
			continue // transient server error — retry
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody)) // client error — don't retry
		}

		return nil // success
	}

	return fmt.Errorf("after %d attempts: %w", maxRetries+1, lastErr)
}
