package pocketcasts

// web.go implements the unofficial Pocket Casts web API used for reading and
// writing episode play state and podcast subscriptions.
//
// Pocket Casts has no official public API. This implementation targets the
// same JSON/form endpoints used by the Pocket Casts web player at
// play.pocketcasts.com and may break without notice if Pocket Casts changes
// its backend.
//
// Endpoints used:
//   POST https://play.pocketcasts.com/users/sign_in                         — authenticate, get session cookie
//   POST https://play.pocketcasts.com/web/podcasts/all.json                  — subscribed podcast list
//   POST https://play.pocketcasts.com/web/episodes/in_progress_episodes.json — in-progress episodes
//   POST https://play.pocketcasts.com/web/episodes/find_by_podcast.json      — paginated episode list per podcast
//   POST https://play.pocketcasts.com/web/episodes/update_episode_position.json — set play position/status

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var pcBaseURL = "https://play.pocketcasts.com"

// SetBaseURLForTest overrides the Pocket Casts base URL for unit tests that use
// an httptest.Server. Must be reset to the production URL after each test.
func SetBaseURLForTest(u string) { pcBaseURL = u }

const (
	pcUA = "podcast-migrate/1.0 (github.com/tyler/podcast-migrate)"

	// pcTimeLayout is the date-time format returned by the Pocket Casts API.
	// Times are stored and returned in UTC without an explicit timezone marker.
	pcTimeLayout = "2006-01-02 15:04:05"

	// Playing status constants mirror the values used by the Pocket Casts API.
	PlayingUnplayed   = 0 // episode not started
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

// APIPodcast is a podcast as returned by /web/podcasts/all.json.
type APIPodcast struct {
	UUID         string `json:"uuid"`
	Title        string `json:"title"`
	Author       string `json:"author"`
	URL          string `json:"url"`          // RSS feed URL
	ThumbnailURL string `json:"thumbnail_url"`
}

// APIEpisode is an episode as returned by episode list endpoints.
// The URL field is the audio enclosure URL — not the RSS <guid> value.
// Episode matching against other providers must rely on PublishedAt + podcast
// feed URL (or Title as a fallback) rather than the enclosure URL.
type APIEpisode struct {
	UUID          string `json:"uuid"`
	PodcastUUID   string `json:"podcast_uuid"`
	Title         string `json:"title"`
	URL           string `json:"url"`            // enclosure/audio URL (not RSS GUID)
	PublishedAt   string `json:"published_at"`   // "YYYY-MM-DD HH:MM:SS" UTC
	Duration      int    `json:"duration"`       // seconds
	PlayingStatus int    `json:"playing_status"` // 0=unplayed, 2=in-progress, 3=played
	PlayedUpTo    int    `json:"played_up_to"`   // seconds
	Starred       bool   `json:"starred"`
	IsDeleted     bool   `json:"is_deleted"`
}

// ParsePublishedAt parses the episode's PublishedAt field as a UTC time.
// Returns the zero time if the field is empty or cannot be parsed.
func (e *APIEpisode) ParsePublishedAt() time.Time {
	if e.PublishedAt == "" {
		return time.Time{}
	}
	t, err := time.ParseInLocation(pcTimeLayout, e.PublishedAt, time.UTC)
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

// Login authenticates with Pocket Casts and returns an *http.Client whose cookie
// jar holds a valid session. The client must be reused for all subsequent API
// calls within the same migration run — creating a new client loses the session.
func Login(ctx context.Context, email, password string) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: create cookie jar: %w", err)
	}
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		// Follow redirects but detect landing on an error page.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("pocketcasts/web: too many redirects during login")
			}
			return nil
		},
	}

	form := url.Values{
		"[user]email":    {email},
		"[user]password": {password},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/users/sign_in", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", pcUA)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: login POST: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
		return nil, fmt.Errorf("pocketcasts/web: login returned HTTP %d — check credentials", resp.StatusCode)
	}
	// The login page embeds an error message in the response body on failure.
	if strings.Contains(strings.ToLower(string(body)), "invalid email or password") {
		return nil, fmt.Errorf("pocketcasts/web: login failed — invalid email or password")
	}

	// Confirm the cookie jar received a session cookie.
	// Check against the final request URL (after any redirects) so the path matches
	// where the cookie was actually set. Fall back to the base URL if unavailable.
	checkURL, _ := url.Parse(pcBaseURL)
	if resp.Request != nil && resp.Request.URL != nil {
		checkURL = resp.Request.URL
	}
	if len(client.Jar.Cookies(checkURL)) == 0 {
		return nil, fmt.Errorf("pocketcasts/web: login succeeded (HTTP %d) but no session cookie was set — check credentials", resp.StatusCode)
	}

	return client, nil
}

// FetchSubscribedPodcasts returns all podcasts the authenticated user is
// currently subscribed to, including their RSS feed URLs.
func FetchSubscribedPodcasts(ctx context.Context, client *http.Client) ([]APIPodcast, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/web/podcasts/all.json", nil)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: build subscribed-podcasts request: %w", err)
	}
	req.Header.Set("User-Agent", pcUA)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: POST /web/podcasts/all.json: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, &TransientError{cause: fmt.Errorf("pocketcasts/web: /web/podcasts/all.json returned HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pocketcasts/web: /web/podcasts/all.json returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))
	}

	var payload struct {
		Podcasts []APIPodcast `json:"podcasts"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("pocketcasts/web: parse subscribed-podcasts response: %w", err)
	}
	return payload.Podcasts, nil
}

// FetchInProgressEpisodes returns all episodes the authenticated user currently
// has in progress (partially listened to). This does not include episodes that
// have been played to completion — use per-podcast episode listing for a full
// picture of play state.
func FetchInProgressEpisodes(ctx context.Context, client *http.Client) ([]APIEpisode, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/web/episodes/in_progress_episodes.json", nil)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: build in-progress request: %w", err)
	}
	req.Header.Set("User-Agent", pcUA)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: POST in_progress_episodes.json: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, &TransientError{cause: fmt.Errorf("pocketcasts/web: in_progress_episodes.json returned HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pocketcasts/web: in_progress_episodes.json returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))
	}

	var payload struct {
		Episodes []APIEpisode `json:"episodes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("pocketcasts/web: parse in-progress response: %w", err)
	}
	return payload.Episodes, nil
}

// FetchPodcastEpisodes fetches one page of episodes for a podcast.
// page is 1-indexed; episodes are sorted newest-first.
// Returns the episode list, total episode count across all pages, and any error.
//
// The total count allows callers to determine whether more pages exist:
// keep requesting pages until the cumulative fetched count equals total.
func FetchPodcastEpisodes(ctx context.Context, client *http.Client, podcastUUID string, page int) ([]APIEpisode, int, error) {
	form := url.Values{
		"uuid": {podcastUUID},
		"page": {strconv.Itoa(page)},
		"sort": {"3"}, // 3 = newest to oldest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/web/episodes/find_by_podcast.json",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, fmt.Errorf("pocketcasts/web: build episode-list request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", pcUA)

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("pocketcasts/web: POST find_by_podcast.json: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, 0, &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, 0, &TransientError{cause: fmt.Errorf("pocketcasts/web: find_by_podcast.json returned HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("pocketcasts/web: find_by_podcast.json returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))
	}

	var payload struct {
		Result struct {
			Episodes []APIEpisode `json:"episodes"`
			Total    int          `json:"total"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, 0, fmt.Errorf("pocketcasts/web: parse episode-list response: %w", err)
	}
	return payload.Result.Episodes, payload.Result.Total, nil
}

// UpdateEpisodeProgress sets the playback position and status for an episode.
//
//   - episodeUUID: Pocket Casts internal episode UUID
//   - podcastUUID: Pocket Casts internal podcast UUID
//   - status: PlayingUnplayed, PlayingInProgress, or PlayingPlayed
//   - positionSec: playback position in seconds (0 for played/unplayed)
//   - durationSec: episode duration in seconds (0 when unknown)
func UpdateEpisodeProgress(ctx context.Context, client *http.Client,
	episodeUUID, podcastUUID string, status, positionSec, durationSec int) error {

	type updateBody struct {
		UUID          string `json:"uuid"`
		PodcastUUID   string `json:"podcast_uuid"`
		PlayingStatus int    `json:"playing_status"`
		PlayedUpTo    int    `json:"played_up_to"`
		Duration      int    `json:"duration"`
	}
	payload := updateBody{
		UUID:          episodeUUID,
		PodcastUUID:   podcastUUID,
		PlayingStatus: status,
		PlayedUpTo:    positionSec,
		Duration:      durationSec,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("pocketcasts/web: marshal update request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/web/episodes/update_episode_position.json",
		bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("pocketcasts/web: build update request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", pcUA)

	resp, err := client.Do(req)
	if err != nil {
		// Network-level failure — transient, caller may retry.
		return &TransientError{cause: fmt.Errorf("pocketcasts/web: update_episode_position.json: %w", err)}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return &TransientError{cause: fmt.Errorf("pocketcasts/web: update_episode_position.json returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pocketcasts/web: update_episode_position.json returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))
	}

	// The API returns {"status":"ok"} on success. Treat any non-"ok" status as an error.
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err == nil && result.Status != "" && result.Status != "ok" {
		return fmt.Errorf("pocketcasts/web: update_episode_position.json: server returned status %q: %s",
			result.Status, bodyExcerpt(body))
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
