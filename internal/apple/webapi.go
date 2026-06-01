package apple

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// DefaultRequestDelay is the pause between consecutive Apple Podcasts web API
// requests when WriteOptions.RequestDelay is not set. 500 ms matches the
// Overcast default and keeps the client well within observed rate limits.
const DefaultRequestDelay = 500 * time.Millisecond

// WebAPIWriter marks episodes as played via the Apple Podcasts web API
// (amp-api.podcasts.apple.com). This approach writes play state directly to
// Apple's backend, which then syncs to all devices (iPhone, iPad, Mac) via the
// PodcastContentService — unlike SQLite writes, which cannot cross the CoreData
// object graph boundary to trigger a sync push.
//
// Two tokens are required, both obtainable by opening podcasts.apple.com in a
// browser, marking any episode as played, and inspecting the network request in
// DevTools:
//
//   - BearerToken:    the Authorization: Bearer <jwt> header value (app-level,
//     valid ~90 days, same for all users)
//   - MediaUserToken: the media-user-token header value (user-specific,
//     identifies the Apple Account)
type WebAPIWriter struct {
	dbPath         string
	bearerToken    string
	mediaUserToken string
	httpClient     *http.Client
}

// NewWebAPIWriter returns a writer that uses the Apple Podcasts web API.
// dbPath is the path to MTLibrary.sqlite, opened read-only for episode lookup.
func NewWebAPIWriter(dbPath, bearerToken, mediaUserToken string) *WebAPIWriter {
	return &WebAPIWriter{
		dbPath:         dbPath,
		bearerToken:    bearerToken,
		mediaUserToken: mediaUserToken,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// Write iterates over lib's played episodes, resolves each to an Apple catalog
// episode ID via SQLite, and calls the playback-positions API to mark it played.
// Returns the number of episodes successfully marked.
func (w *WebAPIWriter) Write(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	// Open SQLite read-only — we only need it for ZSTORETRACKID lookups.
	db, err := sql.Open("sqlite", "file:"+w.dbPath+"?mode=ro&_journal=off")
	if err != nil {
		return 0, fmt.Errorf("apple/webapi: open SQLite %s: %w", w.dbPath, err)
	}
	defer db.Close()

	index, err := buildAppleIndex(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("apple/webapi: build index: %w", err)
	}

	// Resolve request delay: honour the caller's preference, or fall back to
	// the Apple default. Mirrors the same pattern used by the Overcast writer.
	requestDelay := opts.RequestDelay
	if requestDelay == 0 {
		requestDelay = DefaultRequestDelay
	}

	feedToTitle := buildFeedToTitleFromLib(lib)
	if len(opts.PodcastFilter) > 0 {
		fmt.Printf("apple/webapi: podcast filter active — limiting to podcasts matching %q\n", opts.PodcastFilter)
	}
	fmt.Printf("apple/webapi: request delay: %v between calls\n", requestDelay)
	episodes := filterLibraryEpisodes(lib.Episodes, feedToTitle, opts.PodcastFilter)

	writeLogHeader(opts.LogWriter)

	marked := 0
	notFound := 0
	noID := 0

	for _, ep := range episodes {
		if ep.FromDestination {
			continue
		}
		if ep.PlayState != model.PlayStatePlayed {
			continue
		}
		if ep.FeedURL == "" {
			continue
		}

		podTitle := feedToTitle[ep.FeedURL]

		appleRec, ok := findInAppleIndex(index, ep, feedToTitle, opts.TitleMatchDateTolerance)
		if !ok {
			notFound++
			writeLogLine(opts.LogWriter, "not_found", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—",
				"no match in Apple Podcasts database")
			continue
		}

		if appleRec.storeTrackID == 0 {
			noID++
			writeLogLine(opts.LogWriter, "no_apple_id", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—",
				"episode has no Apple catalog ID (private or unindexed feed)")
			continue
		}

		// Note: we intentionally do NOT call applySatisfied here. The local
		// SQLite state is not authoritative for the web API path — Apple's
		// server is. Previous SQLite-write attempts may have left behind
		// stale local state (e.g. ZPLAYSTATE=2/source=3) that looks
		// satisfied locally but was never pushed to the server. The PUT is
		// idempotent, so re-marking an already-played episode is harmless.

		if opts.DryRun {
			fmt.Printf("  [dry-run] would mark via web API: %q — %q (id=%d)\n",
				podTitle, ep.Title, appleRec.storeTrackID)
			writeLogLine(opts.LogWriter, "would_update", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition),
				playStateLabel(appleRec.playState, appleRec.playPosition), "")
			marked++
			continue
		}

		if err := w.markPlayed(ctx, appleRec.storeTrackID); err != nil {
			fmt.Printf("  warning: web API call failed for %q (id=%d): %v\n",
				ep.Title, appleRec.storeTrackID, err)
			writeLogLine(opts.LogWriter, "error", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition),
				playStateLabel(appleRec.playState, appleRec.playPosition), err.Error())
			continue
		}

		writeLogLine(opts.LogWriter, "updated", podTitle, ep.Title, ep.PubDate,
			playStateLabel(ep.PlayState, ep.PlayPosition),
			playStateLabel(appleRec.playState, appleRec.playPosition), "")
		marked++

		select {
		case <-ctx.Done():
			return marked, ctx.Err()
		case <-time.After(requestDelay):
		}
	}

	if notFound > 0 {
		fmt.Printf("apple/webapi: %d episode(s) not found in local Apple Podcasts database\n", notFound)
	}
	if noID > 0 {
		fmt.Printf("apple/webapi: %d episode(s) skipped — no Apple catalog ID (private/unindexed feed)\n", noID)
	}

	return marked, nil
}

// markPlayed calls PUT /v1/me/playback/positions/podcast-episodes/{id} with
// completed=true, which Apple's backend uses to sync the played state to all
// devices associated with the account.
func (w *WebAPIWriter) markPlayed(ctx context.Context, appleEpisodeID int64) error {
	type attributes struct {
		PositionInMilliseconds int    `json:"positionInMilliseconds"`
		RecordedAtTimestamp    string `json:"recordedAtTimestamp"`
		Completed              bool   `json:"completed"`
	}
	type payload struct {
		Type       string     `json:"type"`
		Attributes attributes `json:"attributes"`
	}

	body, err := json.Marshal(payload{
		Type: "playback-positions",
		Attributes: attributes{
			PositionInMilliseconds: 0,
			RecordedAtTimestamp:    time.Now().UTC().Format(time.RFC3339Nano),
			Completed:              true,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	url := fmt.Sprintf(
		"https://amp-api.podcasts.apple.com/v1/me/playback/positions/podcast-episodes/%d?with=entitlements%%2ChlsVideo&l=en-US",
		appleEpisodeID,
	)

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
		return fmt.Errorf("HTTP PUT: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
