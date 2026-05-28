package apple_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyler/podcast-migrate/internal/apple"
)

func writeOPML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "subs.opml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write OPML: %v", err)
	}
	return path
}

func TestOPMLReader_ParsesFlatSubscriptions(t *testing.T) {
	path := writeOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a" htmlUrl="https://show-a.example.com"/>
    <outline text="Show B" type="rss" xmlUrl="https://feeds.example.com/show-b"/>
  </body>
</opml>`)

	lib, err := apple.NewOPMLReader(path).Read(context.Background())
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

func TestOPMLReader_ParsesNestedGroupOutlines(t *testing.T) {
	// Apple Podcasts wraps feeds in folder outlines when exported.
	path := writeOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="News">
      <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a"/>
      <outline text="Show B" type="rss" xmlUrl="https://feeds.example.com/show-b"/>
    </outline>
    <outline text="Show C" type="rss" xmlUrl="https://feeds.example.com/show-c"/>
  </body>
</opml>`)

	lib, err := apple.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lib.Podcasts) != 3 {
		t.Fatalf("got %d podcasts, want 3 (2 nested + 1 top-level)", len(lib.Podcasts))
	}
}

func TestOPMLReader_SkipsOutlinesWithoutFeedURL(t *testing.T) {
	path := writeOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Group with no xmlUrl">
      <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a"/>
    </outline>
    <outline text="Orphan outline" type="rss"/>
  </body>
</opml>`)

	lib, err := apple.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Only Show A should be collected; the group outline and the orphan have no xmlUrl.
	if len(lib.Podcasts) != 1 {
		t.Errorf("got %d podcasts, want 1 (only the nested feed with xmlUrl)", len(lib.Podcasts))
	}
}

func TestOPMLReader_NoEpisodesReturned(t *testing.T) {
	path := writeOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a"/>
  </body>
</opml>`)

	lib, err := apple.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lib.Episodes) != 0 {
		t.Errorf("Apple OPML reader should return no episodes, got %d", len(lib.Episodes))
	}
}

func TestOPMLReader_SourceProvider(t *testing.T) {
	path := writeOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0"><body></body></opml>`)

	lib, err := apple.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if lib.SourceProvider != "Apple Podcasts (OPML)" {
		t.Errorf("SourceProvider: got %q, want %q", lib.SourceProvider, "Apple Podcasts (OPML)")
	}
}

func TestOPMLReader_EmptyBody(t *testing.T) {
	path := writeOPML(t, `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0"><body></body></opml>`)

	lib, err := apple.NewOPMLReader(path).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lib.Podcasts) != 0 {
		t.Errorf("got %d podcasts from empty body, want 0", len(lib.Podcasts))
	}
}

func TestOPMLReader_FileNotFound(t *testing.T) {
	_, err := apple.NewOPMLReader("/nonexistent/path/subs.opml").Read(context.Background())
	if err == nil {
		t.Error("expected error for nonexistent file, got nil")
	}
}

func TestOPMLReader_InvalidXML(t *testing.T) {
	path := writeOPML(t, `this is not xml`)
	_, err := apple.NewOPMLReader(path).Read(context.Background())
	if err == nil {
		t.Error("expected error for invalid XML, got nil")
	}
}
