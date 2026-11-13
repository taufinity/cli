package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	ignore "github.com/sabhiram/go-gitignore"

	"github.com/taufinity/cli/internal/archive"
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

// ignoreFiles are the files whose change changes what goes into the template ZIP.
var ignoreFiles = map[string]bool{".gitignore": true, ".dockerignore": true, ".taufinityignore": true, "taufinity.yaml": true}

// uploadMatcher is the same ignore matcher the template ZIP is built with
// (internal/archive.CreateZip), so a file that never goes up never triggers a refill.
func uploadMatcher(dir string) *ignore.GitIgnore {
	return ignore.CompileIgnoreLines(archive.CollectIgnorePatterns(dir)...)
}

func skipWatchDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".hg", ".svn", ".taufinity":
		return true
	}
	return false
}

// bannerMaxWatchDirs is the most folders the watcher follows. On macOS (kqueue)
// every file of a watched folder costs a file handle, so an unbounded tree can
// exhaust the process.
const bannerMaxWatchDirs = 2000

// watchOptions tune watchBannerWith. The zero value is the normal behaviour.
type watchOptions struct {
	Warn     func(msg string)     // nil = stderr
	MaxDirs  int                  // 0 = bannerMaxWatchDirs
	AddError func(p string) error // test hook: a non-nil result stands for a failed Add
}

// watchBanner watches the template folder (recursively) and, when it lies
// elsewhere, the data file. It calls onChange once, debounce after the last
// relevant event. It returns when ctx ends.
func watchBanner(ctx context.Context, dir, dataFile string, debounce time.Duration, onChange func()) error {
	return watchBannerWith(ctx, dir, dataFile, debounce, onChange, watchOptions{})
}

func watchBannerWith(ctx context.Context, dir, dataFile string, debounce time.Duration, onChange func(), opts watchOptions) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	warn := opts.Warn
	if warn == nil {
		warn = func(m string) { fmt.Fprintln(os.Stderr, m) }
	}
	maxDirs := opts.MaxDirs
	if maxDirs <= 0 {
		maxDirs = bannerMaxWatchDirs
	}
	watched := 0
	capWarned := false
	failed := map[string]bool{}
	add := func(p string) bool {
		if watched >= maxDirs {
			if !capWarned {
				capWarned = true
				warn(fmt.Sprintf("Waarschuwing: de map bevat meer dan %d mappen; de rest wordt niet bewaakt. Zet mappen die niet nodig zijn in .taufinityignore.", maxDirs))
			}
			return false
		}
		var err error
		if opts.AddError != nil {
			err = opts.AddError(p)
		}
		if err == nil {
			err = w.Add(p)
		}
		if err != nil {
			if !failed[p] {
				failed[p] = true
				warn(fmt.Sprintf("Waarschuwing: %s kan niet worden bewaakt (%v); wijzigingen daar worden gemist.", p, err))
			}
			return true
		}
		watched++
		return true
	}

	// matcher is the upload's ignore matcher: a folder it excludes is never
	// watched, as nothing in it ever goes up.
	matcher := uploadMatcher(dir)
	addTree := func(root string) {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != root && skipWatchDir(d.Name()) {
					return filepath.SkipDir
				}
				if p != dir {
					if rel, err := filepath.Rel(dir, p); err == nil && (matcher.MatchesPath(filepath.ToSlash(rel)) || matcher.MatchesPath(filepath.ToSlash(rel)+"/")) {
						return filepath.SkipDir
					}
				}
				if !add(p) && watched >= maxDirs {
					return filepath.SkipAll
				}
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
			add(filepath.Dir(absData)) // data file outside the folder
		}
	}
	relevant := func(name string) bool {
		abs, _ := filepath.Abs(name)
		if isEditorTempFile(abs) {
			return false
		}
		if abs == absData {
			return true // the data file counts even when an ignore rule would leave it out of the ZIP
		}
		if !strings.HasPrefix(abs, absDir+string(os.PathSeparator)) {
			return false
		}
		if ignoreFiles[filepath.Base(abs)] {
			matcher = uploadMatcher(dir) // the rules changed: what goes up may have changed too
			return true
		}
		rel, err := filepath.Rel(absDir, abs)
		if err != nil {
			return false
		}
		return !matcher.MatchesPath(filepath.ToSlash(rel))
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

// latestOnly runs refresh for every change, cancelling the one still in flight:
// a save during an upload makes that upload stale, so it is dropped rather than
// waited for (as Vite and Shopify's theme dev do). Refreshes still never overlap
// (bannerSession.mu); a cancelled one returns without an error message.
func latestOnly(parent context.Context, refresh func(ctx context.Context)) func() {
	var mu sync.Mutex
	cancel := context.CancelFunc(func() {})
	return func() {
		mu.Lock()
		cancel()
		ctx, c := context.WithCancel(parent)
		cancel = c
		mu.Unlock()
		go refresh(ctx)
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

	pauseUntil time.Time // after a 429/503 no refill is sent before this moment
}

// errBannerPaused is returned by Refresh while the server asked us to wait.
var errBannerPaused = errors.New("vernieuwen is gepauzeerd")

func (s *bannerSession) Refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.pauseUntil) {
		return errBannerPaused // the server asked us to wait; sending now only makes it worse
	}
	res, err := s.Fill(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := err.Error()
		var pause *bannerPause
		if errors.As(err, &pause) {
			s.pauseUntil = pause.Until
			msg += fmt.Sprintf("; vernieuwen is gepauzeerd tot %s", pause.Until.Format("15:04:05"))
		}
		s.Srv.Fail(msg)
		if s.Warn != nil {
			s.Warn("Vernieuwen mislukt: " + msg)
		}
		return err
	}
	s.Srv.Swap(res)
	return nil
}
