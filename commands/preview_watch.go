package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const bannerDebounce = 250 * time.Millisecond

// isEditorTempFile is true for the files editors and the OS create around a
// save: swap files, backups, lock files, Finder metadata.
func isEditorTempFile(name string) bool {
	base := filepath.Base(name)
	switch {
	case base == ".DS_Store", base == "4913":
		return true
	case strings.HasSuffix(base, ".swp"), strings.HasSuffix(base, ".swx"), strings.HasSuffix(base, "~"):
		return true
	case strings.HasPrefix(base, ".#"):
		return true
	}
	return false
}

func skipWatchDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".hg", ".svn":
		return true
	}
	return false
}

// watchBanner watches the template folder (recursively) and, when it lies
// elsewhere, the data file. It calls onChange once, debounce after the last
// relevant event. It returns when ctx ends.
func watchBanner(ctx context.Context, dir, dataFile string, debounce time.Duration, onChange func()) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	addTree := func(root string) {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != root && skipWatchDir(d.Name()) {
					return filepath.SkipDir
				}
				_ = w.Add(p)
			}
			return nil
		})
	}
	addTree(dir)

	absDir, _ := filepath.Abs(dir)
	absData := ""
	if dataFile != "" {
		absData, _ = filepath.Abs(dataFile)
		if !strings.HasPrefix(absData, absDir+string(os.PathSeparator)) {
			_ = w.Add(filepath.Dir(absData)) // data file outside the folder
		}
	}
	relevant := func(name string) bool {
		abs, _ := filepath.Abs(name)
		if isEditorTempFile(abs) {
			return false
		}
		if abs == absData || strings.HasPrefix(abs, absDir+string(os.PathSeparator)) {
			return true
		}
		return false
	}

	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op == fsnotify.Chmod || !relevant(ev.Name) {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() && !skipWatchDir(st.Name()) {
					addTree(ev.Name)
				}
			}
			if timer == nil {
				timer = time.NewTimer(debounce)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounce)
			}
		case <-w.Errors:
		case <-timerC:
			timer, timerC = nil, nil
			onChange()
		}
	}
}

// bannerSession ties a source to the server: a refresh fills again and either
// switches the server over or, on failure, keeps the last good banner under an
// error message and prints one Dutch line.
type bannerSession struct {
	Fill func(ctx context.Context) (*bannerResult, error)
	Srv  *bannerServer
	Warn func(msg string) // one line per failure; nil = silent
	mu   sync.Mutex       // refills never overlap
}

func (s *bannerSession) Refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.Fill(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := err.Error()
		s.Srv.Fail(msg)
		if s.Warn != nil {
			s.Warn("Vernieuwen mislukt: " + msg)
		}
		return err
	}
	s.Srv.Swap(res)
	return nil
}
