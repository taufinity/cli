// Package pixl fires fire-and-forget analytics pixels to the GCS pixel bucket.
// Events are GET requests; the response body is discarded. Never blocks callers.
// No-op when PixlBaseURL is empty (local builds without the ldflag).
package pixl

import (
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/taufinity/cli/internal/telemetry"
)

// PixlBaseURL is injected at build time via -ldflags. Empty → all calls no-op.
var PixlBaseURL string

var (
	version string
	wg      sync.WaitGroup
)

// Init wires the version string. Call from main.go alongside telemetry.Init.
// Avoids a second -ldflags entry that could silently diverge from commands.Version.
func Init(v string) { version = v }

func Enabled() bool { return PixlBaseURL != "" }

// Fire sends GET {PixlBaseURL}/{event}?params in a background goroutine.
// extra key=value pairs are merged on top of the standard params (v, os, arch, did).
// Never blocks. Never surfaces errors — analytics must not affect UX.
func Fire(event string, extra map[string]string) {
	if !Enabled() {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()

		q := url.Values{
			"v":    {version},
			"os":   {runtime.GOOS},
			"arch": {runtime.GOARCH},
			"did":  {telemetry.DeviceID()},
		}
		for k, v := range extra {
			q.Set(k, v)
		}

		reqURL := fmt.Sprintf("%s/%s?%s", strings.TrimRight(PixlBaseURL, "/"), event, q.Encode())
		client := &http.Client{Timeout: 3 * time.Second}
		req, err := http.NewRequest(http.MethodGet, reqURL, nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", fmt.Sprintf("taufinity-cli/%s", version))
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
}

// Flush waits up to d for in-flight pixel goroutines to complete.
// Call from main.go alongside telemetry.Flush() to avoid losing tail events
// when the process exits immediately after firing (e.g. taufinity update).
func Flush(d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}
