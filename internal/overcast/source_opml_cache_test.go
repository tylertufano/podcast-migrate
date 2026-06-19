package overcast

// White-box tests for source_opml_cache.go.
// Uses setSourceOPMLCachePathForTest to redirect to a temp dir.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalSourceOPML = `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>Overcast Subscriptions</title></head>
  <body>
    <outline text="feeds">
      <outline text="Fresh Air" type="rss" xmlUrl="https://feeds.npr.org/381444908/podcast.xml" overcastId="42">
        <outline type="podcast-episode" title="A Great Episode" overcastId="9001"
          pubDate="2024-06-01T12:00:00Z" played="1"/>
      </outline>
    </outline>
  </body>
</opml>`

func setCachePathForSourceTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "overcast-source.opml")
	setSourceOPMLCachePathForTest(path)
	t.Cleanup(func() { setSourceOPMLCachePathForTest("") })
	return path
}

// ---- sourceOPMLCachePath ----

func TestSourceOPMLCachePath_DefaultSuffix(t *testing.T) {
	// Without an override, the path should contain the expected filename.
	p := sourceOPMLCachePath()
	if !strings.HasSuffix(p, "overcast-source.opml") {
		t.Errorf("sourceOPMLCachePath: expected suffix overcast-source.opml, got %q", p)
	}
}

func TestSourceOPMLCachePath_TestOverride(t *testing.T) {
	custom := setCachePathForSourceTest(t)
	if got := sourceOPMLCachePath(); got != custom {
		t.Errorf("sourceOPMLCachePath: got %q, want %q", got, custom)
	}
}

// ---- loadCachedSourceOPML ----

func TestLoadCachedSourceOPML_ForceRefresh_ReturnsMiss(t *testing.T) {
	setCachePathForSourceTest(t)
	// Even if a valid cache file existed, forceRefresh=true must return miss.
	result, err := loadCachedSourceOPML(context.Background(), true)
	if err != nil {
		t.Fatalf("loadCachedSourceOPML forceRefresh: %v", err)
	}
	if result != nil {
		t.Error("forceRefresh=true should always return nil (cache miss)")
	}
}

func TestLoadCachedSourceOPML_NoCacheFile_ReturnsMiss(t *testing.T) {
	setCachePathForSourceTest(t)
	// Cache file does not exist — should be a miss, not an error.
	result, err := loadCachedSourceOPML(context.Background(), false)
	if err != nil {
		t.Fatalf("loadCachedSourceOPML missing file: %v", err)
	}
	if result != nil {
		t.Error("missing cache file should return nil (cache miss)")
	}
}

func TestLoadCachedSourceOPML_ValidCache_ReturnsLibrary(t *testing.T) {
	path := setCachePathForSourceTest(t)
	// Write a fresh cache file.
	if err := os.WriteFile(path, []byte(minimalSourceOPML), 0644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	result, err := loadCachedSourceOPML(context.Background(), false)
	if err != nil {
		t.Fatalf("loadCachedSourceOPML valid cache: %v", err)
	}
	if result == nil {
		t.Fatal("expected cache hit, got miss")
	}
	if len(result.lib.Podcasts) != 1 {
		t.Errorf("podcasts: got %d, want 1", len(result.lib.Podcasts))
	}
	if result.lib.Podcasts[0].Title != "Fresh Air" {
		t.Errorf("podcast title: got %q, want %q", result.lib.Podcasts[0].Title, "Fresh Air")
	}
}

func TestLoadCachedSourceOPML_StaleCache_ReturnsMiss(t *testing.T) {
	path := setCachePathForSourceTest(t)
	if err := os.WriteFile(path, []byte(minimalSourceOPML), 0644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	// Back-date the file modification time to exceed the 24 h TTL.
	staleTime := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	result, err := loadCachedSourceOPML(context.Background(), false)
	if err != nil {
		t.Fatalf("loadCachedSourceOPML stale: %v", err)
	}
	if result != nil {
		t.Error("stale cache (>24h) should return nil (cache miss)")
	}
}

func TestLoadCachedSourceOPML_CorruptCache_ReturnsMiss(t *testing.T) {
	path := setCachePathForSourceTest(t)
	if err := os.WriteFile(path, []byte("this is not valid opml xml!@#$"), 0644); err != nil {
		t.Fatalf("write corrupt cache: %v", err)
	}

	// Corrupt file should be treated as a miss, not an error.
	result, err := loadCachedSourceOPML(context.Background(), false)
	if err != nil {
		t.Fatalf("loadCachedSourceOPML corrupt: %v", err)
	}
	if result != nil {
		t.Error("corrupt cache file should return nil (cache miss)")
	}
}

func TestLoadCachedSourceOPML_ValidCache_ReportsAge(t *testing.T) {
	path := setCachePathForSourceTest(t)
	if err := os.WriteFile(path, []byte(minimalSourceOPML), 0644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	// Set mtime 1 hour ago.
	ageTarget := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(path, ageTarget, ageTarget); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	result, err := loadCachedSourceOPML(context.Background(), false)
	if err != nil {
		t.Fatalf("loadCachedSourceOPML: %v", err)
	}
	if result == nil {
		t.Fatal("expected cache hit for 1h-old file (TTL is 24h)")
	}
	// Age should be roughly 1 hour.
	if result.age < 50*time.Minute || result.age > 70*time.Minute {
		t.Errorf("age: got %v, want ~1h", result.age)
	}
}

// ---- saveRawSourceOPML ----

func TestSaveRawSourceOPML_WritesFile(t *testing.T) {
	path := setCachePathForSourceTest(t)
	data := []byte(minimalSourceOPML)
	if err := saveRawSourceOPML(data); err != nil {
		t.Fatalf("saveRawSourceOPML: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("saved content mismatch: got %q, want %q", got, data)
	}
}

func TestSaveRawSourceOPML_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	// Put the cache file in a sub-sub-directory that doesn't yet exist.
	path := filepath.Join(dir, "sub1", "sub2", "overcast-source.opml")
	setSourceOPMLCachePathForTest(path)
	t.Cleanup(func() { setSourceOPMLCachePathForTest("") })

	if err := saveRawSourceOPML([]byte(minimalSourceOPML)); err != nil {
		t.Fatalf("saveRawSourceOPML: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("saved file should exist: %v", err)
	}
}

// ---- round-trip: save → load ----

func TestSourceOPMLCache_RoundTrip(t *testing.T) {
	setCachePathForSourceTest(t)
	data := []byte(minimalSourceOPML)
	if err := saveRawSourceOPML(data); err != nil {
		t.Fatalf("saveRawSourceOPML: %v", err)
	}
	result, err := loadCachedSourceOPML(context.Background(), false)
	if err != nil {
		t.Fatalf("loadCachedSourceOPML: %v", err)
	}
	if result == nil {
		t.Fatal("expected cache hit after save")
	}
	if len(result.lib.Podcasts) != 1 {
		t.Errorf("round-trip: got %d podcasts, want 1", len(result.lib.Podcasts))
	}
}
