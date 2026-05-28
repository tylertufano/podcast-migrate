package overcast

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// overcastOPML captures the extended OPML schema that Overcast exports.
// Overcast adds per-episode progress attributes on <outline type="podcast">.
type overcastOPML struct {
	XMLName xml.Name         `xml:"opml"`
	Version string           `xml:"version,attr"`
	Head    overcastHead     `xml:"head"`
	Body    overcastBody     `xml:"body"`
}

type overcastHead struct {
	Title string `xml:"title"`
}

type overcastBody struct {
	Outlines []overcastFeedOutline `xml:"outline"`
}

// overcastFeedOutline represents a subscribed podcast.
type overcastFeedOutline struct {
	Text    string                   `xml:"text,attr"`
	Type    string                   `xml:"type,attr"`
	XMLURL  string                   `xml:"xmlUrl,attr"`
	HTMLURL string                   `xml:"htmlUrl,attr"`
	// Overcast nests episode outlines inside each feed outline.
	Episodes []overcastEpisodeOutline `xml:"outline"`
}

// overcastEpisodeOutline represents a single episode with play state.
// Overcast uses non-standard attributes: overcastId, played, progress.
type overcastEpisodeOutline struct {
	Type       string `xml:"type,attr"`
	Title      string `xml:"title,attr"`
	URL        string `xml:"url,attr"`       // enclosure URL
	GUID       string `xml:"overcastId,attr"` // Overcast internal ID, not RSS GUID
	Played     string `xml:"played,attr"`    // "1" or "0"
	Progress   string `xml:"progress,attr"`  // seconds as string
	PubDate    string `xml:"pubDate,attr"`   // RFC 1123
}

// OPMLReader parses an Overcast OPML export (downloaded from overcast.fm/account/export_opml).
type OPMLReader struct {
	path string
}

func NewOPMLReader(path string) *OPMLReader {
	return &OPMLReader{path: path}
}

func (r *OPMLReader) Read(_ context.Context) (*model.Library, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return nil, fmt.Errorf("overcast/opml: read %s: %w", r.path, err)
	}

	var doc overcastOPML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("overcast/opml: parse: %w", err)
	}

	var podcasts []model.Podcast
	var episodes []model.EpisodeState

	for _, feed := range doc.Body.Outlines {
		if feed.XMLURL == "" {
			continue
		}
		podcasts = append(podcasts, model.Podcast{
			FeedURL: feed.XMLURL,
			Title:   feed.Text,
		})
		for _, ep := range feed.Episodes {
			es := parseEpisodeOutline(ep, feed.XMLURL)
			if es != nil {
				episodes = append(episodes, *es)
			}
		}
	}

	return &model.Library{
		Podcasts:       podcasts,
		Episodes:       episodes,
		ExportedAt:     time.Now(),
		SourceProvider: "Overcast (OPML)",
	}, nil
}

func parseEpisodeOutline(ep overcastEpisodeOutline, feedURL string) *model.EpisodeState {
	if ep.Title == "" {
		return nil
	}
	es := &model.EpisodeState{
		GUID:    ep.GUID,
		FeedURL: feedURL,
		Title:   ep.Title,
	}
	if ep.Played == "1" {
		es.PlayState = model.PlayStatePlayed
	}
	if secs := parseFloat(ep.Progress); secs > 0 {
		es.PlayPosition = time.Duration(secs * float64(time.Second))
		if es.PlayState == model.PlayStateUnplayed {
			es.PlayState = model.PlayStateInProgress
		}
	}
	if ep.PubDate != "" {
		if t, err := time.Parse(time.RFC1123, strings.TrimSpace(ep.PubDate)); err == nil {
			es.PubDate = t
		}
	}
	return es
}

// OPMLWriter generates an OPML file suitable for Overcast's subscription import.
// It writes subscriptions only — Overcast's import does not accept play state.
type OPMLWriter struct{}

func (w *OPMLWriter) Write(lib *model.Library, path string) error {
	doc := overcastOPML{
		Version: "2.0",
		Head:    overcastHead{Title: "Podcast Subscriptions"},
		Body:    overcastBody{},
	}

	for _, p := range lib.Podcasts {
		doc.Body.Outlines = append(doc.Body.Outlines, overcastFeedOutline{
			Type:   "rss",
			Text:   p.Title,
			XMLURL: p.FeedURL,
		})
	}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("overcast/opml: marshal: %w", err)
	}

	content := append([]byte(xml.Header), out...)
	// Ensure a trailing newline.
	content = append(bytes.TrimRight(content, "\n"), '\n')

	if err := os.WriteFile(path, content, 0644); err != nil {
		return fmt.Errorf("overcast/opml: write %s: %w", path, err)
	}
	return nil
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}
