package overcast

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

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
	items map[string]string // episodeURL → numericID
	path  string
	dirty bool
}

// loadEpisodeIDCache reads the cache from disk and returns it. If the file does
// not exist or cannot be parsed, an empty cache is returned.
func loadEpisodeIDCache() *episodeIDCache {
	path := episodeIDCachePath()
	c := &episodeIDCache{path: path, items: make(map[string]string)}
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &c.items) // parse errors → empty map (non-fatal)
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

// get returns the cached numericID for the given episode URL, or "" if not cached.
func (c *episodeIDCache) get(episodeURL string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.items[episodeURL]
}

// set stores a numericID for an episode URL and marks the cache as dirty.
func (c *episodeIDCache) set(episodeURL, numericID string) {
	c.mu.Lock()
	c.items[episodeURL] = numericID
	c.dirty = true
	c.mu.Unlock()
}

// size returns the number of cached entries.
func (c *episodeIDCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// save writes the cache to disk. No-op if nothing changed since load or last save.
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
	data, err := json.Marshal(c.items)
	if err != nil {
		return
	}
	if err := os.WriteFile(c.path, data, 0644); err != nil {
		fmt.Printf("overcast: warning: could not save episode ID cache to %s: %v\n", c.path, err)
	}
}
