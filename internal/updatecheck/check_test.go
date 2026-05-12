package updatecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/buildinfo"
)

// withTempHome redirects config.Dir() to a temp directory for the test.
// It works because config.Dir() reads HOME via os.UserHomeDir / $HOME.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

func TestCacheRoundTrip(t *testing.T) {
	withTempHome(t)

	c := Cache{CheckedAt: time.Now().UTC().Truncate(time.Second), LatestSHA: "abc123"}
	if err := SaveCache(c); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	got := LoadCache()
	if !got.CheckedAt.Equal(c.CheckedAt) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, c.CheckedAt)
	}
	if got.LatestSHA != c.LatestSHA {
		t.Errorf("LatestSHA = %q, want %q", got.LatestSHA, c.LatestSHA)
	}
}

func TestSaveCacheAtomic_NoTmpLeftBehind(t *testing.T) {
	withTempHome(t)

	if err := SaveCache(Cache{CheckedAt: time.Now(), LatestSHA: "ok"}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	tmp := cachePath() + ".tmp"
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("expected %s to be cleaned up, stat err = %v", tmp, err)
	}

	// Cache must be valid JSON.
	data, err := os.ReadFile(cachePath())
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("cache not valid JSON: %v", err)
	}
}

func TestLoadCache_MissingFile(t *testing.T) {
	withTempHome(t)
	got := LoadCache()
	if !got.CheckedAt.IsZero() {
		t.Errorf("expected zero-value Cache for missing file, got %+v", got)
	}
}

func TestLoadCache_CorruptFile(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".config", "taufinity")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "update-check.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := LoadCache()
	if !got.CheckedAt.IsZero() {
		t.Errorf("expected zero-value Cache for corrupt file, got %+v", got)
	}
}

func TestCacheIsFresh(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		checkedAt time.Time
		maxAge    time.Duration
		want      bool
	}{
		{"zero is never fresh", time.Time{}, 24 * time.Hour, false},
		{"1h ago, 24h window: fresh", now.Add(-1 * time.Hour), 24 * time.Hour, true},
		{"25h ago, 24h window: stale", now.Add(-25 * time.Hour), 24 * time.Hour, false},
		{"exactly window: stale (strict <)", now.Add(-24 * time.Hour), 24 * time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Cache{CheckedAt: tt.checkedAt}
			if got := c.IsFresh(now, tt.maxAge); got != tt.want {
				t.Errorf("IsFresh = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFetchSHA_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}`))
	}))
	defer srv.Close()

	sha, err := fetchSHA(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("fetchSHA: %v", err)
	}
	if sha != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("sha = %q", sha)
	}
}

func TestFetchSHA_403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := fetchSHA(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestFetchSHA_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := fetchSHA(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("expected error on 5xx, got nil")
	}
}

func TestFetchSHA_EmptySHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sha":""}`))
	}))
	defer srv.Close()

	_, err := fetchSHA(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("expected error on empty sha, got nil")
	}
}

func TestRunner_SuccessWritesCache(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sha":"abcdef1234567890"}`))
	}))
	defer srv.Close()

	r := &Runner{
		APIURL:     srv.URL,
		HTTPClient: srv.Client(),
		Timeout:    2 * time.Second,
		Now:        func() time.Time { return time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC) },
	}
	r.Start(context.Background())
	r.Wait(5 * time.Second)

	got := LoadCache()
	if got.LatestSHA != "abcdef1234567890" {
		t.Errorf("LatestSHA = %q", got.LatestSHA)
	}
	if got.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero after successful check")
	}
}

func TestRunner_FailureCachesEmptySHA(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	r := &Runner{
		APIURL:     srv.URL,
		HTTPClient: srv.Client(),
		Timeout:    2 * time.Second,
		Now:        func() time.Time { return time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC) },
	}
	r.Start(context.Background())
	r.Wait(5 * time.Second)

	got := LoadCache()
	if got.LatestSHA != "" {
		t.Errorf("LatestSHA = %q, want empty (failure backs off)", got.LatestSHA)
	}
	if got.CheckedAt.IsZero() {
		t.Error("CheckedAt must be set even on failure so we back off 24h")
	}
}

func TestRunner_WaitTimeout(t *testing.T) {
	withTempHome(t)
	// Server that blocks long enough to force the bounded wait.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	r := &Runner{
		APIURL:     srv.URL,
		HTTPClient: srv.Client(),
		Timeout:    5 * time.Second,
	}
	r.Start(context.Background())

	start := time.Now()
	r.Wait(50 * time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Errorf("Wait(50ms) took %v, expected ~50ms", elapsed)
	}
}

func TestShouldWarn(t *testing.T) {
	freshOutOfDate := Cache{
		CheckedAt: time.Now(),
		LatestSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	info := buildinfo.Info{
		Version: "abc1234",
		Commit:  "abc1234567890abc",
	}

	tests := []struct {
		name string
		info buildinfo.Info
		c    Cache
		o    Options
		env  string
		want bool
	}{
		{"behind, no opts → warn", info, freshOutOfDate, Options{}, "", true},
		{"quiet suppresses", info, freshOutOfDate, Options{Quiet: true}, "", false},
		{"config-disabled suppresses", info, freshOutOfDate, Options{ConfigDisabled: true}, "", false},
		{"annotation suppresses", info, freshOutOfDate, Options{CommandSuppress: true}, "", false},
		{"env var suppresses", info, freshOutOfDate, Options{}, "1", false},
		{"dirty tree never warns", buildinfo.Info{Commit: "abc1234567890abc", Dirty: true}, freshOutOfDate, Options{}, "", false},
		{"empty latest SHA → silent", info, Cache{CheckedAt: time.Now()}, Options{}, "", false},
		{"unknown commit → silent", buildinfo.Info{Commit: "unknown"}, freshOutOfDate, Options{}, "", false},
		{
			"short ldflag SHA matches long github SHA prefix → no warn",
			buildinfo.Info{Commit: "deadbee"},
			Cache{CheckedAt: time.Now(), LatestSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
			Options{}, "", false,
		},
		{
			"different SHAs of equal length → warn",
			buildinfo.Info{Commit: "deadbeef1111aaaa"},
			Cache{CheckedAt: time.Now(), LatestSHA: "deadbeef2222bbbb"},
			Options{}, "", true,
		},
		{
			"short SHA too short → silent (defensive)",
			buildinfo.Info{Commit: "abc"},
			freshOutOfDate, Options{}, "", false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv(EnvDisable, tt.env)
			}
			if got := shouldWarn(tt.info, tt.c, tt.o); got != tt.want {
				t.Errorf("shouldWarn = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMaybeWarn_WritesFormattedLine(t *testing.T) {
	var buf bytes.Buffer
	info := buildinfo.Info{Commit: "abc1234567890"}
	cache := Cache{CheckedAt: time.Now(), LatestSHA: "def4567890abc"}

	warned := MaybeWarn(&buf, info, cache, Options{})
	if !warned {
		t.Fatal("expected warning")
	}
	out := buf.String()
	if !contains(out, "abc1234") || !contains(out, "def4567") {
		t.Errorf("output missing short SHAs: %q", out)
	}
	if !contains(out, "taufinity update") {
		t.Errorf("output missing command hint: %q", out)
	}
}

func TestMaybeWarn_NoWriteWhenSuppressed(t *testing.T) {
	var buf bytes.Buffer
	info := buildinfo.Info{Commit: "abc1234567890"}
	cache := Cache{CheckedAt: time.Now(), LatestSHA: "def4567890abc"}

	warned := MaybeWarn(&buf, info, cache, Options{Quiet: true})
	if warned {
		t.Error("warned despite Quiet=true")
	}
	if buf.Len() != 0 {
		t.Errorf("wrote despite suppression: %q", buf.String())
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
