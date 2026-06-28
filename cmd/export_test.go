package cmd

// Tests for the export command using an OPML source so no live credentials
// are needed. The OPML provider implements GetLibrary entirely from a local
// file, making this fully offline.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyler/podcast-migrate/internal/model"
)

// minimalOPML returns a valid OPML 2.0 subscription list with one podcast.
func minimalOPML(t *testing.T) string {
	t.Helper()
	return `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>Test Subscriptions</title></head>
  <body>
    <outline type="rss"
             text="Test Show"
             title="Test Show"
             xmlUrl="https://feeds.example.com/testshow"
             htmlUrl="https://example.com/testshow"/>
  </body>
</opml>`
}

func TestExportCmd_OPML_WritesJSONFile(t *testing.T) {
	dir := t.TempDir()
	opmlPath := filepath.Join(dir, "input.opml")
	outPath := filepath.Join(dir, "output.json")

	if err := os.WriteFile(opmlPath, []byte(minimalOPML(t)), 0644); err != nil {
		t.Fatalf("write OPML: %v", err)
	}

	cmd := exportCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--from", "opml",
		"--opml-file", opmlPath,
		"--out", outPath,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}

	var lib model.Library
	if err := json.Unmarshal(data, &lib); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if len(lib.Podcasts) != 1 {
		t.Fatalf("expected 1 podcast in output, got %d", len(lib.Podcasts))
	}
	if lib.Podcasts[0].Title != "Test Show" {
		t.Errorf("podcast title = %q, want Test Show", lib.Podcasts[0].Title)
	}
	if lib.Podcasts[0].FeedURL != "https://feeds.example.com/testshow" {
		t.Errorf("feed URL = %q, want https://feeds.example.com/testshow", lib.Podcasts[0].FeedURL)
	}
}

func TestExportCmd_OPML_WritesToStdout(t *testing.T) {
	dir := t.TempDir()
	opmlPath := filepath.Join(dir, "input.opml")

	if err := os.WriteFile(opmlPath, []byte(minimalOPML(t)), 0644); err != nil {
		t.Fatalf("write OPML: %v", err)
	}

	cmd := exportCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--from", "opml",
		"--opml-file", opmlPath,
		"--out", "-",
	})

	var out string
	var execErr error
	out = captureStdout(func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("export: %v", execErr)
	}

	if !strings.Contains(out, "Test Show") {
		t.Errorf("stdout does not contain podcast title; got:\n%s", out)
	}
	if !strings.Contains(out, "feeds.example.com/testshow") {
		t.Errorf("stdout does not contain feed URL; got:\n%s", out)
	}
}

func TestExportCmd_MissingRequiredFrom_ReturnsError(t *testing.T) {
	cmd := exportCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--out", "-"}) // missing --from
	if err := cmd.Execute(); err == nil {
		t.Error("expected error for missing --from flag, got nil")
	}
}

func TestExportCmd_UnknownProvider_ReturnsError(t *testing.T) {
	cmd := exportCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--from", "nonexistent-provider", "--out", "-"})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error for unknown provider, got nil")
	}
}

func TestExportCmd_MissingOPMLFile_ReturnsError(t *testing.T) {
	cmd := exportCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--from", "opml",
		"--opml-file", "/nonexistent/path/to/file.opml",
		"--out", "-",
	})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error for missing OPML file, got nil")
	}
}
