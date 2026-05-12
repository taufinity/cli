// Package updatecheck queries GitHub for the latest commit on the CLI's main
// branch, caches the result for 24h, and prints a one-line stderr warning when
// the running binary is behind.
//
// Design constraints:
//   - Must never block the parent command for noticeable time (we cap waits at
//     a few hundred ms).
//   - Must never break the parent command on any failure (corrupt cache,
//     network error, 4xx/5xx from GitHub — all swallowed).
//   - Must be opt-out-friendly: env var, config flag, --quiet, dirty tree, and
//     a cobra annotation for special commands (MCP stdio).
package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/taufinity/cli/internal/buildinfo"
)

// Defaults.
const (
	DefaultAPIURL     = "https://api.github.com/repos/taufinity/cli/commits/main"
	DefaultCacheMaxAge = 24 * time.Hour
	DefaultHTTPTimeout = 2 * time.Second

	// EnvDisable, when set to "1", skips both the network check and the
	// warning at exit.
	EnvDisable = "TAUFINITY_NO_UPDATE_CHECK"

	// AnnotationSuppress is the cobra command annotation that disables the
	// update check side effects (background goroutine + warning) for that
	// command. Used by `taufinity mcp stdio`.
	AnnotationSuppress = "suppress-update-warning"
)

// fetchSHA queries the GitHub commits API and returns the head SHA of the
// configured branch. The httpClient is injectable for tests; pass nil for the
// default (http.DefaultClient is NOT suitable — we always want our own short
// timeout, applied at the context level).
func fetchSHA(ctx context.Context, apiURL string, httpClient *http.Client) (string, error) {
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "taufinity-cli-updatecheck")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api returned status %d", resp.StatusCode)
	}

	var payload struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.SHA == "" {
		return "", fmt.Errorf("github api returned empty sha")
	}
	return payload.SHA, nil
}

// Runner controls a background staleness check.
type Runner struct {
	APIURL     string
	HTTPClient *http.Client
	Timeout    time.Duration
	Now        func() time.Time // for tests
	Debug      io.Writer        // non-nil to write debug lines on failure

	wg    sync.WaitGroup
	done  chan struct{}
}

// Start kicks off the network check in a background goroutine. The goroutine
// writes the result (or a failure marker) to the cache file when it finishes.
// Call Wait to block for completion up to a bounded duration.
func (r *Runner) Start(ctx context.Context) {
	r.done = make(chan struct{})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(r.done)

		apiURL := r.APIURL
		if apiURL == "" {
			apiURL = DefaultAPIURL
		}
		timeout := r.Timeout
		if timeout == 0 {
			timeout = DefaultHTTPTimeout
		}
		nowFn := r.Now
		if nowFn == nil {
			nowFn = time.Now
		}

		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		sha, err := fetchSHA(cctx, apiURL, r.HTTPClient)
		if err != nil {
			if r.Debug != nil {
				fmt.Fprintf(r.Debug, "updatecheck: %v\n", err)
			}
			// Cache the failure for the full cache window so we back off.
			_ = SaveCache(Cache{CheckedAt: nowFn(), LatestSHA: ""})
			return
		}
		_ = SaveCache(Cache{CheckedAt: nowFn(), LatestSHA: sha})
	}()
}

// Wait blocks for the goroutine to finish or for d to elapse. If d is zero,
// Wait returns immediately if the goroutine hasn't started or has already
// finished.
func (r *Runner) Wait(d time.Duration) {
	if r.done == nil {
		return
	}
	if d == 0 {
		select {
		case <-r.done:
		default:
		}
		return
	}
	select {
	case <-r.done:
	case <-time.After(d):
	}
}

// Options controls MaybeWarn behavior.
type Options struct {
	// Quiet suppresses the warning unconditionally.
	Quiet bool

	// ConfigDisabled is the resolved value of the user-config opt-out
	// (UserConfig.UpdateCheck == "false").
	ConfigDisabled bool

	// CommandSuppress is set by the caller when the running cobra command
	// (or any ancestor) carries AnnotationSuppress.
	CommandSuppress bool
}

// shouldWarn applies the opt-out matrix. Pure function, easy to test.
func shouldWarn(info buildinfo.Info, cache Cache, opts Options) bool {
	if info.Dirty {
		return false
	}
	if opts.Quiet || opts.ConfigDisabled || opts.CommandSuppress {
		return false
	}
	if os.Getenv(EnvDisable) == "1" {
		return false
	}
	if cache.LatestSHA == "" {
		// Either we've never checked, or the last check failed. Either way,
		// no comparison is possible — stay silent.
		return false
	}
	if info.Commit == "unknown" {
		// Built without VCS info, no way to compare.
		return false
	}

	current := strings.ToLower(info.Commit)
	latest := strings.ToLower(cache.LatestSHA)

	// Either could be a short SHA; compare on the prefix of the shorter one.
	n := len(current)
	if len(latest) < n {
		n = len(latest)
	}
	if n < 7 {
		// Implausible — protect against false alarms.
		return false
	}
	return current[:n] != latest[:n]
}

// MaybeWarn writes a one-line warning to out if the current binary is behind
// the cached latest SHA. Returns true if it wrote.
func MaybeWarn(out io.Writer, info buildinfo.Info, cache Cache, opts Options) bool {
	if !shouldWarn(info, cache, opts) {
		return false
	}
	currentShort := info.Commit
	if len(currentShort) > 7 {
		currentShort = currentShort[:7]
	}
	latestShort := cache.LatestSHA
	if len(latestShort) > 7 {
		latestShort = latestShort[:7]
	}
	fmt.Fprintf(out, "A newer taufinity is available (%s → %s). Run: taufinity update\n", currentShort, latestShort)
	return true
}
