package cfaccess

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The cache keeps the service token read from gcloud on disk, next to
// credentials.json and with the same discipline: directory 0700, file 0600,
// written to a temp file in the same directory and renamed over the target so
// a concurrent reader never sees a partial file. One file per host.
//
// It is dropped and re-read from gcloud when Access refuses the cached token,
// so a rotated token heals itself on the next request.

type cacheEntry struct {
	Host         string    `json:"host"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret"`
	FetchedAt    time.Time `json:"fetched_at"`
}

func cacheFile(dir, host string) string {
	return filepath.Join(dir, "cf-access-"+strings.ToLower(host)+".json")
}

func (r *Resolver) readCache(host string) (Token, bool) {
	if r.CacheDir == nil {
		return Token{}, false
	}
	path := cacheFile(r.CacheDir(), host)
	info, err := os.Stat(path)
	if err != nil {
		return Token{}, false
	}
	// A cache readable by others is not trusted: someone loosened it, or it
	// is not ours. Drop it and read the token afresh.
	if info.Mode().Perm()&0o077 != 0 {
		_ = os.Remove(path)
		return Token{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Token{}, false
	}
	var e cacheEntry
	if err := json.Unmarshal(data, &e); err != nil || !strings.EqualFold(e.Host, host) {
		return Token{}, false
	}
	tok := Token{ID: e.ClientID, Secret: e.ClientSecret}
	return tok, tok.valid()
}

func (r *Resolver) writeCache(host string, tok Token) error {
	if r.CacheDir == nil {
		return nil
	}
	dir := r.CacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cacheEntry{
		Host: strings.ToLower(host), ClientID: tok.ID, ClientSecret: tok.Secret, FetchedAt: time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "cf-access-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp cache: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp cache: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp cache: %w", err)
	}
	if err := os.Rename(tmpName, cacheFile(dir, host)); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	return nil
}

func (r *Resolver) removeCache(host string) {
	if r.CacheDir == nil {
		return
	}
	_ = os.Remove(cacheFile(r.CacheDir(), host))
}
