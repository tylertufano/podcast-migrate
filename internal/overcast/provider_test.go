package overcast_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/overcast"
	"github.com/tyler/podcast-migrate/internal/provider"
)

func writeMinimalOvercastOPML(t *testing.T) string {
	t.Helper()
	content := `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>Podcast Subscriptions</title></head>
  <body>
    <outline text="Show A" type="rss" xmlUrl="https://feeds.example.com/show-a"/>
  </body>
</opml>`
	path := filepath.Join(t.TempDir(), "overcast.opml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write OPML: %v", err)
	}
	return path
}

func TestOvercastProvider_Name(t *testing.T) {
	p := overcast.NewProvider("", "")
	if p.Name() != "Overcast" {
		t.Errorf("Name: got %q, want %q", p.Name(), "Overcast")
	}
}

func TestOvercastProvider_Capabilities_BothPaths(t *testing.T) {
	p := overcast.NewProvider("import.opml", "export.opml")
	caps := p.Capabilities()
	if !caps.ReadSubscriptions {
		t.Error("ReadSubscriptions should be true when sourceOPMLPath is set")
	}
	if !caps.ReadPlayState {
		t.Error("ReadPlayState should be true when sourceOPMLPath is set")
	}
	if !caps.WriteSubscriptions {
		t.Error("WriteSubscriptions should be true when exportOPMLPath is set")
	}
	// WritePlayState requires credentials, not an OPML path.
	if caps.WritePlayState {
		t.Error("WritePlayState should be false without credentials")
	}
}

func TestOvercastProvider_Capabilities_NoPaths(t *testing.T) {
	p := overcast.NewProvider("", "")
	caps := p.Capabilities()
	if caps.ReadSubscriptions || caps.ReadPlayState {
		t.Error("read capabilities should be false when no importOPMLPath is set")
	}
	if caps.WriteSubscriptions {
		t.Error("WriteSubscriptions should be false when no exportOPMLPath is set")
	}
}

func TestOvercastProvider_GetLibrary_ReadsOPML(t *testing.T) {
	importPath := writeMinimalOvercastOPML(t)
	p := overcast.NewProvider(importPath, "")

	lib, err := p.GetLibrary(context.Background())
	if err != nil {
		t.Fatalf("GetLibrary: %v", err)
	}
	if len(lib.Podcasts) != 1 {
		t.Errorf("got %d podcasts, want 1", len(lib.Podcasts))
	}
}

func TestOvercastProvider_GetLibrary_NoPath_ReturnsUnsupported(t *testing.T) {
	p := overcast.NewProvider("", "")
	_, err := p.GetLibrary(context.Background())
	if err == nil {
		t.Error("expected error when importOPMLPath is empty")
	}
}

func TestOvercastProvider_SetLibrary_WritesOPML(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.opml")
	p := overcast.NewProvider("", outPath)

	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/show-a", Title: "Show A"}},
	}
	if err := p.SetLibrary(context.Background(), lib, provider.WriteOptions{}); err != nil {
		t.Fatalf("SetLibrary: %v", err)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("OPML file should have been written: %v", err)
	}
}

func TestOvercastProvider_SetLibrary_DryRun_DoesNotWrite(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.opml")
	p := overcast.NewProvider("", outPath)

	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/show-a"}},
	}
	if err := p.SetLibrary(context.Background(), lib, provider.WriteOptions{DryRun: true}); err != nil {
		t.Fatalf("SetLibrary dry-run: %v", err)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Error("dry-run should not write a file")
	}
}

func TestOvercastProvider_SetLibrary_NoPath_ReturnsUnsupported(t *testing.T) {
	p := overcast.NewProvider("", "")
	err := p.SetLibrary(context.Background(), &model.Library{}, provider.WriteOptions{})
	if err == nil {
		t.Error("expected error when exportOPMLPath is empty")
	}
}

func TestOvercastProvider_SetLibrary_OnlyPlayState_ReturnsUnsupported(t *testing.T) {
	p := overcast.NewProvider("", "/tmp/out.opml")
	err := p.SetLibrary(context.Background(), &model.Library{}, provider.WriteOptions{OnlyPlayState: true})
	if err == nil {
		t.Error("expected unsupported error for OnlyPlayState when no credentials configured")
	}
}

func TestOvercastProvider_WithCredentials_Capabilities(t *testing.T) {
	importPath := writeMinimalOvercastOPML(t)
	outPath := filepath.Join(t.TempDir(), "out.opml")

	p := overcast.NewProviderWithCredentials(importPath, outPath, "user@example.com", "secret")
	caps := p.Capabilities()

	if !caps.WritePlayState {
		t.Error("WritePlayState should be true when credentials are set")
	}
	if !caps.ReadSubscriptions {
		t.Error("ReadSubscriptions should be true when sourceOPMLPath is set")
	}
	if !caps.WriteSubscriptions {
		t.Error("WriteSubscriptions should be true when exportOPMLPath is set")
	}
}

func TestOvercastProvider_WithCredentials_NoSourceOPML_WritePlayStateTrue(t *testing.T) {
	// Credentials alone are sufficient for WritePlayState — the destination matching
	// OPML is auto-fetched from the live account after login (no sourceOPMLPath needed).
	p := overcast.NewProviderWithCredentials("", "/tmp/out.opml", "user@example.com", "secret")
	caps := p.Capabilities()
	if !caps.WritePlayState {
		t.Error("WritePlayState should be true when credentials are set (match OPML auto-fetched at write time)")
	}
	// ReadSubscriptions and ReadPlayState still require a source OPML.
	if caps.ReadSubscriptions {
		t.Error("ReadSubscriptions should be false when sourceOPMLPath is empty")
	}
	if caps.ReadPlayState {
		t.Error("ReadPlayState should be false when sourceOPMLPath is empty")
	}
}

func TestOvercastProvider_SetLibrary_PlayStateDryRun(t *testing.T) {
	// Dry-run with no episodes in scope: exits before any auth or HTTP calls.
	// Note: in real usage dry-run DOES authenticate and fetch podcast pages via
	// augmentIndexFromPodcastPages (read-only) to give an accurate preview.
	// This test exercises the zero-episode early-exit path only.
	importPath := writeMinimalOvercastOPML(t)
	outPath := filepath.Join(t.TempDir(), "out.opml")
	p := overcast.NewProviderWithCredentials(importPath, outPath, "user@example.com", "secret")
	p.SetMatchOPMLPath(importPath)

	lib := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://feeds.example.com/show-a", Title: "Show A"}},
		// No episodes with play state → early exit, no auth attempted.
	}
	if err := p.SetLibrary(context.Background(), lib, provider.WriteOptions{DryRun: true}); err != nil {
		t.Fatalf("SetLibrary dry-run with credentials: %v", err)
	}
}
