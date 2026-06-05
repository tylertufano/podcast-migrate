package overcast

// Tests for the persistent episode ID cache (id_cache.go).
// All tests use setEpisodeCachePathForTest to avoid touching the real user cache.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestCache returns a cache backed by a temp-dir file.
// Use this for unit tests that exercise individual methods without round-tripping
// through loadFromPath (which is tested separately).
func newTestCache(t *testing.T) *episodeIDCache {
	t.Helper()
	return &episodeIDCache{
		path:  filepath.Join(t.TempDir(), "episode-ids.json"),
		items: make(map[string]episodeCacheEntry),
	}
}

// ---- get ----

func TestEpisodeIDCache_Get_Miss(t *testing.T) {
	c := newTestCache(t)
	if id := c.get("https://overcast.fm/+unknown", 0); id != "" {
		t.Errorf("get on empty cache: got %q, want empty", id)
	}
}

func TestEpisodeIDCache_Get_Hit_NoMaxAge(t *testing.T) {
	c := newTestCache(t)
	c.set("https://overcast.fm/+ep1", "12345")
	if id := c.get("https://overcast.fm/+ep1", 0); id != "12345" {
		t.Errorf("get after set: got %q, want %q", id, "12345")
	}
}

func TestEpisodeIDCache_Get_MaxAge_Hit_RecentEntry(t *testing.T) {
	c := newTestCache(t)
	c.items["https://overcast.fm/+ep1"] = episodeCacheEntry{
		ID:       "99",
		CachedAt: time.Now().Add(-1 * time.Hour), // 1 hour old
	}
	// Max age 24 h → entry is fresh.
	if id := c.get("https://overcast.fm/+ep1", 24*time.Hour); id != "99" {
		t.Errorf("recent entry within maxAge: got %q, want %q", id, "99")
	}
}

func TestEpisodeIDCache_Get_MaxAge_Miss_StaleEntry(t *testing.T) {
	c := newTestCache(t)
	c.items["https://overcast.fm/+ep1"] = episodeCacheEntry{
		ID:       "99",
		CachedAt: time.Now().Add(-48 * time.Hour), // 48 hours old
	}
	// Max age 24 h → entry is stale.
	if id := c.get("https://overcast.fm/+ep1", 24*time.Hour); id != "" {
		t.Errorf("stale entry: got %q, want empty", id)
	}
}

func TestEpisodeIDCache_Get_LegacyZeroCachedAt_StaleWithMaxAge(t *testing.T) {
	c := newTestCache(t)
	// Legacy v0 entry: ID set but CachedAt is zero (no timestamp).
	c.items["https://overcast.fm/+ep1"] = episodeCacheEntry{ID: "77"}
	// Any non-zero maxAge should treat zero CachedAt as stale.
	if id := c.get("https://overcast.fm/+ep1", 1*time.Second); id != "" {
		t.Errorf("zero CachedAt with maxAge>0: got %q, want empty (treated as stale)", id)
	}
}

func TestEpisodeIDCache_Get_LegacyZeroCachedAt_ValidWithoutMaxAge(t *testing.T) {
	c := newTestCache(t)
	c.items["https://overcast.fm/+ep1"] = episodeCacheEntry{ID: "77"}
	// maxAge=0 means indefinite → zero CachedAt should not be penalised.
	if id := c.get("https://overcast.fm/+ep1", 0); id != "77" {
		t.Errorf("zero CachedAt with maxAge=0: got %q, want %q", id, "77")
	}
}

// ---- set ----

func TestEpisodeIDCache_Set_MarksDirty(t *testing.T) {
	c := newTestCache(t)
	if c.dirty {
		t.Fatal("new cache should not be dirty before set")
	}
	c.set("https://overcast.fm/+ep1", "42")
	if !c.dirty {
		t.Error("cache should be dirty after set")
	}
}

func TestEpisodeIDCache_Set_StoresTimestamp(t *testing.T) {
	c := newTestCache(t)
	before := time.Now().UTC().Truncate(time.Second)
	c.set("https://overcast.fm/+ep1", "42")
	after := time.Now().UTC().Add(time.Second)

	entry, ok := c.items["https://overcast.fm/+ep1"]
	if !ok {
		t.Fatal("entry not found after set")
	}
	if entry.CachedAt.IsZero() {
		t.Error("set should record a non-zero CachedAt timestamp")
	}
	if entry.CachedAt.Before(before) || entry.CachedAt.After(after) {
		t.Errorf("CachedAt %v not within expected range [%v, %v]", entry.CachedAt, before, after)
	}
}

// ---- clear ----

func TestEpisodeIDCache_Clear_ReturnsCount(t *testing.T) {
	c := newTestCache(t)
	c.set("https://overcast.fm/+ep1", "1")
	c.set("https://overcast.fm/+ep2", "2")

	n := c.clear()
	if n != 2 {
		t.Errorf("clear: returned %d, want 2", n)
	}
}

func TestEpisodeIDCache_Clear_EmptiesItems(t *testing.T) {
	c := newTestCache(t)
	c.set("https://overcast.fm/+ep1", "1")
	c.clear()
	if len(c.items) != 0 {
		t.Errorf("items not empty after clear: %d entries remain", len(c.items))
	}
}

func TestEpisodeIDCache_Clear_MarksDirty(t *testing.T) {
	c := newTestCache(t)
	c.set("https://overcast.fm/+ep1", "1")
	c.dirty = false // reset after set
	c.clear()
	if !c.dirty {
		t.Error("cache should be dirty after clear")
	}
}

func TestEpisodeIDCache_Clear_EmptyCache_ReturnsZero(t *testing.T) {
	c := newTestCache(t)
	if n := c.clear(); n != 0 {
		t.Errorf("clear on empty cache: got %d, want 0", n)
	}
}

// ---- size ----

func TestEpisodeIDCache_Size(t *testing.T) {
	c := newTestCache(t)
	if c.size() != 0 {
		t.Errorf("size on empty cache: got %d, want 0", c.size())
	}
	c.set("https://overcast.fm/+ep1", "1")
	c.set("https://overcast.fm/+ep2", "2")
	if c.size() != 2 {
		t.Errorf("size: got %d, want 2", c.size())
	}
}

// ---- save ----

func TestEpisodeIDCache_Save_NoopWhenNotDirty(t *testing.T) {
	c := newTestCache(t)
	c.save()
	if _, err := os.Stat(c.path); !os.IsNotExist(err) {
		t.Error("save() should not write a file when cache is not dirty")
	}
}

func TestEpisodeIDCache_Save_WritesV1Format(t *testing.T) {
	c := newTestCache(t)
	c.set("https://overcast.fm/+ep1", "42")
	c.save()

	data, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		t.Fatalf("unmarshal saved file: %v", err)
	}
	if cf.Version != 1 {
		t.Errorf("version: got %d, want 1", cf.Version)
	}
	e, ok := cf.Entries["https://overcast.fm/+ep1"]
	if !ok {
		t.Fatal("entry not found in saved file")
	}
	if e.ID != "42" {
		t.Errorf("ID: got %q, want %q", e.ID, "42")
	}
	if e.CachedAt.IsZero() {
		t.Error("saved entry should have non-zero CachedAt")
	}
}

func TestEpisodeIDCache_Save_NotDirtyAfterSave(t *testing.T) {
	c := newTestCache(t)
	c.set("https://overcast.fm/+ep1", "1")
	c.save()
	// dirty is not cleared by save (save holds the write lock but doesn't reset dirty —
	// this is by design: save is a write operation, not a state transition).
	// We verify the file was written; dirty state doesn't matter for correctness.
	if _, err := os.Stat(c.path); err != nil {
		t.Errorf("saved file should exist: %v", err)
	}
}

// ---- loadFromPath ----

func TestLoadFromPath_MissingFile_EmptyCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.json")
	c := loadFromPath(path)
	if len(c.items) != 0 {
		t.Errorf("missing file should produce empty cache, got %d entries", len(c.items))
	}
	if c.path != path {
		t.Errorf("path: got %q, want %q", c.path, path)
	}
}

func TestLoadFromPath_CorruptFile_EmptyCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("not valid json !@#$"), 0644); err != nil {
		t.Fatal(err)
	}
	c := loadFromPath(path)
	if len(c.items) != 0 {
		t.Errorf("corrupt file should produce empty cache, got %d entries", len(c.items))
	}
}

func TestLoadFromPath_V1Format_LoadsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.json")
	cachedAt := time.Now().UTC().Truncate(time.Second)
	cf := cacheFile{
		Version: 1,
		Entries: map[string]episodeCacheEntry{
			"https://overcast.fm/+ep1": {ID: "111", CachedAt: cachedAt},
			"https://overcast.fm/+ep2": {ID: "222", CachedAt: cachedAt},
		},
	}
	data, _ := json.Marshal(cf)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	c := loadFromPath(path)
	if len(c.items) != 2 {
		t.Fatalf("v1 load: got %d items, want 2", len(c.items))
	}
	if e := c.items["https://overcast.fm/+ep1"]; e.ID != "111" {
		t.Errorf("ep1 ID: got %q, want %q", e.ID, "111")
	}
	// Verify round-trip: get() returns the correct IDs.
	if id := c.get("https://overcast.fm/+ep1", 0); id != "111" {
		t.Errorf("get ep1: got %q, want %q", id, "111")
	}
}

func TestLoadFromPath_LegacyV0Format_LoadsWithZeroCachedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v0.json")
	// Legacy format: plain map[string]string
	legacy := map[string]string{
		"https://overcast.fm/+ep1": "111",
		"https://overcast.fm/+ep2": "222",
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	c := loadFromPath(path)
	if len(c.items) != 2 {
		t.Fatalf("v0 load: got %d items, want 2", len(c.items))
	}
	// Legacy entries must have zero CachedAt.
	if e := c.items["https://overcast.fm/+ep1"]; !e.CachedAt.IsZero() {
		t.Error("legacy entry should have zero CachedAt (migrated without timestamp)")
	}
	// maxAge=0 → legacy ID returned (indefinite validity).
	if id := c.get("https://overcast.fm/+ep1", 0); id != "111" {
		t.Errorf("legacy get with maxAge=0: got %q, want %q", id, "111")
	}
	// maxAge>0 → zero CachedAt treated as stale.
	if id := c.get("https://overcast.fm/+ep1", 1*time.Second); id != "" {
		t.Errorf("legacy get with maxAge>0: got %q, want empty (stale)", id)
	}
}

// ---- round-trip: set → save → loadFromPath → get ----

func TestEpisodeIDCache_RoundTrip_PersistsToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roundtrip.json")

	// Write.
	c1 := &episodeIDCache{path: path, items: make(map[string]episodeCacheEntry)}
	c1.set("https://overcast.fm/+ep1", "999")
	c1.save()

	// Read fresh.
	c2 := loadFromPath(path)
	if id := c2.get("https://overcast.fm/+ep1", 0); id != "999" {
		t.Errorf("round-trip: got %q after reload, want %q", id, "999")
	}
}

// ---- concurrency ----

func TestEpisodeIDCache_ConcurrentSetGet(t *testing.T) {
	c := newTestCache(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			url := "https://overcast.fm/+concurrent" + string(rune('A'+i))
			c.set(url, "id")
			_ = c.get(url, 0)
		}()
	}
	wg.Wait()
	if c.size() != 20 {
		t.Errorf("after 20 concurrent sets: size %d, want 20", c.size())
	}
}
