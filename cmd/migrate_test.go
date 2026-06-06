package cmd

// Tests for the utility functions in migrate.go:
//   - parseConflictStrategy
//   - buildPodcastFilter
//   - buildProvider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/overcast"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// ---- parseConflictStrategy ----

func TestParseConflictStrategy_Source(t *testing.T) {
	if got := parseConflictStrategy("source"); got != provider.SourceWins {
		t.Errorf("parseConflictStrategy(%q) = %v, want SourceWins", "source", got)
	}
}

func TestParseConflictStrategy_Target(t *testing.T) {
	if got := parseConflictStrategy("target"); got != provider.TargetWins {
		t.Errorf("parseConflictStrategy(%q) = %v, want TargetWins", "target", got)
	}
}

func TestParseConflictStrategy_Furthest_Explicit(t *testing.T) {
	if got := parseConflictStrategy("furthest"); got != provider.FurthestWins {
		t.Errorf("parseConflictStrategy(%q) = %v, want FurthestWins", "furthest", got)
	}
}

func TestParseConflictStrategy_Empty_DefaultsToFurthest(t *testing.T) {
	if got := parseConflictStrategy(""); got != provider.FurthestWins {
		t.Errorf("parseConflictStrategy(%q) = %v, want FurthestWins (default)", "", got)
	}
}

func TestParseConflictStrategy_Unknown_DefaultsToFurthest(t *testing.T) {
	for _, s := range []string{"invalid", "SOURCE", "FURTHEST", "bogus"} {
		if got := parseConflictStrategy(s); got != provider.FurthestWins {
			t.Errorf("parseConflictStrategy(%q) = %v, want FurthestWins (fallback)", s, got)
		}
	}
}

// ---- buildPodcastFilter ----

func TestBuildPodcastFilter_NilInput_ReturnsNil(t *testing.T) {
	got, err := buildPodcastFilter(nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty inputs: got %v, want nil/empty", got)
	}
}

func TestBuildPodcastFilter_CLIPatterns_Lowercased(t *testing.T) {
	got, err := buildPodcastFilter([]string{"Foo", "BAR", "baz"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 entries", got)
	}
	for _, p := range got {
		if p != strings.ToLower(p) {
			t.Errorf("pattern not lowercased: %q", p)
		}
	}
}

func TestBuildPodcastFilter_CLIPatterns_Deduplicated(t *testing.T) {
	got, err := buildPodcastFilter([]string{"Foo", "foo", "FOO", "bar"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "foo" appears three times (case-insensitive) → deduplicated to 1; "bar" → 1
	if len(got) != 2 {
		t.Errorf("got %v (len %d), want 2 deduplicated entries", got, len(got))
	}
}

func TestBuildPodcastFilter_CLIPatterns_TrimsWhitespace(t *testing.T) {
	got, err := buildPodcastFilter([]string{"  hello  ", "world"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 entries", got)
	}
	if got[0] != "hello" {
		t.Errorf("pattern[0]: got %q, want %q", got[0], "hello")
	}
}

func TestBuildPodcastFilter_BlankPatterns_Skipped(t *testing.T) {
	got, err := buildPodcastFilter([]string{"", "   ", "real"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "real" {
		t.Errorf("blank patterns should be skipped: got %v", got)
	}
}

func TestBuildPodcastFilter_ListFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "patterns.txt")
	if err := os.WriteFile(f, []byte("Alpha\nbeta\n\nGamma\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := buildPodcastFilter(nil, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %v (len %d), want 3 entries (blank line skipped)", got, len(got))
	}
	// Patterns should be lowercased.
	want := []string{"alpha", "beta", "gamma"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestBuildPodcastFilter_ListFile_MergesWithCLI(t *testing.T) {
	f := filepath.Join(t.TempDir(), "patterns.txt")
	if err := os.WriteFile(f, []byte("fileonly\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := buildPodcastFilter([]string{"clionly"}, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 entries (CLI + file)", got)
	}
}

func TestBuildPodcastFilter_ListFile_DeduplicatesAcrossSources(t *testing.T) {
	f := filepath.Join(t.TempDir(), "patterns.txt")
	if err := os.WriteFile(f, []byte("shared\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := buildPodcastFilter([]string{"shared"}, f) // "shared" in both CLI and file
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("duplicate across sources should deduplicate: got %v", got)
	}
}

func TestBuildPodcastFilter_ListFile_NotFound_ReturnsError(t *testing.T) {
	_, err := buildPodcastFilter(nil, "/nonexistent/path/to/file.txt")
	if err == nil {
		t.Error("expected error for non-existent list file, got nil")
	}
}

// ---- buildProvider ----

func TestBuildProvider_Apple_Podcasts(t *testing.T) {
	p, err := buildProvider("podcasts", "", "", "", "", "", "")
	if err != nil {
		t.Fatalf("buildProvider(podcasts): %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if p.Name() != "Apple Podcasts" {
		t.Errorf("Name: got %q, want %q", p.Name(), "Apple Podcasts")
	}
}

func TestBuildProvider_Apple_Alias(t *testing.T) {
	p, err := buildProvider("apple", "", "", "", "", "", "")
	if err != nil {
		t.Fatalf("buildProvider(apple): %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

func TestBuildProvider_Overcast_NoPaths_Error(t *testing.T) {
	// No source OPML, no export path, no credentials → error.
	_, err := buildProvider("overcast", "", "", "", "", "", "")
	if err == nil {
		t.Error("expected error for overcast with no configuration, got nil")
	}
}

func TestBuildProvider_Overcast_WithSourceOPML(t *testing.T) {
	p, err := buildProvider("overcast", "", "", "source.opml", "", "", "")
	if err != nil {
		t.Fatalf("buildProvider(overcast, source.opml): %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if p.Name() != "Overcast" {
		t.Errorf("Name: got %q, want %q", p.Name(), "Overcast")
	}
}

func TestBuildProvider_Overcast_WithExportPath(t *testing.T) {
	p, err := buildProvider("overcast", "", "", "", "out.opml", "", "")
	if err != nil {
		t.Fatalf("buildProvider(overcast, export): %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	caps := p.Capabilities()
	if !caps.WriteSubscriptions {
		t.Error("WriteSubscriptions should be true when exportOPMLPath is set")
	}
}

func TestBuildProvider_Overcast_WithCredentials_ReturnsWriteCapable(t *testing.T) {
	p, err := buildProvider("overcast", "", "", "", "", "user@example.com", "secret")
	if err != nil {
		t.Fatalf("buildProvider(overcast, credentials): %v", err)
	}
	if _, ok := p.(*overcast.Provider); !ok {
		t.Errorf("expected *overcast.Provider, got %T", p)
	}
	if !p.Capabilities().WritePlayState {
		t.Error("WritePlayState should be true when credentials are set")
	}
}

func TestBuildProvider_Unknown_ReturnsError(t *testing.T) {
	for _, name := range []string{"spotify", "pocketcasts", "", "OVERCAST"} {
		_, err := buildProvider(name, "", "", "", "", "", "")
		if err == nil {
			t.Errorf("buildProvider(%q): expected error for unknown provider, got nil", name)
		}
	}
}

// ---- parseSince ----

func TestParseSince_DaySuffix(t *testing.T) {
	before := time.Now().Add(-7 * 24 * time.Hour)
	got, err := parseSince("7d")
	if err != nil {
		t.Fatalf("parseSince(7d): %v", err)
	}
	after := time.Now().Add(-7 * 24 * time.Hour)
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Errorf("parseSince(7d) = %v, expected ~%v", got, before)
	}
}

func TestParseSince_GoDuration(t *testing.T) {
	before := time.Now().Add(-24 * time.Hour)
	got, err := parseSince("24h")
	if err != nil {
		t.Fatalf("parseSince(24h): %v", err)
	}
	after := time.Now().Add(-24 * time.Hour)
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Errorf("parseSince(24h) = %v, expected ~%v", got, before)
	}
}

func TestParseSince_DateOnly(t *testing.T) {
	got, err := parseSince("2026-06-01")
	if err != nil {
		t.Fatalf("parseSince(2026-06-01): %v", err)
	}
	want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("parseSince(2026-06-01) = %v, want %v", got, want)
	}
}

func TestParseSince_DateTimeNoZone(t *testing.T) {
	got, err := parseSince("2026-06-01T12:30")
	if err != nil {
		t.Fatalf("parseSince(2026-06-01T12:30): %v", err)
	}
	want := time.Date(2026, 6, 1, 12, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("parseSince(2026-06-01T12:30) = %v, want %v", got, want)
	}
}

func TestParseSince_RFC3339(t *testing.T) {
	got, err := parseSince("2026-06-01T12:00:00Z")
	if err != nil {
		t.Fatalf("parseSince RFC3339: %v", err)
	}
	want := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseSince RFC3339 = %v, want %v", got, want)
	}
}

func TestParseSince_InvalidReturnsError(t *testing.T) {
	for _, s := range []string{"", "yesterday", "2026/06/01", "-1d", "0h"} {
		if _, err := parseSince(s); err == nil {
			t.Errorf("parseSince(%q) expected error, got nil", s)
		}
	}
}
