// Package opml implements a generic OPML provider that can read and write
// both standard podcast subscription OPML and an extended format that includes
// per-episode play state.
//
// Extended format is compatible with Overcast's extended OPML export:
// episodes are nested <outline> elements inside each feed <outline>, with
// attributes type="podcast-episode", title, pubDate (RFC3339), played ("1"/"0"),
// and progress (seconds as a decimal string).
//
// Reading:
//   - Standard OPML: feed outlines directly in <body>; no episode data.
//   - Extended OPML: feed outlines may be nested inside a group container
//     (e.g. <outline text="feeds">), with episode outlines nested inside feeds.
//   - Overcast extended export format is fully supported as input.
//
// Writing:
//   - Feed outlines are written directly inside <body> (no group container).
//   - When extended=true, played and in-progress episodes are nested inside
//     their feed outline. Unplayed episodes are omitted.
//   - When extended=false (or OnlySubscriptions), only feed outlines are written.
package opml

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// ---- XML types used for writing ----

type writeDoc struct {
	XMLName xml.Name      `xml:"opml"`
	Version string        `xml:"version,attr"`
	Head    writeHead     `xml:"head"`
	Body    writeBody     `xml:"body"`
}

type writeHead struct {
	Title string `xml:"title"`
}

type writeBody struct {
	Outlines []feedOutline `xml:"outline"`
}

// feedOutline represents a subscribed podcast.
type feedOutline struct {
	Type     string           `xml:"type,attr"`
	Text     string           `xml:"text,attr"`
	XMLURL   string           `xml:"xmlUrl,attr"`
	HTMLURL  string           `xml:"htmlUrl,attr,omitempty"`
	Episodes []episodeOutline `xml:"outline"`
}

// episodeOutline represents a single episode with play state.
type episodeOutline struct {
	Type     string `xml:"type,attr"`
	Title    string `xml:"title,attr"`
	URL      string `xml:"url,attr,omitempty"`
	PubDate  string `xml:"pubDate,attr,omitempty"`
	Played   string `xml:"played,attr,omitempty"`
	Progress string `xml:"progress,attr,omitempty"`
}

// ---- XML types used for reading ----

// readDoc is used exclusively for parsing. A single flexible node type covers
// group containers, feed outlines, and episode outlines, since Go's xml package
// cannot map two sibling fields to the same element name.
type readDoc struct {
	XMLName xml.Name   `xml:"opml"`
	Body    struct {
		Nodes []readNode `xml:"outline"`
	} `xml:"body"`
}

type readNode struct {
	Text      string     `xml:"text,attr"`
	Type      string     `xml:"type,attr"`
	XMLURL    string     `xml:"xmlUrl,attr"`
	Title     string     `xml:"title,attr"`
	URL       string     `xml:"url,attr"`
	PubDate   string     `xml:"pubDate,attr"`
	Played    string     `xml:"played,attr"`
	Progress  string     `xml:"progress,attr"`
	OvercastID string    `xml:"overcastId,attr"` // Overcast compatibility
	Children  []readNode `xml:"outline"`
}

// ---- Reader ----

// Reader reads an OPML file and returns a model.Library.
type Reader struct {
	path string
}

// NewReader returns a Reader for the OPML file at path.
func NewReader(path string) *Reader {
	return &Reader{path: path}
}

// Read reads and parses the OPML file.
func (r *Reader) Read() (*model.Library, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return nil, fmt.Errorf("opml: read %s: %w", r.path, err)
	}
	return Parse(data)
}

// Parse parses raw OPML XML bytes and returns a Library.
// It handles both standard OPML (subscriptions only) and extended OPML
// (with nested episode play state), including Overcast's extended format.
func Parse(data []byte) (*model.Library, error) {
	var doc readDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("opml: parse: %w", err)
	}

	var podcasts []model.Podcast
	var episodes []model.EpisodeState

	for _, node := range doc.Body.Nodes {
		var feedNodes []readNode
		if node.XMLURL != "" {
			// Standard: feed outline is directly inside <body>.
			feedNodes = append(feedNodes, node)
		} else {
			// Extended: group container (e.g. <outline text="feeds">).
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
				FeedURL:    feed.XMLURL,
				Title:      name,
				OvercastID: feed.OvercastID,
			})
			for _, ep := range feed.Children {
				if es := parseEpisodeNode(ep, feed.XMLURL); es != nil {
					episodes = append(episodes, *es)
				}
			}
		}
	}

	src := "OPML"
	if len(episodes) > 0 {
		src = "OPML (extended)"
	}
	return &model.Library{
		Podcasts:       podcasts,
		Episodes:       episodes,
		ExportedAt:     time.Now(),
		SourceProvider: src,
	}, nil
}

// parseEpisodeNode converts an episode readNode into a model.EpisodeState.
// Returns nil if the node has no title (i.e. is not a recognisable episode outline).
func parseEpisodeNode(n readNode, feedURL string) *model.EpisodeState {
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
	if secs := parseProgress(n.Progress); secs > 0 {
		es.PlayPosition = time.Duration(secs * float64(time.Second))
		if es.PlayState == model.PlayStateUnplayed {
			es.PlayState = model.PlayStateInProgress
		}
	}
	if n.PubDate != "" {
		raw := strings.TrimSpace(n.PubDate)
		for _, layout := range []string{time.RFC3339, time.RFC1123Z, time.RFC1123} {
			if t, err := time.Parse(layout, raw); err == nil {
				es.PubDate = t
				break
			}
		}
	}
	return es
}

func parseProgress(s string) float64 {
	if s == "" {
		return 0
	}
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

// ---- Writer ----

// Writer generates an OPML file from a model.Library.
// When Extended is true, played and in-progress episodes are included as nested
// outlines inside each feed outline (Overcast-compatible extended format).
// When Extended is false, only feed outlines are written (standard OPML).
type Writer struct {
	Extended bool
}

// Write serialises lib as OPML and writes it to path.
func (w *Writer) Write(lib *model.Library, path string) error {
	// Build a per-feed episode index when writing extended OPML.
	epsByFeed := make(map[string][]model.EpisodeState)
	if w.Extended {
		for _, ep := range lib.Episodes {
			if ep.FromDestination || ep.PlayState == model.PlayStateUnplayed {
				continue
			}
			// Skip episodes with no title: they can't be matched by any destination
			// provider. These occur when the episode's GUID is absent from the current
			// RSS feed (e.g. the episode has rotated off, or the podcast changed hosts
			// and GUIDs changed in the process).
			if ep.Title == "" {
				continue
			}
			epsByFeed[ep.FeedURL] = append(epsByFeed[ep.FeedURL], ep)
		}
	}

	doc := writeDoc{
		Version: "2.0",
		Head:    writeHead{Title: "Podcast Library"},
	}

	for _, p := range lib.Podcasts {
		fo := feedOutline{
			Type:   "rss",
			Text:   p.Title,
			XMLURL: p.FeedURL,
		}
		for _, ep := range epsByFeed[p.FeedURL] {
			eo := episodeOutline{
				Type:  "podcast-episode",
				Title: ep.Title,
			}
			if !ep.PubDate.IsZero() {
				eo.PubDate = ep.PubDate.UTC().Format(time.RFC3339)
			}
			switch ep.PlayState {
			case model.PlayStatePlayed:
				eo.Played = "1"
				if ep.PlayPosition > 0 {
					eo.Progress = fmt.Sprintf("%.1f", ep.PlayPosition.Seconds())
				}
			case model.PlayStateInProgress:
				eo.Played = "0"
				eo.Progress = fmt.Sprintf("%.1f", ep.PlayPosition.Seconds())
			}
			fo.Episodes = append(fo.Episodes, eo)
		}
		doc.Body.Outlines = append(doc.Body.Outlines, fo)
	}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("opml: marshal: %w", err)
	}
	content := append([]byte(xml.Header), out...)
	content = append(bytes.TrimRight(content, "\n"), '\n')
	if err := os.WriteFile(path, content, 0644); err != nil {
		return fmt.Errorf("opml: write %s: %w", path, err)
	}
	return nil
}
