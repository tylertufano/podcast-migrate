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
	HTMLURL string                   `xml:"htmlUrl,attr,omitempty"` // omit when empty — some parsers reject empty attributes
	// Overcast nests episode outlines inside each feed outline.
	Episodes []overcastEpisodeOutline `xml:"outline"`
}

// overcastEpisodeOutline represents a single episode with play state in the BASIC
// OPML export format (used by the writer tests). The extended export uses opmlNode.
type overcastEpisodeOutline struct {
	Type        string `xml:"type,attr"`
	Title       string `xml:"title,attr"`
	URL         string `xml:"url,attr"`
	GUID        string `xml:"overcastId,attr"`
	OvercastURL string `xml:"overcastUrl,attr"`
	Played      string `xml:"played,attr"`
	Progress    string `xml:"progress,attr"`
	PubDate     string `xml:"pubDate,attr"`
}

// opmlNode is a flexible outline element used when reading OPML.
// A single type covers group containers, feed outlines, and episode outlines
// since Go's xml package cannot map two sibling fields to the same element name.
type opmlNode struct {
	// Common to all outline types
	Text string `xml:"text,attr"`
	Type string `xml:"type,attr"`

	// Feed outline attributes (type="rss")
	XMLURL  string `xml:"xmlUrl,attr"`
	HTMLURL string `xml:"htmlUrl,attr,omitempty"`

	// Episode outline attributes (type="podcast-episode" / "podcast")
	Title       string `xml:"title,attr"`
	URL         string `xml:"url,attr"`
	OvercastID  string `xml:"overcastId,attr"` // numeric ID == data-item-id for set_progress
	OvercastURL string `xml:"overcastUrl,attr"`
	Played      string `xml:"played,attr"`
	Progress    string `xml:"progress,attr"`
	PubDate     string `xml:"pubDate,attr"` // RFC3339 (extended) or RFC1123Z (basic)

	Children []opmlNode `xml:"outline"`
}

// opmlReadDoc is used exclusively for parsing. The writer uses overcastOPML instead.
type opmlReadDoc struct {
	XMLName xml.Name `xml:"opml"`
	Body    struct {
		Nodes []opmlNode `xml:"outline"`
	} `xml:"body"`
}

// OPMLReader parses an Overcast OPML export (downloaded from overcast.fm/account/export_opml).
// It handles both the basic export (feeds directly inside <body>) and the extended export
// (feeds inside a <outline text="feeds"> group container).
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

	var doc opmlReadDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("overcast/opml: parse: %w", err)
	}

	var podcasts []model.Podcast
	var episodes []model.EpisodeState

	for _, node := range doc.Body.Nodes {
		var feedNodes []opmlNode
		if node.XMLURL != "" {
			// Basic export: feed outline directly inside <body>.
			feedNodes = append(feedNodes, node)
		} else {
			// Extended export: group container (e.g. <outline text="feeds">).
			// The actual feed outlines are one level deeper.
			for _, child := range node.Children {
				if child.XMLURL != "" {
					feedNodes = append(feedNodes, child)
				}
			}
		}

		for _, feed := range feedNodes {
			name := feed.Text
			if name == "" {
				name = feed.Title
			}
			podcasts = append(podcasts, model.Podcast{
				FeedURL: feed.XMLURL,
				Title:   name,
			})
			for _, ep := range feed.Children {
				es := parseOpmlNode(ep, feed.XMLURL)
				if es != nil {
					episodes = append(episodes, *es)
				}
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

// parseOpmlNode converts an episode opmlNode into a model.EpisodeState.
// Returns nil if the node has no title (i.e. is not a recognisable episode outline).
func parseOpmlNode(n opmlNode, feedURL string) *model.EpisodeState {
	if n.Title == "" {
		return nil
	}
	es := &model.EpisodeState{
		GUID:    n.OvercastID,
		FeedURL: feedURL,
		Title:   n.Title,
	}
	if n.Played == "1" {
		es.PlayState = model.PlayStatePlayed
	}
	if secs := parseFloat(n.Progress); secs > 0 {
		es.PlayPosition = time.Duration(secs * float64(time.Second))
		if es.PlayState == model.PlayStateUnplayed {
			es.PlayState = model.PlayStateInProgress
		}
	}
	if n.PubDate != "" {
		raw := strings.TrimSpace(n.PubDate)
		// Extended OPML exports use RFC3339 (e.g. "2021-05-25T15:30:06-04:00").
		// Basic exports use RFC1123Z (e.g. "Mon, 15 Jan 2024 12:00:00 +0000").
		// Try most-specific first so we don't misparse.
		for _, layout := range []string{time.RFC3339, time.RFC1123Z, time.RFC1123} {
			if t, err := time.Parse(layout, raw); err == nil {
				es.PubDate = t
				break
			}
		}
	}
	return es
}

// parseEpisodeOutline is kept for compatibility with tests that rely on the
// basic OPML episode format (type="podcast", overcastId, played, progress).
func parseEpisodeOutline(ep overcastEpisodeOutline, feedURL string) *model.EpisodeState {
	return parseOpmlNode(opmlNode{
		Title:      ep.Title,
		OvercastID: ep.GUID,
		Played:     ep.Played,
		Progress:   ep.Progress,
		PubDate:    ep.PubDate,
		URL:        ep.URL,
	}, feedURL)
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
