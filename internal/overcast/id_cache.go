package overcast

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// episodeCacheEntry is a single record in the on-disk episode ID cache.
type episodeCacheEntry struct {
	ID       string    `json:"id"`
	CachedAt time.Time `json:"t"`
}

// cacheFile is the versioned on-disk format.
// Version 1 stores entries as a map[url]episodeCacheEntry.
// Version 0 (absent) is the legacy format: map[url]string — handled on load.
type cacheFile struct {
	Version int                          `json:"v,omitempty"`
	Entries map[string]episodeCacheEntry `json:"entries"`
}

// episodeIDCache is a persistent, thread-safe map from Overcast episode URLs to
// their numeric data-item-id values. Persisting the cache between runs avoids
// re-fetching individual episode pages for episodes already resolved in a prior sync.
//
// Cache file location: os.UserCacheDir()/podcast-migrate/overcast-episode-ids.json
// Fallback (if UserCacheDir fails): os.TempDir()/podcast-migrate-overcast-episode-ids.json
//
// Load and save are both best-effort: errors are non-fatal so a missing or
// corrupt cache file never blocks a sync.
type episodeIDCache struct {
	mu    sync.RWMutex
	items map[string]episodeCacheEntry // episodeURL → entry
	path  string
	dirty bool
}

// loadEpisodeIDCache reads the cache from disk and returns it. If the file does
// not exist or cannot be parsed, an empty cache is returned.
func loadEpisodeIDCache() *episodeIDCache {
	path := episodeIDCachePath()
	c := &episodeIDCache{path: path, items: make(map[string]episodeCacheEntry)}

	data, err := os.ReadFile(path)
	if err != nil {
		return c // missing is fine
	}

	// Try v1 (versioned) format first.
	var cf cacheFile
	if json.Unmarshal(data, &cf) == nil && cf.Version >= 1 {
		c.items = cf.Entries
		return c
	}

	// Fall back to legacy v0 format: map[string]string (no timestamps).
	// Migrate entries with a zero CachedAt so maxAge filtering treats them as
	// "age unknown" and re-fetches them when a max-age is configured.
	var legacy map[string]string
	if json.Unmarshal(data, &legacy) == nil {
		for url, id := range legacy {
			c.items[url] = episodeCacheEntry{ID: id} // CachedAt zero = no timestamp
		}
	}
	return c
}

// episodeIDCachePath returns the absolute path of the cache file.
func episodeIDCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "podcast-migrate-overcast-episode-ids.json")
	}
	return filepath.Join(dir, "podcast-migrate", "overcast-episode-ids.json")
}

// get returns the cached numericID for the given episode URL.
// Returns "" when the entry is absent or when maxAge > 0 and the entry is stale
// (including entries migrated from the legacy format that have no timestamp).
func (c *episodeIDCache) get(episodeURL string, maxAge time.Duration) string {
	c.mu.RLock()
	entry, ok := c.items[episodeURL]
	c.mu.RUnlock()
	if !ok {
		return ""
	}
	if maxAge > 0 {
		// No timestamp (legacy entry) or entry too old → treat as stale.
		if entry.CachedAt.IsZero() || time.Since(entry.CachedAt) > maxAge {
			return ""
		}
	}
	return entry.ID
}

// set stores a numericID for an episode URL with the current timestamp and
// marks the cache as dirty.
func (c *episodeIDCache) set(episodeURL, numericID string) {
	c.mu.Lock()
	c.items[episodeURL] = episodeCacheEntry{ID: numericID, CachedAt: time.Now().UTC()}
	c.dirty = true
	c.mu.Unlock()
}

// clear removes all entries and marks the cache as dirty so the (now empty)
// state is persisted on the next save.
func (c *episodeIDCache) clear() int {
	c.mu.Lock()
	n := len(c.items)
	c.items = make(map[string]episodeCacheEntry)
	c.dirty = true
	c.mu.Unlock()
	return n
}

// size returns the number of cached entries (including potentially stale ones).
func (c *episodeIDCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// save writes the cache to disk in v1 format. No-op if nothing changed.
func (c *episodeIDCache) save() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0755); err != nil {
		fmt.Printf("overcast: warning: could not create cache directory: %v\n", err)
		return
	}
	cf := cacheFile{Version: 1, Entries: c.items}
	data, err := json.Marshal(cf)
	if err != nil {
		return
	}
	if err := os.WriteFile(c.path, data, 0644); err != nil {
		fmt.Printf("overcast: warning: could not save episode ID cache to %s: %v\n", c.path, err)
	}
}
