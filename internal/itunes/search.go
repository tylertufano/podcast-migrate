package itunes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const userAgent = "podcast-migrate/1.0 (+https://github.com/tylertufano/podcast-migrate)"

// SearchResult holds the iTunes Store match for a podcast.
type SearchResult struct {
	CollectionID int64
	FeedURL      string
	Title        string
	Author       string
}

// FindByHints searches the iTunes Store for a podcast using title as the query
// and scores candidates using feedURL and author as disambiguation hints.
//
// Scoring per candidate:
//
//	+100  exact normalised feed URL match
//	 +50  exact case-insensitive title match
//	 +30  exact case-insensitive author match
//	 +20  same eTLD+1 domain as feedURL (weak tiebreaker)
//
// Acceptance thresholds (applied to the unique best-scoring candidate):
//
//	≥100  accepted   (URL match)
//	 ≥80  accepted   (title + author, or URL domain + title + ...)
//	 ≥50  accepted   when the candidate has an exact title match
//	ties  rejected   (multiple candidates at the best score)
//	 <50  rejected
//
// Returns a zero SearchResult when no candidate meets the thresholds.
// Never returns an error for "not found" — only for network or parse failures.
func FindByHints(ctx context.Context, client *http.Client, title, feedURL, author string) (SearchResult, error) {
	if title == "" {
		return SearchResult{}, nil
	}

	searchURL := "https://itunes.apple.com/search?" + url.Values{
		"term":      {title},
		"media":     {"podcast"},
		"entity":    {"podcast"},
		"attribute": {"titleTerm"},
		"limit":     {"10"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return SearchResult{}, fmt.Errorf("itunes search: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return SearchResult{}, fmt.Errorf("itunes search: request: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	resp.Body.Close()
	if readErr != nil {
		return SearchResult{}, fmt.Errorf("itunes search: read: %w", readErr)
	}
	if resp.StatusCode != 200 {
		return SearchResult{}, fmt.Errorf("itunes search: HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Results []struct {
			CollectionID int64  `json:"collectionId"`
			FeedURL      string `json:"feedUrl"`
			Title        string `json:"collectionName"`
			Author       string `json:"artistName"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return SearchResult{}, fmt.Errorf("itunes search: parse: %w", err)
	}

	wantTitle := strings.ToLower(strings.TrimSpace(title))
	wantFeed := normalizeURL(feedURL)
	wantAuthor := strings.ToLower(strings.TrimSpace(author))
	wantDomain := etldPlusOne(feedURL)

	type candidate struct {
		result     SearchResult
		score      int
		exactTitle bool
	}

	var candidates []candidate
	for _, r := range payload.Results {
		if r.CollectionID <= 0 || r.FeedURL == "" {
			continue
		}
		var score int
		normFeed := normalizeURL(r.FeedURL)
		if wantFeed != "" && normFeed == wantFeed {
			score += 100
		} else if wantDomain != "" && etldPlusOne(r.FeedURL) == wantDomain {
			score += 20
		}
		gotTitle := strings.ToLower(strings.TrimSpace(r.Title))
		exactTitle := gotTitle == wantTitle
		if exactTitle {
			score += 50
		}
		if wantAuthor != "" && strings.ToLower(strings.TrimSpace(r.Author)) == wantAuthor {
			score += 30
		}
		if score > 0 {
			candidates = append(candidates, candidate{
				result: SearchResult{
					CollectionID: r.CollectionID,
					FeedURL:      r.FeedURL,
					Title:        r.Title,
					Author:       r.Author,
				},
				score:      score,
				exactTitle: exactTitle,
			})
		}
	}

	if len(candidates) == 0 {
		return SearchResult{}, nil
	}

	best := 0
	for _, c := range candidates {
		if c.score > best {
			best = c.score
		}
	}
	if best < 50 {
		return SearchResult{}, nil
	}

	var topTier []candidate
	for _, c := range candidates {
		if c.score == best {
			topTier = append(topTier, c)
		}
	}
	if len(topTier) > 1 {
		return SearchResult{}, nil // ambiguous — reject
	}

	winner := topTier[0]
	if best >= 100 || best >= 80 || winner.exactTitle {
		return winner.result, nil
	}
	return SearchResult{}, nil
}

// normalizeURL strips the scheme, lowercases host+path, and removes trailing
// slashes. Used for exact URL comparison that is scheme-agnostic.
func normalizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	return strings.ToLower(strings.TrimRight(u.Host+u.Path, "/"))
}

// etldPlusOne returns a best-effort effective TLD+1 for a URL by taking the
// last two dot-separated labels of the hostname. This is intentionally simple
// since it is used only as a ±20 tiebreaker signal.
func etldPlusOne(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}
