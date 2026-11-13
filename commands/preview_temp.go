package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	bannerTempPrefix = "taufinity-preview-"
	bannerLockName   = ".taufinity-lock"
	bannerStaleAfter = 24 * time.Hour
)

// newBannerTempDir makes a temp folder for one fill and writes a lock file with
// this process's id, so a later start can tell it is in use.
func newBannerTempDir() (string, error) {
	dir, err := os.MkdirTemp("", bannerTempPrefix+"*")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, bannerLockName), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// cleanStaleBannerDirs removes taufinity-preview-* folders in tmp that are older
// than maxAge and not in use: a folder whose lock file names a live process is
// kept whatever its age. Returns how many it removed.
func cleanStaleBannerDirs(tmp string, maxAge time.Duration) int {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), bannerTempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < maxAge {
			continue
		}
		full := filepath.Join(tmp, e.Name())
		if raw, err := os.ReadFile(filepath.Join(full, bannerLockName)); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pidAlive(pid) {
				continue
			}
		}
		if os.RemoveAll(full) == nil {
			removed++
		}
	}
	return removed
}

// checkTempOutsideTemplate refuses to run when the temp folder lies inside the
// template folder: the watcher would see every fill write and refill forever.
func checkTempOutsideTemplate(dir string) error {
	tmp := os.TempDir()
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	tmp, dir = filepath.Clean(tmp), filepath.Clean(dir)
	if tmp == dir || strings.HasPrefix(tmp, dir+string(os.PathSeparator)) {
		return fmt.Errorf("de tijdelijke map (%s) ligt in de sjabloonmap (%s); dat zou eindeloos verversen. Zet TMPDIR op een map buiten het sjabloon", tmp, dir)
	}
	return nil
}
