package overcast

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

const sourceOPMLCacheMaxAge = 24 * time.Hour

// sourceOPMLCacheTestPath, when non-empty, overrides the default cache path.
// Only set via setSourceOPMLCachePathForTest; never in production code.
var sourceOPMLCacheTestPath string

// setSourceOPMLCachePathForTest redirects the source OPML cache to path so
// that tests can use a temp directory instead of the real user cache.
// Reset to "" to restore the default behaviour.
func setSourceOPMLCachePathForTest(path string) { sourceOPMLCacheTestPath = path }

// sourceOPMLCachePath returns the path where the auto-fetched Overcast source OPML is cached.
// Follows the same directory convention as the episode ID cache.
func sourceOPMLCachePath() string {
	if sourceOPMLCacheTestPath != "" {
		return sourceOPMLCacheTestPath
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "podcast-migrate-overcast-source.opml")
	}
	return filepath.Join(dir, "podcast-migrate", "overcast-source.opml")
}

// loadCachedSourceOPML returns the cached source OPML library if it exists and is less than
// sourceOPMLCacheMaxAge old. Returns nil, nil on a cache miss or when forceRefresh is true.
// A corrupt or unreadable cache file is treated as a miss (not an error).
func loadCachedSourceOPML(ctx context.Context, forceRefresh bool) (*sourceOPMLCacheResult, error) {
	if forceRefresh {
		return nil, nil
	}
	path := sourceOPMLCachePath()
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil // not found
	}
	age := time.Since(info.ModTime())
	if age > sourceOPMLCacheMaxAge {
		return nil, nil // stale
	}
	lib, err := NewOPMLReader(path).Read(ctx)
	if err != nil {
		return nil, nil // corrupt — treat as miss
	}
	return &sourceOPMLCacheResult{lib: lib, age: age}, nil
}

type sourceOPMLCacheResult struct {
	lib *model.Library
	age time.Duration
}

// saveRawSourceOPML writes raw OPML bytes to the cache path, creating parent directories as needed.
func saveRawSourceOPML(data []byte) error {
	path := sourceOPMLCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("overcast: create source OPML cache dir: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}
