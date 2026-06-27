package apple

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/httputil"
)

// rssFeed is the minimal parsed representation of an RSS 2.0 feed.
type rssFeed struct {
	Title    string       `xml:"channel>title"`
	Author   string       `xml:"-"` // populated from itunes:author
	ImageURL string       `xml:"-"` // populated from itunes:image or image>url
	Items    []rssItem    `xml:"channel>item"`
}

// rssItem is one episode from an RSS feed.
type rssItem struct {
	GUID      string    // from <guid> or <enclosure url>
	Title     string
	PubDate   time.Time
	Duration  time.Duration
	EnclosURL string
}

// rssFeedRaw is used for xml.Unmarshal; post-processed into rssFeed.
type rssFeedRaw struct {
	Channel rssChannelRaw `xml:"channel"`
}

type rssChannelRaw struct {
	Title        string        `xml:"title"`
	ItunesAuthor string        `xml:"author"`
	Image        rssImageRaw   `xml:"image"`
	ItunesImage  rssItunesImg  `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
	Items        []rssItemRaw  `xml:"item"`
}

type rssImageRaw struct {
	URL string `xml:"url"`
}

type rssItunesImg struct {
	Href string `xml:"href,attr"`
}

type rssItemRaw struct {
	Title          string         `xml:"title"`
	GUID           rssGUIDRaw     `xml:"guid"`
	PubDate        string         `xml:"pubDate"`
	ItunesDuration string         `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
	Enclosure      rssEnclosureRaw `xml:"enclosure"`
}

type rssGUIDRaw struct {
	Value string `xml:",chardata"`
}

type rssEnclosureRaw struct {
	URL string `xml:"url,attr"`
}

// pubDateLayouts are the date formats seen in real-world RSS feeds.
var pubDateLayouts = []string{
	time.RFC1123Z, // "Mon, 02 Jan 2006 15:04:05 -0700"
	time.RFC1123,  // "Mon, 02 Jan 2006 15:04:05 MST"
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 2006 15:04:05 +0000",
	"2 Jan 2006 15:04:05 -0700",
	"2006-01-02T15:04:05Z07:00", // ISO 8601
}

func parsePubDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range pubDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// parseItunesDuration parses an itunes:duration field. Accepts "HH:MM:SS",
// "MM:SS", or a plain integer number of seconds.
func parseItunesDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		var secs int
		if _, err := fmt.Sscanf(s, "%d", &secs); err == nil {
			return time.Duration(secs) * time.Second
		}
	case 2:
		var m, sec int
		if _, err := fmt.Sscanf(parts[0], "%d", &m); err == nil {
			if _, err := fmt.Sscanf(parts[1], "%d", &sec); err == nil {
				return time.Duration(m*60+sec) * time.Second
			}
		}
	case 3:
		var h, m, sec int
		if _, err := fmt.Sscanf(parts[0], "%d", &h); err == nil {
			if _, err := fmt.Sscanf(parts[1], "%d", &m); err == nil {
				if _, err := fmt.Sscanf(parts[2], "%d", &sec); err == nil {
					return time.Duration(h*3600+m*60+sec) * time.Second
				}
			}
		}
	}
	return 0
}

// fetchRSSFeed fetches and parses an RSS feed. Network errors, 5xx, and
// truncated XML are retried up to 2 times with exponential backoff.
// Returns (rssFeed{}, nil) for 4xx and empty responses so callers can
// still use KVS data for the feed.
func fetchRSSFeed(ctx context.Context, client *http.Client, feedURL string) (rssFeed, error) {
	var out rssFeed
	err := httputil.RetryFunc(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
		if err != nil {
			return fmt.Errorf("rss request: %w", err) // permanent — bad URL
		}
		req.Header.Set("User-Agent", "podcast-migrate/1.0 (+https://github.com/tylertufano/podcast-migrate)")
		req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml, */*")

		resp, err := client.Do(req)
		if err != nil {
			return httputil.NewTransientError(fmt.Errorf("rss fetch: %w", err))
		}
		defer resp.Body.Close()

		if resp.StatusCode == 429 {
			return &httputil.RateLimitError{Wait: httputil.ParseRetryAfter(resp, 30*time.Second)}
		}
		if resp.StatusCode >= 500 {
			return httputil.NewTransientError(fmt.Errorf("rss fetch: HTTP %d", resp.StatusCode))
		}
		if resp.StatusCode >= 400 {
			return nil // auth-gated or gone — not retriable, treat as empty
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
		if err != nil {
			return httputil.NewTransientError(fmt.Errorf("rss read: %w", err))
		}
		if len(body) == 0 {
			return nil // empty response — treat as empty, not retriable
		}

		var raw rssFeedRaw
		if err := xml.Unmarshal(body, &raw); err != nil {
			return httputil.NewTransientError(fmt.Errorf("rss parse: %w", err))
		}

		ch := raw.Channel
		out = rssFeed{
			Title:  ch.Title,
			Author: ch.ItunesAuthor,
		}
		if ch.ItunesImage.Href != "" {
			out.ImageURL = ch.ItunesImage.Href
		} else {
			out.ImageURL = ch.Image.URL
		}
		for _, item := range ch.Items {
			guid := strings.TrimSpace(item.GUID.Value)
			if guid == "" {
				guid = item.Enclosure.URL
			}
			out.Items = append(out.Items, rssItem{
				GUID:      guid,
				Title:     strings.TrimSpace(item.Title),
				PubDate:   parsePubDate(item.PubDate),
				Duration:  parseItunesDuration(item.ItunesDuration),
				EnclosURL: item.Enclosure.URL,
			})
		}
		return nil
	}, httputil.RetryOptions{MaxTransientAttempts: 2, BaseDelay: 1 * time.Second})
	return out, err
}
