package commands

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/api"
)

// fakeBanner is an httptest server speaking the banner-preview contract.
type fakeBanner struct {
	srv      *httptest.Server
	mu       sync.Mutex
	zipNames []string
	rows     []map[string]string
	gotRows  bool
	calls    int32
	status   int // non-zero: answer this status instead
	body     string
	report   bannerReport
	html     string
}

func newFakeBanner(t *testing.T) *fakeBanner {
	f := &fakeBanner{
		report: bannerReport{
			Entry: "index.html", Width: 300, Height: 250,
			Fields: []bannerName{"titel", "logo_url", "prijs"},
			Rows: []bannerRow{
				{Index: 0, Label: "voorbeeld", Values: map[string]string{"titel": "Hallo", "prijs": "9,95"}},
				{Index: 1, Label: "leeg"},
			},
		},
		html: "<html><head><title>x</title></head><body>hoi</body></html>",
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBanner) handle(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&f.calls, 1)
	if r.URL.Path != bannerEndpoint || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "geen toegang", 401)
		return
	}
	_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	mr := multipart.NewReader(r.Body, params["boundary"])
	f.mu.Lock()
	f.zipNames, f.rows, f.gotRows = nil, nil, false
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(p)
		switch p.FormName() {
		case "template":
			zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err == nil {
				for _, zf := range zr.File {
					f.zipNames = append(f.zipNames, zf.Name)
				}
			}
		case "rows":
			f.gotRows = true
			_ = json.Unmarshal(data, &f.rows)
		}
	}
	status, body, rep, html := f.status, f.body, f.report, f.html
	f.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	js, _ := json.Marshal(rep)
	e, _ := zw.Create("report.json")
	e.Write(js)
	for _, row := range rep.Rows {
		e, _ := zw.Create("rows/" + itoa(row.Index) + "/" + rep.Entry)
		e.Write([]byte(html))
	}
	zw.Close()
	w.Header().Set("Content-Type", "application/zip")
	_, _ = w.Write(buf.Bytes())
}

func (f *fakeBanner) source(t *testing.T, dir string) *bannerSource {
	c := api.NewNoRateLimitRetry(f.srv.URL)
	c.SetAuth("test-token")
	return &bannerSource{Client: c, Dir: dir}
}

func templateDir(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	if files == nil {
		files = map[string]string{}
	}
	if _, ok := files["index.html"]; !ok {
		files["index.html"] = "<html></html>"
	}
	for n, c := range files {
		p := filepath.Join(dir, filepath.FromSlash(n))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBannerPreviewRequest(t *testing.T) {
	ctx := context.Background()

	t.Run("sends_zip_and_rows", func(t *testing.T) {
		f := newFakeBanner(t)
		dir := templateDir(t, map[string]string{"style.css": "a{}", "data.csv": "titel,prijs\nA,1\nB,2\n"})
		res, err := f.source(t, dir).Fill(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(res.Dir)
		if !f.gotRows || len(f.rows) != 2 || f.rows[1]["titel"] != "B" || f.rows[0]["prijs"] != "1" {
			t.Fatalf("rows: %v", f.rows)
		}
		has := map[string]bool{}
		for _, n := range f.zipNames {
			has[n] = true
		}
		if !has["index.html"] || !has["style.css"] {
			t.Fatalf("zip: %v", f.zipNames)
		}
		if _, err := os.Stat(filepath.Join(res.Dir, "rows", "0", "index.html")); err != nil {
			t.Fatal(err)
		}
		if res.Report.Width != 300 || len(res.Report.Rows) != 2 {
			t.Fatalf("report: %+v", res.Report)
		}
	})

	t.Run("semicolon_csv", func(t *testing.T) {
		f := newFakeBanner(t)
		dir := templateDir(t, map[string]string{"d.csv": "\xef\xbb\xbftitel;prijs\nHoi, wereld;\"1,50\"\n"})
		res, err := f.source(t, dir).Fill(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(res.Dir)
		if len(f.rows) != 1 || f.rows[0]["titel"] != "Hoi, wereld" || f.rows[0]["prijs"] != "1,50" {
			t.Fatalf("rows: %v", f.rows)
		}
	})

	t.Run("row_flag_picks_line", func(t *testing.T) {
		f := newFakeBanner(t)
		dir := templateDir(t, map[string]string{"data/d.csv": "titel\nA\nB\nC\n"})
		src := f.source(t, dir)
		src.Row = 3 // header is line 1, so line 3 is the second data row
		res, err := src.Fill(ctx)
		if err != nil {
			t.Fatal(err)
		}
		os.RemoveAll(res.Dir)
		if len(f.rows) != 1 || f.rows[0]["titel"] != "B" {
			t.Fatalf("rows: %v", f.rows)
		}
		src.Row = 1
		if _, err := src.Fill(ctx); err == nil {
			t.Fatal("line 1 is the header and must be refused")
		}
		src.Row = 9
		if _, err := src.Fill(ctx); err == nil || !strings.Contains(err.Error(), "bestaat niet") {
			t.Fatalf("err: %v", err)
		}
	})

	t.Run("no_csv_sends_no_rows", func(t *testing.T) {
		f := newFakeBanner(t)
		res, err := f.source(t, templateDir(t, nil)).Fill(ctx)
		if err != nil {
			t.Fatal(err)
		}
		os.RemoveAll(res.Dir)
		if f.gotRows {
			t.Fatal("rows part must be omitted")
		}
	})

	t.Run("two_csvs_asks_for_data", func(t *testing.T) {
		f := newFakeBanner(t)
		dir := templateDir(t, map[string]string{"a.csv": "x\n1\n", "data/b.csv": "x\n2\n"})
		_, err := f.source(t, dir).Fill(ctx)
		if err == nil || !strings.Contains(err.Error(), "--data") || !strings.Contains(err.Error(), "a.csv") || !strings.Contains(err.Error(), "b.csv") {
			t.Fatalf("err: %v", err)
		}
		if atomic.LoadInt32(&f.calls) != 0 {
			t.Fatal("nothing may be sent")
		}
		src := f.source(t, dir)
		src.DataFlag = filepath.Join(dir, "data", "b.csv")
		res, err := src.Fill(ctx)
		if err != nil {
			t.Fatal(err)
		}
		os.RemoveAll(res.Dir)
		if f.rows[0]["x"] != "2" {
			t.Fatalf("rows: %v", f.rows)
		}
	})

	t.Run("too_many_rows", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString("x\n")
		for i := 0; i < 51; i++ {
			sb.WriteString("a\n")
		}
		d, _ := parseBannerCSV([]byte(sb.String()))
		if _, err := selectBannerRows(d, 0); err == nil || !strings.Contains(err.Error(), "50") {
			t.Fatalf("err: %v", err)
		}
	})

	t.Run("http_errors_are_dutch", func(t *testing.T) {
		f := newFakeBanner(t)
		dir := templateDir(t, nil)
		f.status, f.body = 429, `{"error":"Te veel voorbeelden, wacht even."}`
		if _, err := f.source(t, dir).Fill(ctx); err == nil || err.Error() != "Te veel voorbeelden, wacht even." {
			t.Fatalf("429: %v", err)
		}
		f.status, f.body = 413, `{"error":"too big"}`
		if _, err := f.source(t, dir).Fill(ctx); err == nil || !strings.Contains(err.Error(), "32 MB") {
			t.Fatalf("413: %v", err)
		}
		f.status, f.body = 400, `{"error":"geen index.html"}`
		if _, err := f.source(t, dir).Fill(ctx); err == nil || !strings.Contains(err.Error(), "geen index.html") {
			t.Fatalf("400: %v", err)
		}
	})
}

// startBanner serves a fill and returns the base URL.
func startBanner(t *testing.T, rep *bannerReport, files map[string]string) (*bannerServer, string, string) {
	t.Helper()
	dir := t.TempDir()
	for n, c := range files {
		p := filepath.Join(dir, filepath.FromSlash(n))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(c), 0o644)
	}
	s := newBannerServer()
	ln, err := s.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	s.Swap(&bannerResult{Dir: dir, Report: rep})
	return s, strings.TrimSuffix(s.URL(), "/"), dir
}

func simpleReport() *bannerReport {
	return &bannerReport{Entry: "index.html", Width: 300, Height: 250, Rows: []bannerRow{{Index: 0, Label: "voorbeeld"}}}
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestBannerPreviewServer(t *testing.T) {
	t.Run("serves_page_and_files", func(t *testing.T) {
		_, base, _ := startBanner(t, simpleReport(), map[string]string{
			"rows/0/index.html": "<html><head></head><body>hoi</body></html>",
			"rows/0/a/b.css":    "b{}",
		})
		if code, body := httpGet(t, base+"/"); code != 200 || !strings.Contains(body, "row-picker") {
			t.Fatalf("page: %d", code)
		}
		if code, body := httpGet(t, base+"/rows/0/a/b.css"); code != 200 || body != "b{}" {
			t.Fatalf("file: %d %q", code, body)
		}
		if code, _ := httpGet(t, base+"/rows/0/nope.css"); code != 404 {
			t.Fatalf("missing: %d", code)
		}
		if code, _ := httpGet(t, base+"/rows/../../etc/passwd"); code == 200 {
			t.Fatal("path escape")
		}
		code, body := httpGet(t, base+"/api/state")
		if code != 200 || !strings.Contains(body, `"entry":"index.html"`) {
			t.Fatalf("state: %d %s", code, body)
		}
	})

	t.Run("binds_localhost_only", func(t *testing.T) {
		s, _, _ := startBanner(t, simpleReport(), map[string]string{"rows/0/index.html": "x"})
		host, _, _ := net.SplitHostPort(s.addr)
		if host != "127.0.0.1" {
			t.Fatalf("bound to %s", host)
		}
		req, _ := http.NewRequest("GET", "http://"+s.addr+"/", nil)
		req.Host = "evil.example.com"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("foreign host: %d", resp.StatusCode)
		}
	})

	t.Run("ready_hook_only_with_meta_tag", func(t *testing.T) {
		plain := "<html><head><title>x</title></head><body></body></html>"
		withMeta := `<html><head><meta content="signal" name="taufinity.ready"><title>x</title></head><body></body></html>`
		notSignal := `<html><head><meta name="taufinity.ready" content="other"></head></html>`
		_, base, _ := startBanner(t, simpleReport(), map[string]string{
			"rows/0/index.html": withMeta, "rows/0/other.html": withMeta,
		})
		_, got := httpGet(t, base+"/rows/0/index.html")
		if !strings.Contains(got, "window.__taufinityReady") {
			t.Fatal("hook missing")
		}
		if !strings.HasPrefix(got, "<html><head>"+bannerReadyHook) {
			t.Fatalf("hook not right after <head>: %s", got)
		}
		if strings.Count(got, "<script") != 1 {
			t.Fatal("only one script")
		}
		if _, other := httpGet(t, base+"/rows/0/other.html"); other != withMeta {
			t.Fatal("only the entry file gets the hook")
		}
		if string(injectReadyHook([]byte(plain))) != plain || string(injectReadyHook([]byte(notSignal))) != notSignal {
			t.Fatal("no meta, no hook")
		}
	})

	t.Run("files_on_disk_unchanged", func(t *testing.T) {
		withMeta := `<html><head><meta name="taufinity.ready" content="signal"></head></html>`
		_, base, dir := startBanner(t, simpleReport(), map[string]string{"rows/0/index.html": withMeta})
		httpGet(t, base+"/rows/0/index.html")
		b, _ := os.ReadFile(filepath.Join(dir, "rows", "0", "index.html"))
		if string(b) != withMeta {
			t.Fatalf("file on disk changed: %s", b)
		}
	})
}

func TestBannerPreviewPage(t *testing.T) {
	page := string(bannerPageHTML)
	for name, id := range map[string]string{
		"has_row_picker":    "row-picker",
		"has_overview":      "overview",
		"has_field_report":  "field-report",
		"has_ready_badge":   "ready-badge",
		"has_error_overlay": "error-overlay",
	} {
		id := id
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(page, `id="`+id+`"`) {
				t.Fatalf("page has no element with id %q", id)
			}
		})
	}
	t.Run("no_external_requests", func(t *testing.T) {
		for _, bad := range []string{`src="http`, `href="http`, `src='http`, "url(http", "@import"} {
			if strings.Contains(page, bad) {
				t.Fatalf("external reference %q", bad)
			}
		}
	})
	t.Run("scrubber_driver_order", func(t *testing.T) {
		seek := strings.Index(page, "__taufinitySeek")
		gs := strings.Index(page, "gsap")
		if seek < 0 || gs < 0 || seek > gs {
			t.Fatalf("__taufinitySeek must come before gsap (%d, %d)", seek, gs)
		}
		if !strings.Contains(page, "geen tijdlijn: als GIF of video wordt deze banner geweigerd") {
			t.Fatal("missing note")
		}
		if !strings.Contains(page, "__taufinityDuration") {
			t.Fatal("duration hook missing")
		}
	})
}

func TestBannerPreviewInit(t *testing.T) {
	ctx := context.Background()
	t.Run("writes_header_and_row", func(t *testing.T) {
		f := newFakeBanner(t)
		dir := templateDir(t, nil)
		if err := runBannerInit(ctx, f.source(t, dir), false); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "voorbeeld.csv"))
		if err != nil {
			t.Fatal(err)
		}
		want := "titel;logo_url;prijs\nHallo;;9,95\n"
		if string(b) != want {
			t.Fatalf("got %q want %q", b, want)
		}
		if f.gotRows {
			t.Fatal("init sends no rows")
		}
	})
	t.Run("placeholders_without_values", func(t *testing.T) {
		rep := &bannerReport{Fields: []bannerName{"titel", "bestand_file"}, Rows: []bannerRow{{Label: "voorbeeld"}}}
		b, _ := bannerInitCSV(rep)
		if string(b) != "titel;bestand_file\n‹titel›;\n" {
			t.Fatalf("got %q", b)
		}
	})
	t.Run("refuses_overwrite", func(t *testing.T) {
		f := newFakeBanner(t)
		dir := templateDir(t, map[string]string{"voorbeeld.csv": "mijn;data\n"})
		err := runBannerInit(ctx, f.source(t, dir), false)
		if err == nil || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("err: %v", err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "voorbeeld.csv"))
		if string(b) != "mijn;data\n" {
			t.Fatal("file was overwritten")
		}
		if err := runBannerInit(ctx, f.source(t, dir), true); err != nil {
			t.Fatal(err)
		}
		b, _ = os.ReadFile(filepath.Join(dir, "voorbeeld.csv"))
		if !strings.HasPrefix(string(b), "titel;") {
			t.Fatalf("force did not overwrite: %q", b)
		}
	})
}

func startWatch(t *testing.T, dir, data string, onChange func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchBanner(ctx, dir, data, 100*time.Millisecond, onChange); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(150 * time.Millisecond) // watcher armed
}

func TestBannerPreviewWatch(t *testing.T) {
	t.Run("burst_of_five_writes_refills_once", func(t *testing.T) {
		dir := templateDir(t, nil)
		var n int32
		startWatch(t, dir, "", func() { atomic.AddInt32(&n, 1) })
		for i := 0; i < 5; i++ {
			os.WriteFile(filepath.Join(dir, "index.html"), []byte{byte('a' + i)}, 0o644)
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
		if got := atomic.LoadInt32(&n); got != 1 {
			t.Fatalf("refills: %d", got)
		}
	})

	t.Run("editor_temp_files_ignored", func(t *testing.T) {
		dir := templateDir(t, nil)
		var n int32
		startWatch(t, dir, "", func() { atomic.AddInt32(&n, 1) })
		for _, name := range []string{".index.html.swp", "index.html~", ".DS_Store", "4913", ".#index.html"} {
			os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
		}
		time.Sleep(400 * time.Millisecond)
		if got := atomic.LoadInt32(&n); got != 0 {
			t.Fatalf("refills: %d", got)
		}
		os.WriteFile(filepath.Join(dir, "style.css"), []byte("x"), 0o644)
		time.Sleep(400 * time.Millisecond)
		if got := atomic.LoadInt32(&n); got != 1 {
			t.Fatalf("a real file must refill, got %d", got)
		}
	})

	t.Run("data_file_outside_folder_is_watched", func(t *testing.T) {
		dir := templateDir(t, nil)
		data := filepath.Join(t.TempDir(), "d.csv")
		os.WriteFile(data, []byte("a\n1\n"), 0o644)
		var n int32
		startWatch(t, dir, data, func() { atomic.AddInt32(&n, 1) })
		os.WriteFile(data, []byte("a\n2\n"), 0o644)
		time.Sleep(400 * time.Millisecond)
		if got := atomic.LoadInt32(&n); got != 1 {
			t.Fatalf("refills: %d", got)
		}
	})

	t.Run("failed_refill_keeps_last_good", func(t *testing.T) {
		f := newFakeBanner(t)
		dir := templateDir(t, nil)
		src := f.source(t, dir)
		srv := newBannerServer()
		ln, _ := srv.Listen(0)
		go srv.Serve(ln)
		defer srv.Close()
		var lines []string
		sess := &bannerSession{Fill: src.Fill, Srv: srv, Warn: func(m string) { lines = append(lines, m) }}
		if err := sess.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.status, f.body = 400, `{"error":"kapot"}`
		f.mu.Unlock()
		if err := sess.Refresh(context.Background()); err == nil {
			t.Fatal("expected failure")
		}
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "Vernieuwen mislukt") || !strings.Contains(lines[0], "kapot") {
			t.Fatalf("lines: %v", lines)
		}
		st := srv.state()
		if st.Failure == "" || st.Report == nil {
			t.Fatalf("state: %+v", st)
		}
		if code, _ := httpGet(t, "http://"+srv.addr+"/rows/0/index.html"); code != 200 {
			t.Fatalf("last good banner gone: %d", code)
		}
		f.mu.Lock()
		f.status = 0
		f.mu.Unlock()
		if err := sess.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if srv.state().Failure != "" {
			t.Fatal("failure must clear after a good refill")
		}
	})

	t.Run("reload_event_sent", func(t *testing.T) {
		srv, base, dir := startBanner(t, simpleReport(), map[string]string{"rows/0/index.html": "x"})
		_ = dir
		resp, err := http.Get(base + "/events")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("content type %s", ct)
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Scan() // ": verbonden"
		next := t.TempDir()
		os.MkdirAll(filepath.Join(next, "rows", "0"), 0o755)
		os.WriteFile(filepath.Join(next, "rows", "0", "index.html"), []byte("y"), 0o644)
		srv.Swap(&bannerResult{Dir: next, Report: simpleReport()})
		got := make(chan string, 1)
		go func() {
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "event:") {
					got <- sc.Text()
					return
				}
			}
		}()
		select {
		case l := <-got:
			if l != "event: reload" {
				t.Fatalf("got %q", l)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no reload event")
		}
		if _, body := httpGet(t, base+"/rows/0/index.html"); body != "y" {
			t.Fatalf("server did not switch folders: %q", body)
		}
	})
}
