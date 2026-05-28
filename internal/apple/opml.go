package apple

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// opmlDoc is the top-level structure of an OPML 2.0 file.
type opmlDoc struct {
	XMLName xml.Name `xml:"opml"`
	Body    opmlBody `xml:"body"`
}

type opmlBody struct {
	Outlines []opmlOutline `xml:"outline"`
}

type opmlOutline struct {
	Text     string `xml:"text,attr"`
	Type     string `xml:"type,attr"`
	XMLURL   string `xml:"xmlUrl,attr"`
	HTMLURL  string `xml:"htmlUrl,attr"`
	// Apple Podcasts groups feeds inside a single outer outline.
	Outlines []opmlOutline `xml:"outline"`
}

// OPMLReader reads subscriptions from an OPML file exported via
// Apple Podcasts > File > Export Subscriptions.
// It does not carry episode play state.
type OPMLReader struct {
	path string
}

func NewOPMLReader(path string) *OPMLReader {
	return &OPMLReader{path: path}
}

func (r *OPMLReader) Read(_ context.Context) (*model.Library, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, fmt.Errorf("apple/opml: open %s: %w", r.path, err)
	}
	defer f.Close()

	var doc opmlDoc
	if err := xml.NewDecoder(f).Decode(&doc); err != nil {
		return nil, fmt.Errorf("apple/opml: parse: %w", err)
	}

	podcasts := collectFeeds(doc.Body.Outlines)
	return &model.Library{
		Podcasts:       podcasts,
		ExportedAt:     time.Now(),
		SourceProvider: "Apple Podcasts (OPML)",
	}, nil
}

// collectFeeds recursively extracts podcast entries from OPML outlines.
func collectFeeds(outlines []opmlOutline) []model.Podcast {
	var out []model.Podcast
	for _, o := range outlines {
		if o.XMLURL != "" {
			out = append(out, model.Podcast{
				FeedURL: o.XMLURL,
				Title:   o.Text,
			})
		}
		// Recurse into groups (Apple Podcasts wraps feeds in a folder outline).
		out = append(out, collectFeeds(o.Outlines)...)
	}
	return out
}
