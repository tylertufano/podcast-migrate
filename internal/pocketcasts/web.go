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
//   POST https://api.pocketcasts.com/user/login                              — authenticate, get Bearer token (scope: mobile)
//   POST https://api.pocketcasts.com/user/podcast/list                       — subscribed podcast list
//   POST https://refresh.pocketcasts.com/import/export_feed_urls             — batch UUID → feed URL (returns subscriber URLs; requires mobile scope)
//   POST https://api.pocketcasts.com/user/in_progress                        — in-progress episodes (with play state)
//   POST https://api.pocketcasts.com/user/history                            — recently-played episodes (with play state)
//   POST https://api.pocketcasts.com/user/podcast/episodes                   — per-podcast episode list WITH user play state
//   GET  https://cache.pocketcasts.com/podcast/full/{uuid}/{page}/3/1000     — paginated episode metadata (no play state, fallback only)
//   POST https://api.pocketcasts.com/sync/update_episode                     — set play position/status
//   POST https://api.pocketcasts.com/user/sync/update                        — full/delta play-state sync (protobuf; used by mobile apps)
//   POST https://api.pocketcasts.com/user/podcast/subscribe                  — subscribe to a podcast by UUID
//   POST https://refresh.pocketcasts.com/author/add_feed_url                 — resolve RSS feed URL → podcast UUID (public, no auth)
//   GET  https://itunes.apple.com/search?term=…&media=podcast                — recover RSS feed URL for unsubscribed podcasts (--pc-include-unsubscribed only)

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/tyler/podcast-migrate/internal/httputil"
)

var pcBaseURL      = "https://api.pocketcasts.com"
var pcCacheURL     = "https://cache.pocketcasts.com"
var pcRefreshURL   = "https://refresh.pocketcasts.com"
var pcPollInterval = 2 * time.Second

// pcDeviceID is a stable random identifier sent as the "device" parameter to
// unauthenticated Pocket Casts refresh endpoints (mirrors the uniqueAppId used
// by the iOS app). It is regenerated on each process start, which is fine —
// the server uses it only for analytics, not for session continuity.
var pcDeviceID = func() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "podcast-migrate-device"
	}
	return hex.EncodeToString(b)
}()

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
	UUID      string `json:"uuid"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	URL       string `json:"url"`       // RSS feed URL; empty for webFeed:false podcasts
	IsPrivate bool   `json:"isPrivate"` // subscriber-only feed with no public RSS equivalent
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

	reqBody, err := json.Marshal(loginReq{Email: email, Password: password, Scope: "mobile"})
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
		return nil, &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /user/podcast/list returned HTTP %d", resp.StatusCode))
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

// FetchExportFeedURLs resolves a batch of podcast UUIDs to their RSS feed URLs
// using the Pocket Casts export service at refresh.pocketcasts.com. This is
// the same endpoint the iOS app uses to build its OPML export, so it returns
// the actual URL the user subscribed with — including private/subscriber URLs
// that /user/podcast/list does not expose.
//
// Requires a client authenticated with the "mobile" scope (the default for Login).
// Returns a map of UUID → feed URL; UUIDs not known to the service are absent from
// the map. An empty UUID slice returns (nil, nil) without making a network call.
func FetchExportFeedURLs(ctx context.Context, client *http.Client, uuids []string) (map[string]string, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	type reqBody struct {
		UUIDs []string `json:"uuids"`
		V     string   `json:"v"`
		DT    string   `json:"dt"`
	}
	body, err := json.Marshal(reqBody{UUIDs: uuids, V: "1", DT: "1"})
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: marshal export-feed-urls request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcRefreshURL+"/import/export_feed_urls", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: build export-feed-urls request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pocketcasts/web: POST /import/export_feed_urls: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /import/export_feed_urls returned HTTP %d", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pocketcasts/web: /import/export_feed_urls returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	var payload struct {
		Status string            `json:"status"`
		Result map[string]string `json:"result"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("pocketcasts/web: parse export-feed-urls response: %w", err)
	}
	if payload.Status != "ok" {
		return nil, fmt.Errorf("pocketcasts/web: export-feed-urls returned status %q", payload.Status)
	}
	return payload.Result, nil
}

// LookupFeedURL searches the iTunes podcast directory for the public RSS feed
// URL of a podcast identified by title and author. Returns "" if no confident
// match is found or the API is unavailable. No authentication is required.
func LookupFeedURL(ctx context.Context, title, author string) (string, error) {
	q := url.QueryEscape(title + " " + author)
	apiURL := "https://itunes.apple.com/search?term=" + q + "&media=podcast&limit=5"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("iTunes search returned HTTP %d", resp.StatusCode)
	}

	var result struct {
		Results []struct {
			CollectionName string `json:"collectionName"`
			FeedURL        string `json:"feedUrl"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	titleLower := strings.ToLower(title)
	for _, r := range result.Results {
		if strings.ToLower(r.CollectionName) == titleLower && r.FeedURL != "" {
			return r.FeedURL, nil
		}
	}
	return "", nil
}

// FetchInProgressEpisodes returns all episodes the authenticated user currently
// has in progress (partially listened to). This does not include episodes that
// have been played to completion — use FetchPlayedEpisodes for those.
//
// The /user/in_progress endpoint uses the same camelCase JSON format as
// /user/history, so this function parses via historyEpisode and converts.
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
		return nil, &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /user/in_progress returned HTTP %d", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pocketcasts/web: /user/in_progress returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	var payload struct {
		Episodes []historyEpisode `json:"episodes"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("pocketcasts/web: parse in-progress response: %w", err)
	}

	out := make([]APIEpisode, 0, len(payload.Episodes))
	for _, h := range payload.Episodes {
		out = append(out, APIEpisode{
			UUID:          h.UUID,
			PodcastUUID:   h.PodcastUUID,
			Title:         h.Title,
			URL:           h.URL,
			PublishedAt:   h.Published,
			Duration:      h.Duration,
			PlayingStatus: h.PlayingStatus,
			PlayedUpTo:    h.PlayedUpTo,
			Starred:       h.Starred,
			IsDeleted:     h.IsDeleted,
		})
	}
	return out, nil
}

// historyEpisode is the episode structure returned by the /user/history
// endpoint.  Unlike the in-progress endpoint (which uses snake_case keys), the
// history endpoint uses camelCase JSON keys.  This struct captures the
// camelCase names; FetchPlayedEpisodes converts each entry to APIEpisode before
// returning so the rest of the codebase only deals with one episode type.
//
// Field name evidence: real response dump at
// https://github.com/AnandChowdhary/life-data/blob/master/podcast-history.yml
type historyEpisode struct {
	UUID          string `json:"uuid"`
	PodcastUUID   string `json:"podcastUuid"`   // camelCase — differs from in_progress
	Title         string `json:"title"`
	URL           string `json:"url"`
	Published     string `json:"published"`     // "published", not "published_at"
	Duration      int    `json:"duration"`
	PlayingStatus int    `json:"playingStatus"` // camelCase — differs from in_progress
	PlayedUpTo    int    `json:"playedUpTo"`    // camelCase — differs from in_progress
	Starred       bool   `json:"starred"`
	IsDeleted     bool   `json:"isDeleted"`     // camelCase — often true for history entries
}

// FetchPlayedEpisodes returns the episodes from the authenticated user's
// listening history that have been played to completion. The endpoint returns
// the most-recently-played episodes (up to ~100); older entries may not be
// included if the history exceeds the server's cap.
//
// Note: history entries routinely have isDeleted=true even for active
// subscriptions — this appears to mean "removed from the active queue" rather
// than "episode deleted". Callers should NOT skip isDeleted entries wholesale;
// filter on feedURL resolution instead.
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
		return nil, &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /user/history returned HTTP %d", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pocketcasts/web: /user/history returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	var payload struct {
		Episodes []historyEpisode `json:"episodes"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("pocketcasts/web: parse history response: %w", err)
	}

	// Convert camelCase history entries to the standard APIEpisode type.
	// The Published field maps to PublishedAt (same ISO 8601 format, different key name).
	out := make([]APIEpisode, 0, len(payload.Episodes))
	for _, h := range payload.Episodes {
		out = append(out, APIEpisode{
			UUID:          h.UUID,
			PodcastUUID:   h.PodcastUUID,
			Title:         h.Title,
			URL:           h.URL,
			PublishedAt:   h.Published,
			Duration:      h.Duration,
			PlayingStatus: h.PlayingStatus,
			PlayedUpTo:    h.PlayedUpTo,
			Starred:       h.Starred,
			IsDeleted:     h.IsDeleted,
		})
	}
	return out, nil
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

// FetchPodcastMeta returns the title and RSS feed URL for a podcast from the
// cache CDN. Used to recover metadata for podcasts not in the subscription list.
func FetchPodcastMeta(ctx context.Context, client *http.Client, podcastUUID string) (title, feedURL string, err error) {
	apiURL := fmt.Sprintf("%s/podcast/full/%s/0/3/1", pcCacheURL, podcastUUID) // page 0, 1 episode (we only need the podcast object)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("pocketcasts/web: podcast meta returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Podcast struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"podcast"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", err
	}
	return payload.Podcast.Title, payload.Podcast.URL, nil
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
		return nil, false, &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, false, httputil.NewTransientError(fmt.Errorf("pocketcasts/web: podcast/full returned HTTP %d", resp.StatusCode))
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

// FetchUserPodcastEpisodes fetches the complete episode list for a podcast from
// the authenticated Pocket Casts API. Unlike FetchPodcastEpisodes (public CDN),
// this endpoint returns the user's actual play state (PlayingStatus, PlayedUpTo)
// for each episode.
//
// The endpoint does not support the same page-based pagination as the CDN; it
// returns all episodes in a single response. Callers should treat hasMore=false
// as always true (one call per podcast, no page loop needed).
//
// The response uses camelCase JSON keys (same format as /user/history).
// The page parameter is accepted for API symmetry with FetchPodcastEpisodes but
// is NOT forwarded in the request body — the endpoint returns all episodes
// regardless and does not accept a page offset. Always pass page=0.
func FetchUserPodcastEpisodes(ctx context.Context, client *http.Client, podcastUUID string, _ int) ([]APIEpisode, bool, error) {
	type reqBody struct {
		UUID string `json:"uuid"`
	}
	b, _ := json.Marshal(reqBody{UUID: podcastUUID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/user/podcast/episodes", bytes.NewReader(b))
	if err != nil {
		return nil, false, fmt.Errorf("pocketcasts/web: build user-episode-list request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("pocketcasts/web: POST /user/podcast/episodes: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024)) // larger limit: all episodes at once
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, false, &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, false, httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /user/podcast/episodes returned HTTP %d", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("pocketcasts/web: /user/podcast/episodes returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	// The authenticated endpoint uses the same camelCase format as /user/history.
	var payload struct {
		Episodes []historyEpisode `json:"episodes"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, false, fmt.Errorf("pocketcasts/web: parse user-episode-list response: %w", err)
	}

	out := make([]APIEpisode, 0, len(payload.Episodes))
	for _, h := range payload.Episodes {
		ep := APIEpisode{
			UUID:          h.UUID,
			PodcastUUID:   podcastUUID, // passed-in UUID; podcastUuid may be absent in per-podcast responses
			Title:         h.Title,
			URL:           h.URL,
			PublishedAt:   h.Published,
			Duration:      h.Duration,
			PlayingStatus: h.PlayingStatus,
			PlayedUpTo:    h.PlayedUpTo,
			Starred:       h.Starred,
			IsDeleted:     h.IsDeleted,
		}
		// Use PodcastUUID from payload if present (useful for cross-check).
		if h.PodcastUUID != "" {
			ep.PodcastUUID = h.PodcastUUID
		}
		out = append(out, ep)
	}
	// This endpoint returns all episodes at once — no additional pages.
	return out, false, nil
}

// SyncEpisodeState is one episode record returned by /user/sync/update.
// Unlike most PC API responses it carries only play state — not episode
// metadata (title, published date). Callers join with per-podcast episode
// lists to get the full picture.
type SyncEpisodeState struct {
	EpisodeUUID   string
	PodcastUUID   string
	PlayingStatus int   // 1=unplayed, 2=in_progress, 3=played
	PlayedUpTo    int64 // seconds
}

// FetchSyncUpdate fetches the complete episode play-state index from the
// Pocket Casts sync service using the same protobuf endpoint the mobile apps
// use. lastModified should be 0 for a full sync, or the value returned by a
// prior call for an incremental (delta) sync.
//
// Wire protocol: POST /user/sync/update with Content-Type application/octet-stream.
// Proto schema: sync_api.proto in github.com/Automattic/pocket-casts-android.
func FetchSyncUpdate(ctx context.Context, client *http.Client, lastModified int64) ([]SyncEpisodeState, int64, error) {
	reqBody := encodeSyncUpdateRequest(lastModified)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcBaseURL+"/user/sync/update", bytes.NewReader(reqBody))
	if err != nil {
		return nil, 0, fmt.Errorf("pocketcasts/web: build sync-update request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, httputil.NewTransientError(fmt.Errorf("pocketcasts/web: POST /user/sync/update: %w", err))
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, 0, &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return nil, 0, httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /user/sync/update returned HTTP %d", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("pocketcasts/web: /user/sync/update returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	return decodeSyncUpdateResponse(respBody)
}

// encodeSyncUpdateRequest encodes a SyncUpdateRequest proto message.
//
// SyncUpdateRequest { int64 device_utc_time_ms=1; int64 last_modified=2; string device_id=4; repeated Record records=5; }
// We send the current time, the caller's lastModified, and an empty records
// list (read-only — no local changes to push).
func encodeSyncUpdateRequest(lastModified int64) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(time.Now().UnixMilli()))
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(lastModified))
	// field 5 (records) omitted — empty repeated field is valid proto3
	return b
}

// decodeSyncUpdateResponse parses a protobuf SyncUpdateResponse.
//
// SyncUpdateResponse { int64 last_modified=1; repeated Record records=2; }
// Record { oneof { SyncUserPodcast podcast=1; SyncUserEpisode episode=2; ... } }
// SyncUserEpisode {
//   string uuid=1; string podcast_uuid=2;
//   Int32Value playing_status=7; Int64Value played_up_to=9;
// }
func decodeSyncUpdateResponse(b []byte) ([]SyncEpisodeState, int64, error) {
	var lastModified int64
	var episodes []SyncEpisodeState

	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, 0, fmt.Errorf("pocketcasts/web: malformed sync-update response (tag)")
		}
		b = b[n:]

		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, 0, fmt.Errorf("pocketcasts/web: malformed sync-update last_modified")
			}
			lastModified = int64(v)
			b = b[n:]

		case num == 2 && typ == protowire.BytesType:
			rec, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, 0, fmt.Errorf("pocketcasts/web: malformed sync-update record")
			}
			b = b[n:]
			if ep, ok := decodeSyncRecord(rec); ok {
				episodes = append(episodes, ep)
			}

		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, 0, fmt.Errorf("pocketcasts/web: malformed sync-update response (field)")
			}
			b = b[n:]
		}
	}
	return episodes, lastModified, nil
}

// decodeSyncRecord extracts a SyncUserEpisode from a Record oneof message.
// Returns false if the record contains no episode field (podcast, playlist, etc.).
func decodeSyncRecord(b []byte) (SyncEpisodeState, bool) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return SyncEpisodeState{}, false
		}
		b = b[n:]
		if num == 2 && typ == protowire.BytesType {
			epBytes, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return SyncEpisodeState{}, false
			}
			return decodeSyncUserEpisode(epBytes), true
		}
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			return SyncEpisodeState{}, false
		}
		b = b[n:]
	}
	return SyncEpisodeState{}, false
}

// decodeSyncUserEpisode parses a SyncUserEpisode proto message.
func decodeSyncUserEpisode(b []byte) SyncEpisodeState {
	var ep SyncEpisodeState
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			s, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return ep
			}
			ep.EpisodeUUID = string(s)
			b = b[n:]
		case num == 2 && typ == protowire.BytesType:
			s, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return ep
			}
			ep.PodcastUUID = string(s)
			b = b[n:]
		case num == 7 && typ == protowire.BytesType: // Int32Value playing_status
			wv, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return ep
			}
			b = b[n:]
			ep.PlayingStatus = int(decodeVarintWrapper(wv))
		case num == 9 && typ == protowire.BytesType: // Int64Value played_up_to
			wv, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return ep
			}
			b = b[n:]
			ep.PlayedUpTo = int64(decodeVarintWrapper(wv))
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return ep
			}
			b = b[n:]
		}
	}
	return ep
}

// decodeVarintWrapper parses a google.protobuf.Int32Value or Int64Value —
// a message containing a single varint at field 1.
func decodeVarintWrapper(b []byte) uint64 {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0
		}
		b = b[n:]
		if num == 1 && typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return 0
			}
			return v
		}
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			return 0
		}
		b = b[n:]
	}
	return 0
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
		return httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /sync/update_episode: %w", err))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /sync/update_episode returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pocketcasts/web: /sync/update_episode returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))
	}
	return nil
}

// FindPodcastByITunesID resolves an iTunes Store podcast collection ID to the
// Pocket Casts internal podcast UUID using the podcasts/show endpoint on
// refresh.pocketcasts.com. No authentication is required.
//
// Returns ("", nil) when the podcast is not found in the Pocket Casts catalog
// (status "error"). Returns an error only on network or parse failures.
func FindPodcastByITunesID(ctx context.Context, itunesID int64) (string, error) {
	type reqBody struct {
		ID     int64  `json:"id"`
		DT     string `json:"dt"`
		Device string `json:"device"`
		V      string `json:"v"`
		M      string `json:"m"`
		AV     string `json:"av"`
		L      string `json:"l"`
		C      string `json:"c"`
	}
	body, err := json.Marshal(reqBody{
		ID:     itunesID,
		DT:     "1",
		Device: pcDeviceID,
		V:      "1.7",
		M:      "18.0",
		AV:     "7.54",
		L:      "en",
		C:      "US",
	})
	if err != nil {
		return "", fmt.Errorf("pocketcasts/web: marshal podcasts/show request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pcRefreshURL+"/podcasts/show", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("pocketcasts/web: build podcasts/show request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", pcUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("pocketcasts/web: POST /podcasts/show: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()

	if resp.StatusCode >= 500 {
		return "", httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /podcasts/show returned HTTP %d", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pocketcasts/web: /podcasts/show returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(respBody))
	}

	var payload struct {
		Status string `json:"status"`
		Result *struct {
			Podcast struct {
				UUID string `json:"uuid"`
			} `json:"podcast"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return "", fmt.Errorf("pocketcasts/web: parse podcasts/show response: %w", err)
	}
	if payload.Status != "ok" || payload.Result == nil {
		return "", nil // podcast not in PC catalog
	}
	return payload.Result.Podcast.UUID, nil
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
		return httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /user/podcast/subscribe: %w", err))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		return httputil.NewTransientError(fmt.Errorf("pocketcasts/web: /user/podcast/subscribe returned HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body)))
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
