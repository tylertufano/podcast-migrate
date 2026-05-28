package overcast_test

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/overcast"
)

func writeOvercastOPML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "overcast.opml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write OPML: %v", err)
	}
	return path
}

// --- OPMLReader ---

func TestOvercastOPMLReader_ParsesSubscriptions(t *testing.T) {
	path := writeOvercastOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>Podcast Subscriptions</title></head>
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a"/>
    <outline text="Show B" type="rss" xmlUrl="https://feeds.example.com/show-b"/>
  </body>
</opml>`)

	lib, err := overcast.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lib.Podcasts) != 2 {
		t.Fatalf("got %d podcasts, want 2", len(lib.Podcasts))
	}
	if lib.Podcasts[0].FeedURL != "https://feeds.example.com/show-a" {
		t.Errorf("Podcasts[0].FeedURL: got %q", lib.Podcasts[0].FeedURL)
	}
	if lib.Podcasts[0].Title != "Show A" {
		t.Errorf("Podcasts[0].Title: got %q", lib.Podcasts[0].Title)
	}
}

func TestOvercastOPMLReader_SkipsFeedsWithoutXMLURL(t *testing.T) {
	path := writeOvercastOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="No URL outline" type="rss"/>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a"/>
  </body>
</opml>`)

	lib, err := overcast.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lib.Podcasts) != 1 {
		t.Errorf("got %d podcasts, want 1", len(lib.Podcasts))
	}
}

func TestOvercastOPMLReader_ParsesPlayedEpisode(t *testing.T) {
	path := writeOvercastOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a">
      <outline type="podcast" title="Episode One" url="https://media.example.com/ep1.mp3"
               overcastId="oc-id-1" played="1" progress="0"
               pubDate="Mon, 15 Jan 2024 12:00:00 +0000"/>
    </outline>
  </body>
</opml>`)

	lib, err := overcast.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lib.Episodes) != 1 {
		t.Fatalf("got %d episodes, want 1", len(lib.Episodes))
	}
	ep := lib.Episodes[0]
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("PlayState: got %d, want %d (Played)", ep.PlayState, model.PlayStatePlayed)
	}
	if ep.PlayPosition != 0 {
		t.Errorf("PlayPosition: got %v, want 0 (played with zero progress)", ep.PlayPosition)
	}
	if ep.GUID != "oc-id-1" {
		t.Errorf("GUID: got %q, want %q", ep.GUID, "oc-id-1")
	}
	if ep.FeedURL != "https://feeds.example.com/show-a" {
		t.Errorf("FeedURL: got %q", ep.FeedURL)
	}
}

func TestOvercastOPMLReader_ParsesInProgressEpisode(t *testing.T) {
	path := writeOvercastOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a">
      <outline type="podcast" title="Episode Two" url="https://media.example.com/ep2.mp3"
               overcastId="oc-id-2" played="0" progress="1500.5"
               pubDate="Tue, 16 Jan 2024 12:00:00 +0000"/>
    </outline>
  </body>
</opml>`)

	lib, err := overcast.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lib.Episodes) != 1 {
		t.Fatalf("got %d episodes, want 1", len(lib.Episodes))
	}
	ep := lib.Episodes[0]
	if ep.PlayState != model.PlayStateInProgress {
		t.Errorf("PlayState: got %d, want %d (InProgress)", ep.PlayState, model.PlayStateInProgress)
	}
	wantPos := time.Duration(1500.5 * float64(time.Second))
	if ep.PlayPosition != wantPos {
		t.Errorf("PlayPosition: got %v, want %v", ep.PlayPosition, wantPos)
	}
}

func TestOvercastOPMLReader_PlayedDominatesProgress(t *testing.T) {
	// An episode marked played=1 with a non-zero progress should be PlayStatePlayed,
	// not InProgress — played takes priority.
	path := writeOvercastOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a">
      <outline type="podcast" title="Episode Three" url="https://media.example.com/ep3.mp3"
               overcastId="oc-id-3" played="1" progress="900"/>
    </outline>
  </body>
</opml>`)

	lib, err := overcast.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lib.Episodes) != 1 {
		t.Fatalf("got %d episodes, want 1", len(lib.Episodes))
	}
	ep := lib.Episodes[0]
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("PlayState: got %d, want %d (Played should dominate progress)", ep.PlayState, model.PlayStatePlayed)
	}
}

func TestOvercastOPMLReader_ParsesPubDate(t *testing.T) {
	path := writeOvercastOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a">
      <outline type="podcast" title="Episode One" url="https://media.example.com/ep1.mp3"
               overcastId="oc-1" played="1" progress="0"
               pubDate="Mon, 15 Jan 2024 12:00:00 +0000"/>
    </outline>
  </body>
</opml>`)

	lib, err := overcast.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	ep := lib.Episodes[0]
	want, _ := time.Parse(time.RFC1123, "Mon, 15 Jan 2024 12:00:00 +0000")
	if !ep.PubDate.Equal(want) {
		t.Errorf("PubDate: got %v, want %v", ep.PubDate, want)
	}
}

func TestOvercastOPMLReader_MalformedPubDateProducesZeroTime(t *testing.T) {
	path := writeOvercastOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a">
      <outline type="podcast" title="Episode One" overcastId="oc-1"
               played="1" progress="0" pubDate="not-a-date"/>
    </outline>
  </body>
</opml>`)

	lib, err := overcast.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !lib.Episodes[0].PubDate.IsZero() {
		t.Errorf("malformed pubDate should produce zero time, got %v", lib.Episodes[0].PubDate)
	}
}

func TestOvercastOPMLReader_SkipsEpisodesWithEmptyTitle(t *testing.T) {
	path := writeOvercastOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a">
      <outline type="podcast" title="" overcastId="oc-1" played="1" progress="0"/>
      <outline type="podcast" title="Good Episode" overcastId="oc-2" played="1" progress="0"/>
    </outline>
  </body>
</opml>`)

	lib, err := overcast.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lib.Episodes) != 1 {
		t.Errorf("got %d episodes, want 1 (empty-title episode should be skipped)", len(lib.Episodes))
	}
}

func TestOvercastOPMLReader_SourceProvider(t *testing.T) {
	path := writeOvercastOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0"><head><title>x</title></head><body></body></opml>`)

	lib, err := overcast.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if lib.SourceProvider != "Overcast (OPML)" {
		t.Errorf("SourceProvider: got %q, want %q", lib.SourceProvider, "Overcast (OPML)")
	}
}

func TestOvercastOPMLReader_FileNotFound(t *testing.T) {
	_, err := overcast.NewOPMLReader("/nonexistent/overcast.opml").Read(context.Background())
	if err == nil {
		t.Error("expected error for nonexistent file, got nil")
	}
}

// --- OPMLWriter ---

func TestOPMLWriter_WritesValidXML(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/show-a", Title: "Show A"},
			{FeedURL: "https://feeds.example.com/show-b", Title: "Show B"},
		},
	}
	path := filepath.Join(t.TempDir(), "out.opml")
	if err := (&overcast.OPMLWriter{}).Write(lib, path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	// Must parse as valid XML.
	var doc struct {
		XMLName xml.Name `xml:"opml"`
		Body    struct {
			Outlines []struct {
				Text   string `xml:"text,attr"`
				Type   string `xml:"type,attr"`
				XMLURL string `xml:"xmlUrl,attr"`
			} `xml:"outline"`
		} `xml:"body"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("output is not valid XML: %v\n%s", err, data)
	}
	if len(doc.Body.Outlines) != 2 {
		t.Errorf("got %d outlines, want 2", len(doc.Body.Outlines))
	}
}

func TestOPMLWriter_OutlineAttributes(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/show-a", Title: "Show A"},
		},
	}
	path := filepath.Join(t.TempDir(), "out.opml")
	if err := (&overcast.OPMLWriter{}).Write(lib, path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, _ := os.ReadFile(path)
	var doc struct {
		XMLName xml.Name `xml:"opml"`
		Body    struct {
			Outlines []struct {
				Text   string `xml:"text,attr"`
				Type   string `xml:"type,attr"`
				XMLURL string `xml:"xmlUrl,attr"`
			} `xml:"outline"`
		} `xml:"body"`
	}
	_ = xml.Unmarshal(data, &doc)

	o := doc.Body.Outlines[0]
	if o.Text != "Show A" {
		t.Errorf("text attr: got %q, want %q", o.Text, "Show A")
	}
	if o.Type != "rss" {
		t.Errorf("type attr: got %q, want %q", o.Type, "rss")
	}
	if o.XMLURL != "https://feeds.example.com/show-a" {
		t.Errorf("xmlUrl attr: got %q", o.XMLURL)
	}
}

func TestOPMLWriter_IncludesXMLHeader(t *testing.T) {
	lib := &model.Library{Podcasts: []model.Podcast{{FeedURL: "https://example.com/feed", Title: "X"}}}
	path := filepath.Join(t.TempDir(), "out.opml")
	if err := (&overcast.OPMLWriter{}).Write(lib, path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data[:5]) != "<?xml" {
		t.Errorf("output should start with XML declaration, got: %q", string(data[:20]))
	}
}

func TestOPMLWriter_NoEpisodeElements(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/show-a", Title: "Show A"}},
		Episodes: []model.EpisodeState{
			{GUID: "ep1", Title: "Episode One", PlayState: model.PlayStatePlayed},
		},
	}
	path := filepath.Join(t.TempDir(), "out.opml")
	if err := (&overcast.OPMLWriter{}).Write(lib, path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, _ := os.ReadFile(path)
	var doc struct {
		Body struct {
			Outlines []struct {
				Episodes []struct{} `xml:"outline"`
			} `xml:"outline"`
		} `xml:"body"`
	}
	_ = xml.Unmarshal(data, &doc)
	if len(doc.Body.Outlines) > 0 && len(doc.Body.Outlines[0].Episodes) > 0 {
		t.Error("writer should not emit episode outlines — subscriptions only")
	}
}

func TestOPMLWriter_EmptyLibrary(t *testing.T) {
	lib := &model.Library{}
	path := filepath.Join(t.TempDir(), "out.opml")
	if err := (&overcast.OPMLWriter{}).Write(lib, path); err != nil {
		t.Fatalf("Write with empty library: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("output file should exist even for empty library: %v", err)
	}
}

func TestOPMLWriter_BadPath(t *testing.T) {
	lib := &model.Library{Podcasts: []model.Podcast{{FeedURL: "https://example.com/feed"}}}
	err := (&overcast.OPMLWriter{}).Write(lib, "/nonexistent/dir/out.opml")
	if err == nil {
		t.Error("expected error writing to nonexistent directory, got nil")
	}
}
