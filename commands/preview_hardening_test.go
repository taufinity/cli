package commands

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/archive"
)

// answerZip is a server answer: report.json plus n filler files.
func answerZip(t *testing.T, rep bannerReport, filler int, pad int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	doc := map[string]any{}
	raw, _ := json.Marshal(rep)
	_ = json.Unmarshal(raw, &doc)
	if pad > 0 {
		doc["pad"] = strings.Repeat("x", pad)
	}
	js, _ := json.Marshal(doc)
	e, _ := zw.Create("report.json")
	e.Write(js)
	for i := 0; i < filler; i++ {
		e, _ := zw.Create(fmt.Sprintf("rows/0/f%d", i))
		e.Write([]byte("x"))
	}
	zw.Close()
	return buf.Bytes()
}

func TestBannerPreviewRetries(t *testing.T) {
	ctx := context.Background()
	t.Run("no_retry_on_500", func(t *testing.T) {
		f := newFakeBanner(t)
		f.status, f.body = 500, `{"error":"stuk"}`
		if _, err := f.source(t, templateDir(t, nil)).Fill(ctx); err == nil {
			t.Fatal("expected an error")
		}
		time.Sleep(100 * time.Millisecond)
		if n := f.callCount(); n != 1 {
			t.Fatalf("template sent %d times", n)
		}
	})
	t.Run("no_retry_on_timeout", func(t *testing.T) {
		f := newFakeBanner(t)
		f.delay = 500 * time.Millisecond
		src := f.source(t, templateDir(t, nil))
		c := api.NewPreviewWithTimeout(f.srv.URL, 100*time.Millisecond)
		c.SetAuth("test-token")
		src.Client = c
		if _, err := src.Fill(ctx); err == nil {
			t.Fatal("expected a timeout")
		}
		time.Sleep(800 * time.Millisecond)
		if n := f.callCount(); n != 1 {
			t.Fatalf("template sent %d times", n)
		}
	})
	t.Run("no_retry_on_timeout_mid_body", func(t *testing.T) {
		f := newFakeBanner(t)
		f.stallBody = true
		src := f.source(t, templateDir(t, nil))
		c := api.NewPreviewWithTimeout(f.srv.URL, 150*time.Millisecond)
		c.SetAuth("test-token")
		src.Client = c
		if _, err := src.Fill(ctx); err == nil {
			t.Fatal("expected a timeout")
		}
		time.Sleep(900 * time.Millisecond)
		if n := f.callCount(); n != 1 {
			t.Fatalf("template sent %d times", n)
		}
	})
	t.Run("one_retry_on_503_after_retry_after", func(t *testing.T) {
		f := newFakeBanner(t)
		f.status, f.body, f.retryAfter = 503, `{"error":"even wachten"}`, "1"
		start := time.Now()
		_, err := f.source(t, templateDir(t, nil)).Fill(ctx)
		var pause *bannerPause
		if !errors.As(err, &pause) {
			t.Fatalf("want a pause error, got %v", err)
		}
		if n := f.callCount(); n != 2 {
			t.Fatalf("calls: %d (want one retry)", n)
		}
		if d := time.Since(start); d < 950*time.Millisecond {
			t.Fatalf("retried after %v, before Retry-After", d)
		}
	})
	t.Run("refills_pause_after_429", func(t *testing.T) {
		f := newFakeBanner(t)
		f.status, f.body, f.retryAfter = 429, `{"error":"te veel verzoeken"}`, "1"
		src := f.source(t, templateDir(t, nil))
		srv := newBannerServer()
		var lines []string
		sess := &bannerSession{Fill: src.Fill, Srv: srv, Warn: func(m string) { lines = append(lines, m) }}
		if err := sess.Refresh(ctx); err == nil {
			t.Fatal("expected failure")
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "gepauzeerd tot") {
			t.Fatalf("lines: %v", lines)
		}
		if err := sess.Refresh(ctx); !errors.Is(err, errBannerPaused) {
			t.Fatalf("second refill: %v", err)
		}
		if n := f.callCount(); n != 1 || len(lines) != 1 {
			t.Fatalf("calls %d, lines %v: nothing may be sent while paused", n, lines)
		}
		time.Sleep(1100 * time.Millisecond)
		sess.Refresh(ctx)
		if n := f.callCount(); n != 2 {
			t.Fatalf("after the pause a refill must go out, calls: %d", n)
		}
	})
}

func (f *fakeBanner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int(f.calls)
}

func TestBannerPreviewAnswerBounds(t *testing.T) {
	ctx := context.Background()
	good := func() bannerReport {
		return bannerReport{Entry: "index.html", Width: 300, Height: 250, Rows: []bannerRow{{Index: 0, Label: "voorbeeld"}}}
	}
	run := func(t *testing.T, answer []byte) error {
		t.Helper()
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		f := newFakeBanner(t)
		f.answer = answer
		res, err := f.source(t, templateDir(t, nil)).Fill(ctx)
		if err == nil {
			os.RemoveAll(res.Dir)
			return nil
		}
		if left, _ := filepath.Glob(filepath.Join(tmp, "taufinity-preview-*")); len(left) != 0 {
			t.Fatalf("refused answer left %v behind", left)
		}
		return err
	}
	t.Run("body_over_cap", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		f := newFakeBanner(t)
		f.answer = make([]byte, 65<<20)
		_, err := f.source(t, templateDir(t, nil)).Fill(ctx)
		if err == nil || !strings.Contains(err.Error(), "groter dan 64 MB") {
			t.Fatalf("err: %v", err)
		}
		if left, _ := filepath.Glob(filepath.Join(tmp, "taufinity-preview-*")); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
	})
	t.Run("too_many_files", func(t *testing.T) {
		err := run(t, answerZip(t, good(), 5001, 0))
		if err == nil || !strings.Contains(err.Error(), "meer dan 5000 bestanden") {
			t.Fatalf("err: %v", err)
		}
		if err := run(t, answerZip(t, good(), 100, 0)); err != nil {
			t.Fatalf("a normal answer must pass: %v", err)
		}
	})
	t.Run("report_too_many_rows", func(t *testing.T) {
		rep := good()
		for i := 0; i < 61; i++ {
			rep.Rows = append(rep.Rows, bannerRow{Index: i + 1, Label: "r"})
		}
		if err := run(t, answerZip(t, rep, 0, 0)); err == nil || !strings.Contains(err.Error(), "regels") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("report_absurd_size", func(t *testing.T) {
		rep := good()
		rep.Width = 100000
		if err := run(t, answerZip(t, rep, 0, 0)); err == nil || !strings.Contains(err.Error(), "afmeting") {
			t.Fatalf("width: %v", err)
		}
		rep = good()
		rep.Height = 4097
		if err := run(t, answerZip(t, rep, 0, 0)); err == nil {
			t.Fatal("height 4097 must be refused")
		}
		if err := run(t, answerZip(t, good(), 0, 5<<20)); err == nil || !strings.Contains(err.Error(), "report.json") {
			t.Fatalf("5 MB report: %v", err)
		}
	})
}

func TestBannerPreviewWatchBounds(t *testing.T) {
	type rec struct {
		mu    sync.Mutex
		added []string
		warns []string
	}
	start := func(t *testing.T, dir string, r *rec, maxDirs int, failOn string) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		opts := watchOptions{
			MaxDirs: maxDirs,
			Warn:    func(m string) { r.mu.Lock(); r.warns = append(r.warns, m); r.mu.Unlock() },
			AddError: func(p string) error {
				if failOn != "" && filepath.Base(p) == failOn {
					return errors.New("kapot")
				}
				r.mu.Lock()
				r.added = append(r.added, p)
				r.mu.Unlock()
				return nil
			},
		}
		go func() { watchBannerWith(ctx, dir, "", 50*time.Millisecond, func() {}, opts); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		time.Sleep(200 * time.Millisecond)
	}
	t.Run("ignored_dir_not_watched", func(t *testing.T) {
		dir := templateDir(t, map[string]string{".gitignore": "build/\ncache\n", "build/a/x.txt": "x", "cache/y.txt": "y", "src/z.txt": "z"})
		r := &rec{}
		start(t, dir, r, 0, "")
		r.mu.Lock()
		defer r.mu.Unlock()
		var sawSrc bool
		for _, p := range r.added {
			rel, _ := filepath.Rel(dir, p)
			if strings.HasPrefix(rel, "build") || strings.HasPrefix(rel, "cache") {
				t.Fatalf("watching ignored folder %s", rel)
			}
			if rel == "src" {
				sawSrc = true
			}
		}
		if !sawSrc {
			t.Fatalf("src must be watched: %v", r.added)
		}
	})
	t.Run("cap_warns", func(t *testing.T) {
		files := map[string]string{}
		for i := 0; i < 10; i++ {
			files[fmt.Sprintf("d%d/f.txt", i)] = "x"
		}
		dir := templateDir(t, files)
		r := &rec{}
		start(t, dir, r, 3, "")
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.added) != 3 {
			t.Fatalf("watched %d folders, cap is 3", len(r.added))
		}
		if len(r.warns) != 1 || !strings.Contains(r.warns[0], "meer dan 3 mappen") {
			t.Fatalf("warnings: %v", r.warns)
		}
	})
	t.Run("add_failure_reported", func(t *testing.T) {
		dir := templateDir(t, map[string]string{"sub/a.txt": "x", "ok/b.txt": "x"})
		r := &rec{}
		start(t, dir, r, 0, "sub")
		os.WriteFile(filepath.Join(dir, "sub", "n.txt"), []byte("x"), 0o644)
		time.Sleep(150 * time.Millisecond)
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.warns) != 1 || !strings.Contains(r.warns[0], "sub") || !strings.Contains(r.warns[0], "kan niet worden bewaakt") {
			t.Fatalf("warnings: %v", r.warns)
		}
	})
}

func TestBannerPreviewZipLimits(t *testing.T) {
	ctx := context.Background()
	t.Run("stops_at_32mb_without_reading_all", func(t *testing.T) {
		dir := t.TempDir()
		blob := make([]byte, 1<<20)
		const total = 80
		for i := 0; i < total; i++ {
			rand.Read(blob) // incompressible: output grows as fast as input
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.bin", i)), blob, 0o644)
		}
		var buf bytes.Buffer
		n, err := archive.CreateZipWithOptions(ctx, dir, &buf, archive.ZipOptions{MaxFiles: bannerMaxZipFiles, MaxBytes: bannerMaxZip})
		if !errors.Is(err, archive.ErrTooLarge) || !strings.Contains(err.Error(), "32 MB") {
			t.Fatalf("err: %v", err)
		}
		if n >= total-10 {
			t.Fatalf("read %d of %d files before stopping", n, total)
		}
		if buf.Len() > bannerMaxZip+(2<<20) {
			t.Fatalf("buffer grew to %d bytes", buf.Len())
		}
		if _, _, err := buildBannerRequest(ctx, dir, "k", nil); err == nil || !strings.Contains(err.Error(), "te groot") {
			t.Fatalf("buildBannerRequest: %v", err)
		}
	})
	t.Run("too_many_files", func(t *testing.T) {
		dir := t.TempDir()
		for i := 0; i < bannerMaxZipFiles+1; i++ {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), []byte("x"), 0o644)
		}
		_, _, err := buildBannerRequest(ctx, dir, "k", nil)
		if err == nil || !strings.Contains(err.Error(), "te veel bestanden") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("cancelled_fill_does_no_work", func(t *testing.T) {
		f := newFakeBanner(t)
		dir := templateDir(t, nil)
		c, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := f.source(t, dir).Fill(c); !errors.Is(err, context.Canceled) {
			t.Fatalf("err: %v", err)
		}
		if f.callCount() != 0 {
			t.Fatal("a request went out")
		}
		if _, err := os.Stat(filepath.Join(dir, ".taufinity")); err == nil {
			t.Fatal("a cancelled fill still did work (key file made)")
		}
	})
}

func TestBannerPreviewIsolation(t *testing.T) {
	entry := `<html><head></head><body>hoi</body></html>`
	files := map[string]string{"rows/0/index.html": entry}
	get := func(t *testing.T, url string) *http.Response {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	t.Run("banner_on_own_origin", func(t *testing.T) {
		srv, base, _ := startBanner(t, simpleReport(), files)
		if srv.addr == srv.bannerAddr || srv.BannerOrigin() == strings.TrimSuffix(srv.URL(), "/") {
			t.Fatal("page and banner share an origin")
		}
		if code, body := httpGet(t, srv.BannerOrigin()+"/rows/0/index.html"); code != 200 || !strings.Contains(body, "hoi") {
			t.Fatalf("banner origin: %d", code)
		}
		if code, _ := httpGet(t, base+"/rows/0/index.html"); code != 404 {
			t.Fatalf("the page origin serves banner files: %d", code)
		}
		if _, body := httpGet(t, base+"/api/state"); !strings.Contains(body, `"banner_origin":"`+srv.BannerOrigin()+`"`) {
			t.Fatalf("state: %s", body)
		}
		page := string(bannerPageHTML)
		if !strings.Contains(page, `sandbox="allow-scripts"`) || strings.Contains(page, "allow-same-origin") {
			t.Fatal("the iframe must be sandboxed with allow-scripts only")
		}
		_, body := httpGet(t, srv.BannerOrigin()+"/rows/0/index.html")
		if !strings.Contains(body, "taufinity-driver") {
			t.Fatal("the bridge is missing from the banner")
		}
	})
	t.Run("banner_origin_has_no_api", func(t *testing.T) {
		srv, _, _ := startBanner(t, simpleReport(), files)
		for _, p := range []string{"/api/state", "/events", "/", "/api/", "/rows/../api/state", "/preview_page.html"} {
			if code, body := httpGet(t, srv.BannerOrigin()+p); code == 200 {
				t.Fatalf("%s served on the banner origin: %q", p, body)
			}
		}
	})
	t.Run("nosniff_everywhere", func(t *testing.T) {
		srv, base, _ := startBanner(t, simpleReport(), files)
		for _, u := range []string{base + "/", base + "/api/state", base + "/nope", srv.BannerOrigin() + "/rows/0/index.html", srv.BannerOrigin() + "/rows/0/missing", srv.BannerOrigin() + "/api/state"} {
			if got := get(t, u).Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("%s: X-Content-Type-Options = %q", u, got)
			}
		}
	})
	t.Run("page_has_csp", func(t *testing.T) {
		srv, base, _ := startBanner(t, simpleReport(), files)
		csp := get(t, base+"/").Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "frame-src " + srv.BannerOrigin()} {
			if !strings.Contains(csp, want) {
				t.Fatalf("CSP %q lacks %q", csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("CSP allows unsafe code: %q", csp)
		}
		page := string(bannerPageHTML)
		if strings.Count(page, "<script") != 1 {
			t.Fatal("the page must hold exactly one inline script")
		}
		i := strings.Index(page, "<script>") + len("<script>")
		sum := sha256.Sum256([]byte(page[i : i+strings.Index(page[i:], "</script>")]))
		if !strings.Contains(csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
			t.Fatalf("CSP does not hash the page script: %q", csp)
		}
		if cb := get(t, srv.BannerOrigin()+"/rows/0/index.html").Header.Get("Content-Security-Policy"); !strings.Contains(cb, "frame-ancestors") {
			t.Fatalf("the banner may be framed by anyone: %q", cb)
		}
	})
}

func TestBannerPreviewTempHygiene(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	mk := func(t *testing.T, tmp, name, lock string, mtime time.Time) string {
		t.Helper()
		d := filepath.Join(tmp, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if lock != "" {
			os.WriteFile(filepath.Join(d, bannerLockName), []byte(lock), 0o600)
		}
		os.Chtimes(d, mtime, mtime)
		return d
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	t.Run("stale_dirs_removed", func(t *testing.T) {
		tmp := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Run() // finished: its pid is dead
		dead := fmt.Sprint(cmd.Process.Pid)
		a := mk(t, tmp, "taufinity-preview-aaa", "", old)
		b := mk(t, tmp, "taufinity-preview-bbb", dead, old)
		recent := mk(t, tmp, "taufinity-preview-new", "", time.Now())
		other := mk(t, tmp, "something-else", "", old)
		if n := cleanStaleBannerDirs(tmp, 24*time.Hour); n != 2 {
			t.Fatalf("removed %d", n)
		}
		if exists(a) || exists(b) {
			t.Fatal("stale folders remain")
		}
		if !exists(recent) || !exists(other) {
			t.Fatal("a recent or foreign folder was removed")
		}
	})
	t.Run("live_dir_kept", func(t *testing.T) {
		tmp := t.TempDir()
		live := mk(t, tmp, "taufinity-preview-live", fmt.Sprint(os.Getpid()), old)
		if n := cleanStaleBannerDirs(tmp, 24*time.Hour); n != 0 || !exists(live) {
			t.Fatalf("a folder in use was removed (n=%d)", n)
		}
		d, err := newBannerTempDir()
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(d)
		if !exists(filepath.Join(d, bannerLockName)) {
			t.Fatal("no lock file in a new temp folder")
		}
	})
	t.Run("temp_inside_template_refused", func(t *testing.T) {
		dir := t.TempDir()
		inside := filepath.Join(dir, "tmp")
		os.MkdirAll(inside, 0o755)
		t.Setenv("TMPDIR", inside)
		err := checkTempOutsideTemplate(dir)
		if err == nil || !strings.Contains(err.Error(), "tijdelijke map") {
			t.Fatalf("err: %v", err)
		}
		t.Setenv("TMPDIR", t.TempDir())
		if err := checkTempOutsideTemplate(dir); err != nil {
			t.Fatalf("a separate temp folder must be fine: %v", err)
		}
	})
}
