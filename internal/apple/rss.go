package apple

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

// fetchRSSFeed fetches and parses an RSS feed. Returns (rssFeed{}, nil) when
// the URL returns 4xx/5xx so callers can still use KVS data for the feed.
func fetchRSSFeed(ctx context.Context, client *http.Client, feedURL string) (rssFeed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return rssFeed{}, fmt.Errorf("rss request: %w", err)
	}
	req.Header.Set("User-Agent", "podcast-migrate/1.0 (+https://github.com/tylertufano/podcast-migrate)")
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml, */*")

	resp, err := client.Do(req)
	if err != nil {
		return rssFeed{}, fmt.Errorf("rss fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return rssFeed{}, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return rssFeed{}, fmt.Errorf("rss read: %w", err)
	}

	var raw rssFeedRaw
	if err := xml.Unmarshal(body, &raw); err != nil {
		return rssFeed{}, fmt.Errorf("rss parse: %w", err)
	}
	ch := raw.Channel

	feed := rssFeed{
		Title:  ch.Title,
		Author: ch.ItunesAuthor,
	}
	if ch.ItunesImage.Href != "" {
		feed.ImageURL = ch.ItunesImage.Href
	} else {
		feed.ImageURL = ch.Image.URL
	}

	for _, raw := range ch.Items {
		guid := strings.TrimSpace(raw.GUID.Value)
		if guid == "" {
			guid = raw.Enclosure.URL
		}
		item := rssItem{
			GUID:      guid,
			Title:     strings.TrimSpace(raw.Title),
			PubDate:   parsePubDate(raw.PubDate),
			Duration:  parseItunesDuration(raw.ItunesDuration),
			EnclosURL: raw.Enclosure.URL,
		}
		feed.Items = append(feed.Items, item)
	}
	return feed, nil
}
