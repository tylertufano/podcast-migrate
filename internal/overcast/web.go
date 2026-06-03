package overcast

// web.go implements the unofficial Overcast web API used for writing episode
// play state. Overcast has no documented public API for this; the implementation
// mirrors what the Overcast web player does internally and may break without
// notice if Overcast changes its backend.
//
// Endpoints used:
//   GET  https://overcast.fm/account/export_opml/extended — live library as extended OPML
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

	"github.com/tyler/podcast-migrate/internal/model"
)

var overcastBaseURL = "https://overcast.fm"

// SetBaseURLForTest overrides the Overcast base URL for unit tests that use
// an httptest.Server. Must be reset to "https://overcast.fm" after each test.
func SetBaseURLForTest(u string) { overcastBaseURL = u }

const (

	// PlayedSentinel is sent as the p parameter to mark an episode as fully played.
	// Overcast treats any position ≥ episode duration as played; INT32_MAX is the
	// conventional sentinel used by the Overcast web player.
	PlayedSentinel = 2147483647

	overcastUA = "podcast-migrate/1.0 (github.com/tyler/podcast-migrate)"
)

// numericIDRe extracts the internal numeric episode ID from an Overcast episode page.
// The original attribute was data-item-id; the fallback looks for the ID inside
// any set_progress URL on the page (form action, href, or JS string literal),
// which is more robust to page-structure changes.
var numericIDRe = regexp.MustCompile(`data-item-id="(\d+)"`)
var numericIDFromSetProgressRe = regexp.MustCompile(`set_progress/(\d+)`)

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

// TransientError is returned when SetProgress receives a 5xx response or a
// network-level failure — both of which may succeed on a subsequent attempt.
// Callers that implement retry logic should detect this type and back off before
// retrying. Permanent client errors (4xx other than 429) are not wrapped.
type TransientError struct {
	cause error
}

func (e *TransientError) Error() string { return e.cause.Error() }
func (e *TransientError) Unwrap() error { return e.cause }

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
	// Read the full body — the numeric ID may appear anywhere on the page.
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("overcast/web: GET %s returned HTTP %d", episodeURL, resp.StatusCode)
	}

	// Primary: data-item-id attribute (original location).
	if m := numericIDRe.FindSubmatch(body); m != nil {
		return string(m[1]), nil
	}
	// Fallback: set_progress URL anywhere on the page (form action, href, JS literal).
	// Overcast's web player must reference the set_progress endpoint, and that URL
	// contains the numeric ID directly — more robust to page-structure changes.
	if m := numericIDFromSetProgressRe.FindSubmatch(body); m != nil {
		return string(m[1]), nil
	}

	// Neither pattern found. Save the raw HTML so the page structure can be inspected.
	debugPath := filepath.Join(os.TempDir(), "overcast-episode-page-debug.html")
	_ = os.WriteFile(debugPath, body, 0644)
	return "", fmt.Errorf("overcast/web: numeric episode ID not found on %s (raw HTML saved to %s)",
		episodeURL, debugPath)
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
		// Network-level failure (DNS, TCP, timeout) — transient, caller may retry.
		return &TransientError{cause: fmt.Errorf("overcast/web: set_progress %s: %w", numericID, err)}
	}

	// Read up to 4 KB of the body for error diagnostics.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode >= 500 {
		// Server-side error — transient, caller may retry with backoff.
		excerpt := bodyExcerpt(body)
		return &TransientError{cause: fmt.Errorf("overcast/web: set_progress %s returned HTTP %d: %s", numericID, resp.StatusCode, excerpt)}
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

// FetchExtendedOPML fetches the authenticated user's current Overcast library as an
// extended OPML and returns it parsed as a *model.Library. The client must be
// authenticated (obtained from Login).
//
// This is equivalent to the user manually downloading
// overcast.fm/account/export_opml/extended and passing it via --overcast-match-opml,
// but happens automatically at write time so the destination index always reflects
// the account's current subscriptions and play state.
func FetchExtendedOPML(ctx context.Context, client *http.Client) (*model.Library, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		overcastBaseURL+"/account/export_opml/extended", nil)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: build OPML fetch request: %w", err)
	}
	req.Header.Set("User-Agent", overcastUA)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: fetch extended OPML: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{Wait: rateLimitWait(resp, 60*time.Second)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("overcast/web: fetch extended OPML: HTTP %d: %s",
			resp.StatusCode, bodyExcerpt(body))
	}

	lib, err := parseOPMLBytes(body)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: parse fetched OPML: %w", err)
	}
	return lib, nil
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

	// Fallback 1: case-insensitive exact title match.
	titleNorm := strings.ToLower(strings.TrimSpace(title))
	for _, r := range payload.Results {
		if strings.ToLower(strings.TrimSpace(r.Title)) == titleNorm {
			return r.ITunesID, nil
		}
	}

	// Fallback 2: Plus-normalised title match.
	// Handles the case where the Overcast OPML has "Fresh Air Plus" but the
	// Overcast search catalog knows it as "Fresh Air" (or vice-versa). Both
	// sides are normalised so that "fresh air plus" == "fresh air" after stripping.
	baseTitleNorm := model.NormalizePlusTitle(title)
	if baseTitleNorm != titleNorm {
		for _, r := range payload.Results {
			if model.NormalizePlusTitle(r.Title) == baseTitleNorm {
				return r.ITunesID, nil
			}
		}
	}

	return "", nil // not found
}

// SubscribedPodcast holds the page URL and title of one podcast from /podcasts.
type SubscribedPodcast struct {
	PageURL string // absolute URL, e.g. "https://overcast.fm/itunes1234567890"
	Title   string // podcast title as displayed in Overcast
}

// feedCellRe matches a subscribed podcast anchor on the /podcasts page.
// Attribute order in the <a> tag is unconstrained (href may precede or follow class).
//
//	1. attributes before href (checked for "feedcell" class)
//	2. podcast page path
//	3. attributes after href (checked for "feedcell" class)
//	4. cell body HTML (searched for the title element)
//
// Two URL formats are used by Overcast:
//   - iTunes-indexed:  /itunes{ID}/{slug}  e.g. /itunes917918570/serial
//   - Private/direct:  /p{ID}-{hash}       e.g. /p2537820-KcG3mF
//
// The /p format starts with a digit immediately after /p (distinguishing it
// from /podcasts and other /p... paths on the page).
var feedCellRe = regexp.MustCompile(
	`(?s)<a\b([^>]*)\bhref="(/(?:itunes[^"]+|p\d[^"]*))\"([^>]*)>(.*?)</a>`)

// cellTitleRe extracts the podcast title from inside a feed cell body.
// Tries several candidate class names used by Overcast across page variants:
// title2, title, feedtitle. The first match wins.
var cellTitleRe = regexp.MustCompile(
	`<[^>]+\bclass="[^"]*\b(?:title2|title|feedtitle)\b[^"]*"[^>]*>([^<]+)<`)

// FetchSubscribedPodcasts returns every podcast currently subscribed in the
// authenticated Overcast account by fetching the /podcasts page. One request
// replaces the per-podcast search_autocomplete calls previously needed to
// resolve each podcast's listing page URL, and it surfaces private
// subscriptions that have no iTunes ID and therefore cannot be found by search.
//
// The client must be authenticated (obtained from Login).
func FetchSubscribedPodcasts(ctx context.Context, client *http.Client) ([]SubscribedPodcast, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, overcastBaseURL+"/podcasts", nil)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: build /podcasts request: %w", err)
	}
	req.Header.Set("User-Agent", overcastUA)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("overcast/web: GET /podcasts: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{Wait: rateLimitWait(resp, 30*time.Second)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("overcast/web: /podcasts returned HTTP %d", resp.StatusCode)
	}

	var podcasts []SubscribedPodcast
	for _, m := range feedCellRe.FindAllSubmatch(body, -1) {
		// m[1]: attrs before href, m[3]: attrs after href.
		// At least one must contain "feedcell" to confirm this is a podcast cell
		// (not an unrelated link to /itunes... elsewhere on the page).
		attrsBefore, attrsAfter := string(m[1]), string(m[3])
		if !strings.Contains(attrsBefore, "feedcell") && !strings.Contains(attrsAfter, "feedcell") {
			continue
		}

		path := string(m[2])
		cellBody := m[4]

		title := ""
		if tm := cellTitleRe.FindSubmatch(cellBody); tm != nil {
			title = strings.TrimSpace(htmlpkg.UnescapeString(string(tm[1])))
		}
		if title == "" {
			continue // no usable title — skip
		}
		podcasts = append(podcasts, SubscribedPodcast{
			PageURL: overcastBaseURL + path,
			Title:   title,
		})
	}

	// Always save the raw HTML so structural issues can be diagnosed by
	// inspecting which cells were missed (not just when 0 results).
	debugPath := filepath.Join(os.TempDir(), "overcast-podcasts-page-debug.html")
	_ = os.WriteFile(debugPath, body, 0644)
	if len(podcasts) == 0 {
		fmt.Printf("overcast: /podcasts page parsed 0 podcasts — raw HTML saved to %s for inspection\n", debugPath)
	}

	return podcasts, nil
}

// subscribeFormRe matches POST forms on an Overcast podcast page.
//
//	1. podcast page path used as form action (e.g. "/itunes1234567890")
//	2. form body HTML (searched for hidden inputs and the subscribe button)
var subscribeFormRe = regexp.MustCompile(
	`(?si)<form\b[^>]*\bmethod="[Pp][Oo][Ss][Tt]"[^>]*\baction="(/[^"]+)"[^>]*>(.*?)</form>`)

// hiddenInputTagRe matches a single <input type="hidden"> tag.
var hiddenInputTagRe = regexp.MustCompile(`(?i)<input\b[^>]*\btype="hidden"[^>]*>`)

// inputAttrNameRe / inputAttrValueRe extract name/value from an input tag.
var inputAttrNameRe = regexp.MustCompile(`\bname="([^"]+)"`)
var inputAttrValueRe = regexp.MustCompile(`\bvalue="([^"]*)"`)

// SubscribeToPodcast subscribes to a podcast by locating and posting the subscribe
// form on its Overcast podcast listing page. It is a no-op (returns nil) when the
// page shows an unsubscribe option instead, meaning the account is already subscribed.
//
// The client must be authenticated (obtained from Login).
func SubscribeToPodcast(ctx context.Context, client *http.Client, podcastPageURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, podcastPageURL, nil)
	if err != nil {
		return fmt.Errorf("overcast/web: subscribe page request: %w", err)
	}
	req.Header.Set("User-Agent", overcastUA)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("overcast/web: GET %s: %w", podcastPageURL, err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{Wait: rateLimitWait(resp, 30*time.Second)}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("overcast/web: GET %s returned HTTP %d", podcastPageURL, resp.StatusCode)
	}

	// Find a POST form whose body contains "subscribe" but not "unsubscribe".
	// Overcast renders an unsubscribe form when already subscribed — we treat
	// that as a no-op rather than an error.
	var formAction string
	formFields := url.Values{}

	for _, m := range subscribeFormRe.FindAllSubmatch(body, -1) {
		action := string(m[1])
		formBody := m[2]
		bodyLower := strings.ToLower(string(formBody))
		if strings.Contains(bodyLower, "unsubscribe") {
			return nil // already subscribed — nothing to do
		}
		if !strings.Contains(bodyLower, "subscribe") {
			continue // unrelated form
		}
		// Winning form: extract hidden fields.
		formAction = action
		for _, tag := range hiddenInputTagRe.FindAll(formBody, -1) {
			tagStr := string(tag)
			nm := inputAttrNameRe.FindStringSubmatch(tagStr)
			val := inputAttrValueRe.FindStringSubmatch(tagStr)
			if len(nm) > 1 && len(val) > 1 {
				formFields.Set(nm[1], htmlpkg.UnescapeString(val[1]))
			}
		}
		break
	}

	if formAction == "" {
		return fmt.Errorf("overcast/web: subscribe form not found on %s (page may be JS-rendered or already subscribed)", podcastPageURL)
	}

	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		overcastBaseURL+formAction, strings.NewReader(formFields.Encode()))
	if err != nil {
		return fmt.Errorf("overcast/web: build subscribe POST: %w", err)
	}
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("User-Agent", overcastUA)
	postReq.Header.Set("Referer", podcastPageURL)

	postResp, err := client.Do(postReq)
	if err != nil {
		return fmt.Errorf("overcast/web: subscribe POST: %w", err)
	}
	_, _ = io.Copy(io.Discard, postResp.Body)
	_ = postResp.Body.Close()

	if postResp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{Wait: rateLimitWait(postResp, 30*time.Second)}
	}
	if postResp.StatusCode >= 400 {
		return fmt.Errorf("overcast/web: subscribe POST to %s returned HTTP %d",
			overcastBaseURL+formAction, postResp.StatusCode)
	}
	return nil
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
