package opml_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/opml"
	"github.com/tyler/podcast-migrate/internal/provider"
)

const minimalOPML = `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>Test</title></head>
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a"/>
  </body>
</opml>`

func writeTestOPML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.opml")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write OPML: %v", err)
	}
	return p
}

// ---- Name ----

func TestOPMLProvider_Name(t *testing.T) {
	p := opml.NewSourceProvider("dummy")
	if p.Name() != "OPML" {
		t.Errorf("Name: got %q, want %q", p.Name(), "OPML")
	}
}

// ---- Capabilities ----

func TestOPMLProvider_Capabilities_SourceOnly(t *testing.T) {
	p := opml.NewSourceProvider("/some/path")
	caps := p.Capabilities()
	if !caps.ReadSubscriptions {
		t.Error("ReadSubscriptions should be true for source provider")
	}
	if !caps.ReadPlayState {
		t.Error("ReadPlayState should be true for source provider")
	}
	if caps.WriteSubscriptions {
		t.Error("WriteSubscriptions should be false for source-only provider")
	}
	if caps.WritePlayState {
		t.Error("WritePlayState should be false for source-only provider")
	}
}

func TestOPMLProvider_Capabilities_OutputOnly_NoExtended(t *testing.T) {
	p := opml.NewOutputProvider("/some/out.opml", false)
	caps := p.Capabilities()
	if caps.ReadSubscriptions || caps.ReadPlayState {
		t.Error("read capabilities should be false for output-only provider")
	}
	if !caps.WriteSubscriptions {
		t.Error("WriteSubscriptions should be true when outputPath is set")
	}
	if caps.WritePlayState {
		t.Error("WritePlayState should be false when extended=false")
	}
}

func TestOPMLProvider_Capabilities_OutputOnly_Extended(t *testing.T) {
	p := opml.NewOutputProvider("/some/out.opml", true)
	caps := p.Capabilities()
	if !caps.WriteSubscriptions {
		t.Error("WriteSubscriptions should be true")
	}
	if !caps.WritePlayState {
		t.Error("WritePlayState should be true when extended=true")
	}
}

func TestOPMLProvider_Capabilities_NoPathsSet(t *testing.T) {
	// An output provider with no path has no write capabilities.
	p := opml.NewOutputProvider("", false)
	caps := p.Capabilities()
	if caps.ReadSubscriptions || caps.ReadPlayState || caps.WriteSubscriptions || caps.WritePlayState {
		t.Error("all capabilities should be false when no paths are set")
	}
}

// ---- GetLibrary ----

func TestOPMLProvider_GetLibrary_ReadsFile(t *testing.T) {
	path := writeTestOPML(t, minimalOPML)
	p := opml.NewSourceProvider(path)

	lib, err := p.GetLibrary(context.Background())
	if err != nil {
		t.Fatalf("GetLibrary: %v", err)
	}
	if len(lib.Podcasts) != 1 {
		t.Errorf("got %d podcasts, want 1", len(lib.Podcasts))
	}
	if lib.Podcasts[0].FeedURL != "https://feeds.example.com/show-a" {
		t.Errorf("FeedURL: got %q", lib.Podcasts[0].FeedURL)
	}
}

func TestOPMLProvider_GetLibrary_NoPath_ReturnsError(t *testing.T) {
	// Output-only provider has no source path.
	p := opml.NewOutputProvider("/out.opml", false)
	_, err := p.GetLibrary(context.Background())
	if err == nil {
		t.Error("expected error when no source path configured")
	}
}

// ---- SetLibrary ----

func TestOPMLProvider_SetLibrary_NoPath_ReturnsError(t *testing.T) {
	p := opml.NewSourceProvider("/src.opml") // has source but no output path
	err := p.SetLibrary(context.Background(), &model.Library{}, provider.WriteOptions{})
	if err == nil {
		t.Error("expected error when no output path configured")
	}
}

func TestOPMLProvider_SetLibrary_WritesSubscriptions(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.opml")
	p := opml.NewOutputProvider(outPath, false)
	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/x", Title: "X Show"}},
	}
	if err := p.SetLibrary(context.Background(), lib, provider.WriteOptions{}); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(data), "X Show") {
		t.Error("output OPML should contain podcast title")
	}
	if !strings.Contains(string(data), "https://feeds.example.com/x") {
		t.Error("output OPML should contain feed URL")
	}
}

func TestOPMLProvider_SetLibrary_DryRun_DoesNotWrite(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.opml")
	p := opml.NewOutputProvider(outPath, false)
	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/x", Title: "X Show"}},
	}
	if err := p.SetLibrary(context.Background(), lib, provider.WriteOptions{DryRun: true}); err != nil {
		t.Fatalf("SetLibrary dry-run: %v", err)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Error("dry-run should not write a file")
	}
}

func TestOPMLProvider_SetLibrary_Extended_IncludesEpisodes(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.opml")
	p := opml.NewOutputProvider(outPath, true)

	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/x", Title: "X Show"}},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/x",
				Title:     "Great Episode",
				PubDate:   time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
				PlayState: model.PlayStatePlayed,
			},
		},
	}
	if err := p.SetLibrary(context.Background(), lib, provider.WriteOptions{}); err != nil {
		t.Fatalf("SetLibrary extended: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(data), "Great Episode") {
		t.Error("extended output OPML should contain episode title")
	}
}

func TestOPMLProvider_SetLibrary_Extended_OnlySubscriptions_SkipsEpisodes(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.opml")
	p := opml.NewOutputProvider(outPath, true)
	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/x", Title: "X Show"}},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   "https://feeds.example.com/x",
				Title:     "Ep One",
				PubDate:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				PlayState: model.PlayStatePlayed,
			},
		},
	}
	if err := p.SetLibrary(context.Background(), lib, provider.WriteOptions{OnlySubscriptions: true}); err != nil {
		t.Fatalf("SetLibrary OnlySubscriptions: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	content := string(data)
	// Podcast should be written; episode outlines should not appear.
	if !strings.Contains(content, "X Show") {
		t.Error("OnlySubscriptions: podcast should still be written")
	}
	if strings.Contains(content, "Ep One") {
		t.Error("OnlySubscriptions: episode title must not appear in subscriptions-only output")
	}
}

func TestOPMLProvider_SetLibrary_Extended_DryRun_DoesNotWrite(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.opml")
	p := opml.NewOutputProvider(outPath, true)
	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/x", Title: "X Show"}},
		Episodes: []model.EpisodeState{
			{FeedURL: "https://feeds.example.com/x", Title: "Ep", PlayState: model.PlayStatePlayed},
		},
	}
	if err := p.SetLibrary(context.Background(), lib, provider.WriteOptions{DryRun: true}); err != nil {
		t.Fatalf("SetLibrary extended dry-run: %v", err)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Error("dry-run should not write a file")
	}
}
