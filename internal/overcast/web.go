package overcast

// web.go implements the unofficial Overcast web API used for writing episode
// play state. Overcast has no documented public API for this; the implementation
// mirrors what the Overcast web player does internally and may break without
// notice if Overcast changes its backend.
//
// Endpoints used:
//   GET  https://overcast.fm/podcasts              — list subscribed podcasts (page paths)
//   GET  https://overcast.fm/itunes{id}/{slug}     — podcast episode listing
//   POST https://overcast.fm/login                 — authenticate, get session cookie
//   GET  https://overcast.fm/+{hash}               — episode player page (contains data-item-id)
//   POST https://overcast.fm/podcasts/set_progress/{numericId} — update position

import (
	"context"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	overcastBaseURL = "https://overcast.fm"

	// PlayedSentinel is sent as the p parameter to mark an episode as fully played.
	// Overcast treats any position ≥ episode duration as played; INT32_MAX is the
	// conventional sentinel used by the Overcast web player.
	PlayedSentinel = 2147483647

	overcastUA = "podcast-migrate/1.0 (github.com/tyler/podcast-migrate)"
)

// numericIDRe extracts the internal numeric episode ID from an Overcast episode page.
var numericIDRe = regexp.MustCompile(`data-item-id="(\d+)"`)

// Login authenticates with Overcast and returns an *http.Client whose cookie jar
// holds a valid session. The client must be reused for all subsequent API calls
// within the same migration run — creating a new client would lose the session.
//
// Returns an error if the credentials are rejected or the network request fails.
func Login(ctx context.Context, email, password string) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: create cookie jar: %w", err)
	}
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("overcast/web: too many redirects during login")
			}
			return nil
		},
	}

	form := url.Values{
		"email":    {email},
		"password": {password},
		"then":     {"account"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		overcastBaseURL+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("overcast/web: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", overcastUA)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: login POST: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	// A successful login follows a redirect to /account (HTTP 200 after redirect).
	// Wrong credentials typically redirect back to /login.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("overcast/web: login returned HTTP %d — check credentials", resp.StatusCode)
	}
	if resp.Request != nil && strings.HasPrefix(resp.Request.URL.Path, "/login") {
		return nil, fmt.Errorf("overcast/web: login was redirected back to /login — check credentials")
	}

	// Confirm the cookie jar received a session cookie.
	u, _ := url.Parse(overcastBaseURL)
	if len(client.Jar.Cookies(u)) == 0 {
		return nil, fmt.Errorf("overcast/web: login succeeded (HTTP %d) but no session cookie was set — check credentials", resp.StatusCode)
	}

	return client, nil
}

// RateLimitError is returned when Overcast responds with HTTP 429.
// The Wait field holds how long to pause before the next attempt.
type RateLimitError struct {
	Wait time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("overcast/web: rate limited (HTTP 429) — retry after %v", e.Wait)
}

// rateLimitWait extracts the Retry-After delay from a 429 response, falling back
// to defaultWait if the header is absent or unparseable.
func rateLimitWait(resp *http.Response, defaultWait time.Duration) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultWait
}

// FetchEpisodeNumericID loads an Overcast episode page and extracts the internal
// numeric ID from the data-item-id HTML attribute. The numeric ID is required by
// the set_progress endpoint.
//
// episodeURL must be of the form "https://overcast.fm/+XXXXXXXX". The client must
// be authenticated (obtained from Login) so Overcast serves the player page.
// Returns *RateLimitError if the server responds with HTTP 429.
func FetchEpisodeNumericID(ctx context.Context, client *http.Client, episodeURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, episodeURL, nil)
	if err != nil {
		return "", fmt.Errorf("overcast/web: build GET request for %s: %w", episodeURL, err)
	}
	req.Header.Set("User-Agent", overcastUA)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("overcast/web: GET %s: %w", episodeURL, err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("overcast/web: GET %s returned HTTP %d", episodeURL, resp.StatusCode)
	}

	m := numericIDRe.FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("overcast/web: data-item-id attribute not found on %s", episodeURL)
	}
	return string(m[1]), nil
}

// SetProgress updates the playback position for an episode on Overcast.
//
//   - numericID: the data-item-id value / overcastId from the OPML.
//   - positionSeconds: playhead in whole seconds. Use PlayedSentinel to mark as played,
//     or 0 to mark as unplayed.
//
// This is an unofficial endpoint — it mirrors what the Overcast web player sends
// internally. The p, speed, and v parameters are URL-encoded form values.
func SetProgress(ctx context.Context, client *http.Client, numericID string, positionSeconds int) error {
	endpoint := overcastBaseURL + "/podcasts/set_progress/" + numericID
	form := url.Values{
		"p":     {strconv.Itoa(positionSeconds)},
		"speed": {"0"},
		"v":     {"0"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("overcast/web: build set_progress request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", overcastUA)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("overcast/web: set_progress %s: %w", numericID, err)
	}

	// Read up to 4 KB of the body for error diagnostics.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode != http.StatusOK {
		excerpt := bodyExcerpt(body)
		return fmt.Errorf("overcast/web: set_progress %s returned HTTP %d: %s", numericID, resp.StatusCode, excerpt)
	}

	// Detect a silent redirect to the login page (session expired or wrong credentials).
	if resp.Request != nil && strings.Contains(resp.Request.URL.Path, "/login") {
		return fmt.Errorf("overcast/web: set_progress %s was redirected to login — session may have expired; check credentials", numericID)
	}

	// Detect explicit error payloads in the response body.
	if lower := strings.ToLower(string(body)); strings.Contains(lower, `"error"`) || strings.Contains(lower, `"status":"error"`) {
		return fmt.Errorf("overcast/web: set_progress %s: server returned error: %s", numericID, bodyExcerpt(body))
	}

	return nil
}

// bodyExcerpt returns a short printable excerpt of a response body for use in error messages.
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

// ---- Extended matching: scrape podcast/episode pages for numeric IDs ----

// SearchPodcastITunesID calls the Overcast search autocomplete JSON endpoint and
// returns the iTunes ID for the podcast identified by title and/or overcastID.
//
// Matching priority:
//  1. Overcast podcast ID exact match (overcastID field from the OPML)
//  2. Case-insensitive exact title match
//
// Returns "" (no error) when the podcast is not found in the search results.
// Returns *RateLimitError when the server responds with HTTP 429.
// The client must be authenticated (obtained from Login).
func SearchPodcastITunesID(ctx context.Context, client *http.Client, title, overcastID string) (string, error) {
	endpoint := overcastBaseURL + "/podcasts/search_autocomplete?q=" + url.QueryEscape(title)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("overcast/web: build search request: %w", err)
	}
	req.Header.Set("User-Agent", overcastUA)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("overcast/web: GET search: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", &RateLimitError{Wait: rateLimitWait(resp, 30*time.Second)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("overcast/web: search returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Results []struct {
			ID       string `json:"id"`
			ITunesID string `json:"iTunesID"`
			Title    string `json:"title"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("overcast/web: parse search response: %w", err)
	}

	// Prefer matching by Overcast podcast ID (unambiguous).
	if overcastID != "" {
		for _, r := range payload.Results {
			if r.ID == overcastID {
				return r.ITunesID, nil
			}
		}
	}

	// Fallback: case-insensitive exact title match.
	titleNorm := strings.ToLower(strings.TrimSpace(title))
	for _, r := range payload.Results {
		if strings.ToLower(strings.TrimSpace(r.Title)) == titleNorm {
			return r.ITunesID, nil
		}
	}

	return "", nil // not found
}

// PodcastEpisodeListing holds the minimal data for one episode extracted from a podcast page.
type PodcastEpisodeListing struct {
	OvercastURL string // "https://overcast.fm/+HASH"
	DateStr     string // "YYYY-MM-DD" — day-level precision
	Title       string // episode title extracted from cell HTML (may be empty)
}

// episodeCellRe matches an episode anchor and captures five groups:
//
//   1. attributes before href (may contain class="extendedepisodecell...")
//   2. episode URL path "/+HASH"
//   3. attributes after href  (may contain class="extendedepisodecell...")
//   4. cell body HTML before the caption2 element (used for title extraction)
//   5. caption2 date/duration text
//
// Attribute order in the <a> tag is intentionally unconstrained — Overcast
// serves href before class on some podcast pages and class before href on
// others. Rather than requiring a fixed order, FetchPodcastEpisodes checks
// groups 1 and 3 in code to confirm the "extendedepisodecell" class is present
// (Go's RE2 engine does not support lookaheads, so we split the check).
//
// Using a single combined match (rather than two parallel global arrays) avoids
// an off-by-one bug: some podcast pages include caption2 elements outside of
// episode cells (e.g. the podcast website URL in the header). A global caption2
// scan would count those extra elements, shifting every date index by one and
// causing the last episode's date to fall off the end entirely.
//
// The (?s) flag makes . match newlines so the lazy .*? can cross line boundaries
// within a cell. Each cell contains exactly one caption2 element, so the lazy
// match always stops at the correct one.
var episodeCellRe = regexp.MustCompile(
	`(?s)<a\b([^>]*)\bhref="(/\+[^"]+)"([^>]*)>(.*?)<[^>]*\bclass="[^"]*caption2[^"]*"[^>]*>([^<]+)<`)

// htmlTagRe strips HTML tags for plain-text extraction.
var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

// extractTextFromHTML strips HTML tags, unescapes HTML entities, and normalises
// whitespace to produce a plain-text string. Used to extract episode titles from
// the HTML content of an episode cell body (the content before the caption2 date element).
func extractTextFromHTML(html string) string {
	s := htmlTagRe.ReplaceAllString(html, " ")
	s = htmlpkg.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// FetchPodcastEpisodes returns all episode listings from an Overcast podcast page.
// Episodes are returned in the order they appear on the page (most recent first).
// The client must be authenticated.
func FetchPodcastEpisodes(ctx context.Context, client *http.Client, podcastPageURL string) ([]PodcastEpisodeListing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, podcastPageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: build podcast page request: %w", err)
	}
	req.Header.Set("User-Agent", overcastUA)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: GET %s: %w", podcastPageURL, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("overcast/web: GET %s returned HTTP %d", podcastPageURL, resp.StatusCode)
	}

	var listings []PodcastEpisodeListing
	for _, m := range episodeCellRe.FindAllSubmatch(body, -1) {
		// m[1]: attributes before href; m[3]: attributes after href.
		// The extendedepisodecell class may appear in either, depending on attribute order.
		attrsBefore, attrsAfter := string(m[1]), string(m[3])
		if !strings.Contains(attrsBefore, "extendedepisodecell") &&
			!strings.Contains(attrsAfter, "extendedepisodecell") {
			continue // not an episode cell — skip other /+HASH anchors on the page
		}
		hash := string(m[2])
		// m[4] is the cell body HTML before the caption2 element — strip tags to get title.
		title := extractTextFromHTML(string(m[4]))
		// m[5] is the date/duration text inside the caption2 element.
		dateText := strings.TrimSpace(htmlpkg.UnescapeString(string(m[5])))
		dateStr, ok := parseOvercastPageDate(dateText)
		if !ok {
			continue
		}
		listings = append(listings, PodcastEpisodeListing{
			OvercastURL: overcastBaseURL + hash,
			DateStr:     dateStr,
			Title:       title,
		})
	}
	// When no episode cells were found the page is almost certainly JavaScript-rendered.
	// Save the raw body to a temp file so it can be inspected for API endpoint hints
	// or embedded JSON. The file is overwritten on every call, so it always contains
	// the most recent zero-listing page.
	if len(listings) == 0 && len(body) > 0 {
		debugPath := filepath.Join(os.TempDir(), "overcast-podcast-page-debug.html")
		_ = os.WriteFile(debugPath, body, 0644)
	}

	return listings, nil
}

// parseOvercastPageDate parses the date text from Overcast's podcast episode cells.
// Handles "Mar 26, 2021 • 76 min" (past year) and "May 27 • 12 min" (current year).
// Returns a "YYYY-MM-DD" string and whether parsing succeeded.
func parseOvercastPageDate(s string) (string, bool) {
	// Strip " • duration/progress" suffix
	if i := strings.Index(s, "•"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	// "Jan 2, 2006" — explicit year
	if t, err := time.Parse("Jan 2, 2006", s); err == nil {
		return t.Format("2006-01-02"), true
	}
	// "Jan 2" — assume current year; roll back one year if the result is > 14 days in the future
	if t, err := time.Parse("Jan 2", s); err == nil {
		now := time.Now().UTC()
		year := now.Year()
		candidate := time.Date(year, t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		if candidate.After(now.AddDate(0, 0, 14)) {
			candidate = time.Date(year-1, t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		}
		return candidate.Format("2006-01-02"), true
	}
	return "", false
}
