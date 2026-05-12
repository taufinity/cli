package updatecheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/taufinity/cli/internal/config"
)

// Cache is the on-disk record of the last GitHub commits-API check.
type Cache struct {
	// CheckedAt is when we last queried GitHub. Zero value means "never".
	CheckedAt time.Time `json:"checked_at"`

	// LatestSHA is the SHA returned by GitHub at CheckedAt, or "" if the call
	// failed (we cache the failure for 24h to avoid hammering on a broken
	// network).
	LatestSHA string `json:"latest_sha"`
}

// IsFresh reports whether the cache was written less than maxAge ago.
func (c Cache) IsFresh(now time.Time, maxAge time.Duration) bool {
	if c.CheckedAt.IsZero() {
		return false
	}
	return now.Sub(c.CheckedAt) < maxAge
}

// cachePath returns the on-disk location of the cache file.
func cachePath() string {
	return filepath.Join(config.Dir(), "update-check.json")
}

// LoadCache reads the cache from disk. A missing or unparseable file is NOT an
// error — it returns a zero-value Cache so callers can treat it as "never
// checked." We deliberately don't surface read errors to the caller: a corrupt
// cache must never break the parent CLI command.
func LoadCache() Cache {
	data, err := os.ReadFile(cachePath())
	if err != nil {
		return Cache{}
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		return Cache{}
	}
	return c
}

// SaveCache writes the cache atomically: tmp file + rename. On the same
// filesystem the rename is atomic, so a mid-write process exit leaves either
// the previous valid cache or the new valid cache — never a torn file.
func SaveCache(c Cache) error {
	dir := config.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}

	finalPath := cachePath()
	tmpPath := finalPath + ".tmp"

	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write tmp cache: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Best effort cleanup; don't mask the rename error with this.
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename cache: %w", err)
	}

	return nil
}

