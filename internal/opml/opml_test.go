package opml_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/opml"
)

// ---- Parse (reader) ----

func TestParse_StandardOPML_SubscriptionsOnly(t *testing.T) {
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>My Podcasts</title></head>
  <body>
    <outline type="rss" text="Alpha Show" xmlUrl="https://feeds.example.com/alpha"/>
    <outline type="rss" text="Beta Show" xmlUrl="https://feeds.example.com/beta" htmlUrl="https://beta.example.com"/>
  </body>
</opml>`)

	lib, err := opml.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(lib.Podcasts) != 2 {
		t.Fatalf("podcasts: got %d, want 2", len(lib.Podcasts))
	}
	if lib.Podcasts[0].FeedURL != "https://feeds.example.com/alpha" {
		t.Errorf("podcast[0].FeedURL = %q", lib.Podcasts[0].FeedURL)
	}
	if lib.Podcasts[0].Title != "Alpha Show" {
		t.Errorf("podcast[0].Title = %q", lib.Podcasts[0].Title)
	}
	if len(lib.Episodes) != 0 {
		t.Errorf("episodes: got %d, want 0", len(lib.Episodes))
	}
	if lib.SourceProvider != "OPML" {
		t.Errorf("SourceProvider = %q, want OPML", lib.SourceProvider)
	}
}

func TestParse_ExtendedOPML_PlayState(t *testing.T) {
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>Podcast Library</title></head>
  <body>
    <outline type="rss" text="Alpha Show" xmlUrl="https://feeds.example.com/alpha">
      <outline type="podcast-episode" title="Ep 1" pubDate="2024-03-15T10:00:00Z" played="1" progress="3600.0"/>
      <outline type="podcast-episode" title="Ep 2" pubDate="2024-04-01T08:00:00Z" played="0" progress="900.5"/>
    </outline>
    <outline type="rss" text="Beta Show" xmlUrl="https://feeds.example.com/beta">
      <outline type="podcast-episode" title="Beta Ep" pubDate="2024-02-10T12:00:00Z" played="1"/>
    </outline>
  </body>
</opml>`)

	lib, err := opml.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(lib.Podcasts) != 2 {
		t.Fatalf("podcasts: got %d, want 2", len(lib.Podcasts))
	}
	if len(lib.Episodes) != 3 {
		t.Fatalf("episodes: got %d, want 3", len(lib.Episodes))
	}
	if lib.SourceProvider != "OPML (extended)" {
		t.Errorf("SourceProvider = %q, want OPML (extended)", lib.SourceProvider)
	}

	ep1 := lib.Episodes[0]
	if ep1.Title != "Ep 1" {
		t.Errorf("ep[0].Title = %q", ep1.Title)
	}
	if ep1.FeedURL != "https://feeds.example.com/alpha" {
		t.Errorf("ep[0].FeedURL = %q", ep1.FeedURL)
	}
	if ep1.PlayState != model.PlayStatePlayed {
		t.Errorf("ep[0].PlayState = %v, want Played", ep1.PlayState)
	}
	if ep1.PlayPosition != 3600*time.Second {
		t.Errorf("ep[0].PlayPosition = %v, want 3600s", ep1.PlayPosition)
	}
	wantDate := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	if !ep1.PubDate.Equal(wantDate) {
		t.Errorf("ep[0].PubDate = %v, want %v", ep1.PubDate, wantDate)
	}

	ep2 := lib.Episodes[1]
	if ep2.PlayState != model.PlayStateInProgress {
		t.Errorf("ep[1].PlayState = %v, want InProgress", ep2.PlayState)
	}
	if ep2.PlayPosition != 900*time.Second+500*time.Millisecond {
		t.Errorf("ep[1].PlayPosition = %v, want 900.5s", ep2.PlayPosition)
	}

	ep3 := lib.Episodes[2]
	if ep3.PlayState != model.PlayStatePlayed {
		t.Errorf("ep[2].PlayState = %v, want Played", ep3.PlayState)
	}
	if ep3.PlayPosition != 0 {
		t.Errorf("ep[2].PlayPosition = %v, want 0 (played with no progress stored)", ep3.PlayPosition)
	}
}

func TestParse_GroupContainer_ExtendedFormat(t *testing.T) {
	// Overcast's extended export wraps feeds in a <outline text="feeds"> container.
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="feeds">
      <outline type="rss" text="Alpha Show" xmlUrl="https://feeds.example.com/alpha">
        <outline type="podcast-episode" title="Ep 1" pubDate="2024-01-01T00:00:00Z" played="1"/>
      </outline>
    </outline>
  </body>
</opml>`)

	lib, err := opml.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(lib.Podcasts) != 1 {
		t.Fatalf("podcasts: got %d, want 1", len(lib.Podcasts))
	}
	if len(lib.Episodes) != 1 {
		t.Fatalf("episodes: got %d, want 1", len(lib.Episodes))
	}
	if lib.Episodes[0].PlayState != model.PlayStatePlayed {
		t.Errorf("ep[0].PlayState = %v, want Played", lib.Episodes[0].PlayState)
	}
}

func TestParse_RFC1123ZPubDate(t *testing.T) {
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline type="rss" text="Show" xmlUrl="https://feeds.example.com/show">
      <outline type="podcast-episode" title="Old Style" pubDate="Mon, 15 Jan 2024 12:00:00 +0000" played="1"/>
    </outline>
  </body>
</opml>`)

	lib, err := opml.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if lib.Episodes[0].PubDate.IsZero() {
		t.Error("PubDate should parse RFC1123Z format")
	}
	if lib.Episodes[0].PubDate.Year() != 2024 {
		t.Errorf("PubDate year = %d, want 2024", lib.Episodes[0].PubDate.Year())
	}
}

func TestParse_SkipsEpisodeWithNoTitle(t *testing.T) {
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline type="rss" text="Show" xmlUrl="https://feeds.example.com/show">
      <outline type="podcast-episode" pubDate="2024-01-01T00:00:00Z" played="1"/>
    </outline>
  </body>
</opml>`)

	lib, err := opml.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(lib.Episodes) != 0 {
		t.Errorf("expected 0 episodes (no title), got %d", len(lib.Episodes))
	}
}

func TestParse_MalformedXML_ReturnsError(t *testing.T) {
	_, err := opml.Parse([]byte("<not valid xml"))
	if err == nil {
		t.Error("expected error for malformed XML")
	}
}

// ---- Writer ----

func TestWriter_StandardOPML(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
			{FeedURL: "https://feeds.example.com/beta", Title: "Beta Show"},
		},
	}

	path := filepath.Join(t.TempDir(), "out.opml")
	w := &opml.Writer{}
	if err := w.Write(lib, path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, _ := os.ReadFile(path)
	xml := string(data)

	if !strings.Contains(xml, `xmlUrl="https://feeds.example.com/alpha"`) {
		t.Error("missing alpha feed URL")
	}
	if !strings.Contains(xml, `text="Beta Show"`) {
		t.Error("missing beta title")
	}
	if strings.Contains(xml, "podcast-episode") {
		t.Error("standard OPML should not contain episode outlines")
	}
	if !strings.HasPrefix(xml, "<?xml") {
		t.Error("missing XML declaration")
	}
}

func TestWriter_ExtendedOPML_IncludesPlayedEpisodes(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/alpha",
				Title:     "Episode One",
				PubDate:   time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC),
				PlayState: model.PlayStatePlayed,
			},
			{
				FeedURL:      "https://feeds.example.com/alpha",
				Title:        "Episode Two",
				PubDate:      time.Date(2024, 4, 1, 8, 0, 0, 0, time.UTC),
				PlayState:    model.PlayStateInProgress,
				PlayPosition: 900 * time.Second,
			},
		},
	}

	path := filepath.Join(t.TempDir(), "extended.opml")
	w := &opml.Writer{Extended: true}
	if err := w.Write(lib, path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, _ := os.ReadFile(path)
	xml := string(data)

	if !strings.Contains(xml, `type="podcast-episode"`) {
		t.Error("missing podcast-episode type")
	}
	if !strings.Contains(xml, `title="Episode One"`) {
		t.Error("missing Episode One")
	}
	if !strings.Contains(xml, `played="1"`) {
		t.Error("missing played=1 for played episode")
	}
	if !strings.Contains(xml, `title="Episode Two"`) {
		t.Error("missing Episode Two")
	}
	if !strings.Contains(xml, `progress="900.0"`) {
		t.Error("missing progress for in-progress episode")
	}
	if !strings.Contains(xml, `pubDate="2024-03-15T10:00:00Z"`) {
		t.Error("missing RFC3339 pubDate")
	}
}

func TestWriter_ExtendedOPML_SkipsUnplayedEpisodes(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/alpha",
				Title:     "Unplayed Episode",
				PlayState: model.PlayStateUnplayed,
			},
		},
	}

	path := filepath.Join(t.TempDir(), "extended.opml")
	w := &opml.Writer{Extended: true}
	if err := w.Write(lib, path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "podcast-episode") {
		t.Error("unplayed episodes should not appear in extended OPML")
	}
}

func TestWriter_ExtendedOPML_SkipsFromDestinationEpisodes(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:         "https://feeds.example.com/alpha",
				Title:           "Destination-only Episode",
				PlayState:       model.PlayStatePlayed,
				FromDestination: true,
			},
		},
	}

	path := filepath.Join(t.TempDir(), "extended.opml")
	w := &opml.Writer{Extended: true}
	if err := w.Write(lib, path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "podcast-episode") {
		t.Error("FromDestination episodes should not appear in extended OPML output")
	}
}

func TestWriter_RoundTrip(t *testing.T) {
	// Write an extended OPML and read it back; verify identity.
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/alpha",
				Title:     "Ep 1",
				PubDate:   time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
				PlayState: model.PlayStatePlayed,
			},
			{
				FeedURL:      "https://feeds.example.com/alpha",
				Title:        "Ep 2",
				PubDate:      time.Date(2024, 6, 5, 8, 0, 0, 0, time.UTC),
				PlayState:    model.PlayStateInProgress,
				PlayPosition: 450 * time.Second,
			},
		},
	}

	path := filepath.Join(t.TempDir(), "roundtrip.opml")
	if err := (&opml.Writer{Extended: true}).Write(lib, path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, _ := os.ReadFile(path)
	got, err := opml.Parse(data)
	if err != nil {
		t.Fatalf("Parse round-trip: %v", err)
	}

	if len(got.Podcasts) != 1 || got.Podcasts[0].FeedURL != lib.Podcasts[0].FeedURL {
		t.Errorf("round-trip podcasts mismatch: %+v", got.Podcasts)
	}
	if len(got.Episodes) != 2 {
		t.Fatalf("round-trip episodes: got %d, want 2", len(got.Episodes))
	}
	if got.Episodes[0].PlayState != model.PlayStatePlayed {
		t.Errorf("round-trip ep[0].PlayState = %v, want Played", got.Episodes[0].PlayState)
	}
	if got.Episodes[1].PlayState != model.PlayStateInProgress {
		t.Errorf("round-trip ep[1].PlayState = %v, want InProgress", got.Episodes[1].PlayState)
	}
	if got.Episodes[1].PlayPosition != 450*time.Second {
		t.Errorf("round-trip ep[1].PlayPosition = %v, want 450s", got.Episodes[1].PlayPosition)
	}
}

func TestWriter_BadPath_ReturnsError(t *testing.T) {
	lib := &model.Library{Podcasts: []model.Podcast{{FeedURL: "x", Title: "y"}}}
	err := (&opml.Writer{}).Write(lib, "/nonexistent/dir/out.opml")
	if err == nil {
		t.Error("expected error writing to nonexistent directory")
	}
}
