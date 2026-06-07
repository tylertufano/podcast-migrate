package pocketcasts

// web.go implements the Pocket Casts API used for reading and writing episode
// play state and podcast subscriptions.
//
// Pocket Casts has no official public API. This implementation targets the
// JSON endpoints used by the Pocket Casts web player and may break without
// notice if Pocket Casts changes its backend.
//
// Authentication uses a Bearer token obtained from POST /user/login. All
// subsequent requests inject the token via a custom http.RoundTripper so
// callers use the returned *http.Client normally without per-call setup.
//
// Endpoints used:
//   POST https://api.pocketcasts.com/user/login                              — authenticate, get Bearer token
//   POST https://api.pocketcasts.com/user/podcast/list                       — subscribed podcast list
//   POST https://api.pocketcasts.com/user/in_progress                        — in-progress episodes (with play state)
//   POST https://api.pocketcasts.com/user/history                            — recently-played episodes (with play state)
//   GET  https://cache.pocketcasts.com/podcast/full/{uuid}/{page}/3/1000     — paginated episode metadata (no play state)
//   POST https://api.pocketcasts.com/sync/update_episode                     — set play position/status
//   POST https://api.pocketcasts.com/user/podcast/subscribe                  — subscribe to a podcast by UUID
//   POST https://refresh.pocketcasts.com/author/add_feed_url                 — resolve RSS feed URL → podcast UUID (public, no auth)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var pcBaseURL     = "https://api.pocketcasts.com"
var pcCacheURL    = "https://cache.pocketcasts.com"
var pcRefreshURL  = "https://refresh.pocketcasts.com"
var pcPollInterval = 2 * time.Second

// SetBaseURLForTest overrides the Pocket Casts API base URL for unit tests
// that spin up an httptest.Server. Must be reset after each test.
func SetBaseURLForTest(u string) { pcBaseURL = u }

// SetCacheURLForTest overrides the Pocket Casts cache CDN base URL for unit
// tests. Must be reset after each test.
func SetCacheURLForTest(u string) { pcCacheURL = u }

// SetRefreshURLForTest overrides the Pocket Casts feed-resolution service URL
// for unit tests. Must be reset after each test.
func SetRefreshURLForTest(u string) { pcRefreshURL = u }

// SetPollIntervalForTest overrides the polling delay used by
// ResolveFeedToPodcastUUID so tests complete without sleeping.
func SetPollIntervalForTest(d time.Duration) { pcPollInterval = d }

const (
	pcUA = "podcast-migrate/1.0 (github.com/tyler/podcast-migrate)"

	// Playing status constants mirror the values used by the Pocket Casts API.
	PlayingUnplayed   = 1 // episode not started
	PlayingInProgress = 2 // episode partially listened to
	PlayingPlayed     = 3 // episode listened to completion
)

// RateLimitError is returned when Pocket Casts responds with HTTP 429.
// The Wait field holds how long to pause before the next attempt.
type RateLimitError struct {
	Wait time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("pocketcasts/web: rate limited (HTTP 429) — retry after %v", e.Wait)
}

// TransientError is returned when an API call receives a 5xx response or a
// network-level failure — both of which may succeed on a subsequent attempt.
type TransientError struct {
	cause error
}

func (e *TransientError) Error() string { return e.cause.Error() }
func (e *TransientError) Unwrap() error { return e.cause }

// bearerTransport injects an Authorization: Bearer header on every outgoing
// request. It is set as the Transport on the *http.Client returned by Login.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "Bearer "+t.token)
	if r2.Header.Get("User-Agent") == "" {
		r2.Header.Set("User-Agent", pcUA)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r2)
}

// APIPodcast is a podcast as returned by /user/podcast/list.
type APIPodcast struct {
	UUID   string `json:"uuid"`
	Title  string `json:"title"`
	Author string `json:"author"`
	URL    string `json:"url"` // RSS feed URL
}

// APIEpisode is an episode as returned by episode list endpoints.
// The URL field is the audio enclosure URL — not the RSS <guid> value.
// Episode matching against other providers must rely on PublishedAt + podcast
// feed URL (or Title as a fallback) rather than the enclosure URL.
type APIEpisode struct {
	UUID          string `json:"uuid"`
	PodcastUUID   string `json:"podcast_uuid"`
	Title         string `json:"title"`
	URL           string `json:"url"`
	PublishedAt   string `json:"published_at"`   // ISO 8601 UTC, e.g. "2024-03-15T10:00:00Z"
	Duration      int    `json:"duration"`        // seconds
	PlayingStatus int    `json:"playing_status"`  // 1=unplayed, 2=in-progress, 3=played
	PlayedUpTo    int    `json:"played_up_to"`    // seconds
	Starred       bool   `json:"starred"`
	IsDeleted     bool   `json:"is_deleted"`
}

// ParsePublishedAt parses the episode's PublishedAt field as a UTC time.
// Accepts ISO 8601 / RFC3339 format (e.g. "2024-03-15T10:00:00Z").
// Returns the zero time if the field is empty or cannot be parsed.
func (e *APIEpisode) ParsePublishedAt() time.Time {
	if e.PublishedAt == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, e.PublishedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// rateLimitWait extracts the Retry-After delay from a 429 response.
func rateLimitWait(resp *http.Response, defaultWait time.Duration) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultWait
}

// Login authenticates with Pocket Casts and returns an *http.Client whose
// every request automatically carries the session Bearer token. The client
// must be reused for all subsequent API calls within the same migration run.
func Login(ctx context.Context, email, password string) (*http.Client, error) {
	type loginReq struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Scope    string `json:"scope"`
	}
	type loginResp struct {
		Token string `json:"token"`
	}

	reqBody, err := json.Marshal(loginReq{Email: email, Password: password, Scope: "webplayer"})
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: marshal login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/user/login", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", pcUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: login POST: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("pocketcasts/web: login failed (HTTP %d) — check credentials", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pocketcasts/web: login returned HTTP %d — check credentials", resp.StatusCode)
	}

	var lr loginResp
	if err := json.Unmarshal(respBody, &lr); err != nil {
		return nil, fmt.Errorf("pocketcasts/web: parse login response: %w", err)
	}
	if lr.Token == "" {
		return nil, fmt.Errorf("pocketcasts/web: login succeeded (HTTP 200) but no token in response — check credentials")
	}

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &bearerTransport{token: lr.Token},
	}
	return client, nil
}

// FetchSubscribedPodcasts returns all podcasts the authenticated user is
// currently subscribed to, including their RSS feed URLs.
func FetchSubscribedPodcasts(ctx context.Context, client *http.Client) ([]APIPodcast, error) {
	reqBody, _ := json.Marshal(map[string]int{"v": 1})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/user/podcast/list", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: build subscribed-podcasts request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: POST /user/podcast/list: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, &TransientError{cause: fmt.Errorf("pocketcasts/web: /user/podcast/list returned HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pocketcasts/web: /user/podcast/list returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	var payload struct {
		Podcasts []APIPodcast `json:"podcasts"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("pocketcasts/web: parse subscribed-podcasts response: %w", err)
	}
	return payload.Podcasts, nil
}

// FetchInProgressEpisodes returns all episodes the authenticated user currently
// has in progress (partially listened to). This does not include episodes that
// have been played to completion — use FetchPlayedEpisodes for those.
func FetchInProgressEpisodes(ctx context.Context, client *http.Client) ([]APIEpisode, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/user/in_progress", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: build in-progress request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: POST /user/in_progress: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, &TransientError{cause: fmt.Errorf("pocketcasts/web: /user/in_progress returned HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pocketcasts/web: /user/in_progress returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	var payload struct {
		Episodes []APIEpisode `json:"episodes"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("pocketcasts/web: parse in-progress response: %w", err)
	}
	return payload.Episodes, nil
}

// FetchPlayedEpisodes returns the episodes from the authenticated user's
// listening history that have been played to completion. The endpoint returns
// the most-recently-played episodes; older entries may not be included if the
// history exceeds the server's maximum page size.
//
// The returned episodes have PlayingStatus = PlayingPlayed and PlayedUpTo set
// to the full episode duration. This is used to supplement the Phase A
// in-progress index so that episodes previously migrated and fully played in
// Pocket Casts are not needlessly re-written.
func FetchPlayedEpisodes(ctx context.Context, client *http.Client) ([]APIEpisode, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/user/history", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: build history request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: POST /user/history: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, &TransientError{cause: fmt.Errorf("pocketcasts/web: /user/history returned HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pocketcasts/web: /user/history returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	var payload struct {
		Episodes []APIEpisode `json:"episodes"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("pocketcasts/web: parse history response: %w", err)
	}
	return payload.Episodes, nil
}

// cacheEpisode is the episode structure returned by the Pocket Casts cache CDN.
// It carries metadata only; play state is not available from this endpoint.
type cacheEpisode struct {
	UUID      string `json:"uuid"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Duration  int    `json:"duration"`
	Published string `json:"published"` // ISO 8601, e.g. "2024-01-15T10:00:00Z"
}

// FetchPodcastEpisodes fetches one page of episode metadata for a podcast from
// the Pocket Casts cache CDN. page is 0-indexed; episodes are sorted newest
// first. Returns the episode list, hasMore (true if more pages exist), and any
// error. Returned episodes have PlayingStatus = PlayingUnplayed since the cache
// CDN does not carry play state.
func FetchPodcastEpisodes(ctx context.Context, client *http.Client, podcastUUID string, page int) ([]APIEpisode, bool, error) {
	url := fmt.Sprintf("%s/podcast/full/%s/%d/3/1000", pcCacheURL, podcastUUID, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("pocketcasts/web: build episode-list request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("pocketcasts/web: GET podcast/full: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, false, &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, false, &TransientError{cause: fmt.Errorf("pocketcasts/web: podcast/full returned HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("pocketcasts/web: podcast/full returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	var payload struct {
		Podcast struct {
			Episodes []cacheEpisode `json:"episodes"`
		} `json:"podcast"`
		HasMoreEpisodes bool `json:"has_more_episodes"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, false, fmt.Errorf("pocketcasts/web: parse episode-list response: %w", err)
	}

	episodes := make([]APIEpisode, 0, len(payload.Podcast.Episodes))
	for _, ce := range payload.Podcast.Episodes {
		episodes = append(episodes, APIEpisode{
			UUID:          ce.UUID,
			PodcastUUID:   podcastUUID,
			Title:         ce.Title,
			URL:           ce.URL,
			PublishedAt:   ce.Published, // same ISO 8601 format as APIEpisode.PublishedAt
			Duration:      ce.Duration,
			PlayingStatus: PlayingUnplayed, // cache CDN does not include play state
		})
	}
	return episodes, payload.HasMoreEpisodes, nil
}

// UpdateEpisodeProgress sets the playback position and status for an episode.
//
//   - episodeUUID: Pocket Casts internal episode UUID
//   - podcastUUID: Pocket Casts internal podcast UUID
//   - status: PlayingUnplayed, PlayingInProgress, or PlayingPlayed
//   - positionSec: playback position in seconds (0 for played/unplayed)
//   - durationSec: episode duration in seconds; included in the request when
//     non-zero so that Pocket Casts can correctly record progress percentage
//     for in-progress episodes
func UpdateEpisodeProgress(ctx context.Context, client *http.Client,
	episodeUUID, podcastUUID string, status, positionSec, durationSec int) error {

	type updateBody struct {
		UUID     string `json:"uuid"`
		Podcast  string `json:"podcast"`
		Status   int    `json:"status"`
		Position int    `json:"position"`
		Duration int    `json:"duration,omitempty"`
	}
	payload := updateBody{
		UUID:     episodeUUID,
		Podcast:  podcastUUID,
		Status:   status,
		Position: positionSec,
		Duration: durationSec,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("pocketcasts/web: marshal update request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/sync/update_episode", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("pocketcasts/web: build update request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// Network-level failure — transient, caller may retry.
		return &TransientError{cause: fmt.Errorf("pocketcasts/web: /sync/update_episode: %w", err)}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return &TransientError{cause: fmt.Errorf("pocketcasts/web: /sync/update_episode returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pocketcasts/web: /sync/update_episode returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))
	}
	return nil
}

// ResolveFeedToPodcastUUID resolves a podcast RSS feed URL to the Pocket Casts
// internal podcast UUID using the feed submission service at
// refresh.pocketcasts.com. This endpoint is public and does not require
// authentication.
//
// The service may respond with status "poll" and a poll_uuid on the first
// request, requiring the caller to retry with that token until status "ok"
// is returned. Up to maxResolvePollAttempts attempts are made with a 2s pause.
func ResolveFeedToPodcastUUID(ctx context.Context, feedURL string) (string, error) {
	const maxAttempts = 10

	type resolveResponse struct {
		Status   string `json:"status"`
		PollUUID string `json:"poll_uuid"`
		Result   struct {
			Podcast struct {
				UUID string `json:"uuid"`
			} `json:"podcast"`
		} `json:"result"`
	}

	var pollUUID string
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(pcPollInterval):
			}
		}

		reqMap := map[string]any{
			"url":           feedURL,
			"public_option": "no",
		}
		if pollUUID != "" {
			reqMap["poll_uuid"] = pollUUID
		}
		data, err := json.Marshal(reqMap)
		if err != nil {
			return "", fmt.Errorf("pocketcasts/web: marshal feed-resolve request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			pcRefreshURL+"/author/add_feed_url", bytes.NewReader(data))
		if err != nil {
			return "", fmt.Errorf("pocketcasts/web: build feed-resolve request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", pcUA)
		req.Header.Set("Origin", "https://pocketcasts.com")
		req.Header.Set("Referer", "https://pocketcasts.com/")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("pocketcasts/web: feed-resolve POST: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("pocketcasts/web: feed-resolve returned HTTP %d: %s",
				resp.StatusCode, bodyExcerpt(body))
		}

		var result resolveResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return "", fmt.Errorf("pocketcasts/web: parse feed-resolve response: %w", err)
		}

		switch result.Status {
		case "ok":
			if result.Result.Podcast.UUID == "" {
				return "", fmt.Errorf("pocketcasts/web: feed-resolve succeeded but returned no UUID for %s", feedURL)
			}
			return result.Result.Podcast.UUID, nil
		case "poll":
			pollUUID = result.PollUUID
		default:
			return "", fmt.Errorf("pocketcasts/web: feed-resolve returned status %q for %s", result.Status, feedURL)
		}
	}
	return "", fmt.Errorf("pocketcasts/web: feed-resolve timed out after %d attempts for %s", maxAttempts, feedURL)
}

// SubscribePodcast subscribes the authenticated user to a podcast by its
// Pocket Casts internal UUID. Use ResolveFeedToPodcastUUID first to obtain
// the UUID from an RSS feed URL.
func SubscribePodcast(ctx context.Context, client *http.Client, podcastUUID string) error {
	data, err := json.Marshal(map[string]string{"uuid": podcastUUID})
	if err != nil {
		return fmt.Errorf("pocketcasts/web: marshal subscribe request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/user/podcast/subscribe", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("pocketcasts/web: build subscribe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return &TransientError{cause: fmt.Errorf("pocketcasts/web: /user/podcast/subscribe: %w", err)}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return &TransientError{cause: fmt.Errorf("pocketcasts/web: /user/podcast/subscribe returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pocketcasts/web: /user/podcast/subscribe returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))
	}
	return nil
}

// bodyExcerpt returns a short printable excerpt of a response body for error messages.
func bodyExcerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) == 0 {
		return "(empty body)"
	}
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
