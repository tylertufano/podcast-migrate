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
	"fmt"
	htmlpkg "html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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

// FetchEpisodeNumericID loads an Overcast episode page and extracts the internal
// numeric ID from the data-item-id HTML attribute. The numeric ID is required by
// the set_progress endpoint.
//
// episodeURL must be of the form "https://overcast.fm/+XXXXXXXX". The client must
// be authenticated (obtained from Login) so Overcast serves the player page.
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
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("overcast/web: GET %s returned HTTP %d", episodeURL, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("overcast/web: read body for %s: %w", episodeURL, err)
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

// PodcastPageEntry holds the podcast page path extracted from the /podcasts listing.
// Path is relative, e.g. "/itunes1551206847/sistersinlaw".
type PodcastPageEntry struct {
	Path  string
	Title string // normalised (lowercase, trimmed)
}

// feedcellRe matches a subscribed podcast link on the /podcasts page.
// Captures the path (/itunes…) and the title text inside <div class="title">.
var feedcellRe = regexp.MustCompile(`(?s)class="feedcell"[^>]*href="(/itunes[^"]+)"[^>]*>.*?class="title"[^>]*>([^<]+)</div>`)

// FetchSubscribedPodcasts returns the Overcast podcast page paths for all subscribed
// podcasts, keyed by normalised (lowercase, trimmed) title.
// The client must be authenticated (from Login).
func FetchSubscribedPodcasts(ctx context.Context, client *http.Client) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, overcastBaseURL+"/podcasts", nil)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: build /podcasts request: %w", err)
	}
	req.Header.Set("User-Agent", overcastUA)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: GET /podcasts: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("overcast/web: GET /podcasts returned HTTP %d", resp.StatusCode)
	}

	result := make(map[string]string)
	for _, m := range feedcellRe.FindAllSubmatch(body, -1) {
		path := string(m[1])
		title := strings.ToLower(strings.TrimSpace(htmlpkg.UnescapeString(string(m[2]))))
		if title != "" && path != "" {
			result[title] = path
		}
	}
	return result, nil
}

// PodcastEpisodeListing holds the minimal data for one episode extracted from a podcast page.
type PodcastEpisodeListing struct {
	OvercastURL string // "https://overcast.fm/+HASH"
	DateStr     string // "YYYY-MM-DD" — day-level precision
}

// episodeHrefRe matches <a class="extendedepisodecell…" href="/+HASH"> links.
var episodeHrefRe = regexp.MustCompile(`class="extendedepisodecell[^"]*"[^>]*href="(/\+[^"]+)"`)

// caption2Re matches the date text inside <span class="caption2">…</span>.
var caption2Re = regexp.MustCompile(`class="caption2"[^>]*>([^<]+)`)

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

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("overcast/web: GET %s returned HTTP %d", podcastPageURL, resp.StatusCode)
	}

	hrefs := episodeHrefRe.FindAllSubmatch(body, -1)
	dates := caption2Re.FindAllSubmatch(body, -1)

	n := len(hrefs)
	if len(dates) < n {
		n = len(dates)
	}

	var listings []PodcastEpisodeListing
	for i := 0; i < n; i++ {
		hash := string(hrefs[i][1])
		dateText := strings.TrimSpace(htmlpkg.UnescapeString(string(dates[i][1])))
		dateStr, ok := parseOvercastPageDate(dateText)
		if !ok {
			continue
		}
		listings = append(listings, PodcastEpisodeListing{
			OvercastURL: overcastBaseURL + hash,
			DateStr:     dateStr,
		})
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
