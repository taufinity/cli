package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/api"
)

// tinyPNG is a real 2x2 PNG, so a browser can decode it.
var tinyPNG = func() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}()

// fakeRenderStudio records the real-render requests of the contract.
type fakeRenderStudio struct {
	srv *httptest.Server
	mu  sync.Mutex

	uploads     int
	uploadFiles []string
	uploadField string
	renders     []map[string]any
	downloads   int
	auths       []string

	uploadStatus  int
	uploadBody    string
	renderStatus  int
	renderBody    string
	renderStatus2 string // "status" in the 200 answer, default succeeded
	notes         string
	hold          chan struct{} // render waits for this to be closed
	entered       chan struct{} // signalled when a render arrives
}

func newFakeRenderStudio(t *testing.T) *fakeRenderStudio {
	f := &fakeRenderStudio{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRenderStudio) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()
	switch {
	case r.URL.Path == renderUploadEndpoint && r.Method == http.MethodPost:
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		var files []string
		field := ""
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(p)
			field = p.FormName()
			if gz, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
				tr := tar.NewReader(gz)
				for {
					h, err := tr.Next()
					if err != nil {
						break
					}
					files = append(files, h.Name)
				}
			}
		}
		sort.Strings(files)
		f.mu.Lock()
		f.uploads++
		f.uploadFiles, f.uploadField = files, field
		st, body := f.uploadStatus, f.uploadBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if st != 0 {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"archive_id":"tpl_abc","expires_at":"2026-11-20T12:00:00Z","file_count":2,"size":10}`))
	case r.URL.Path == renderEndpoint && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.renders = append(f.renders, body)
		st, rb, status, notes := f.renderStatus, f.renderBody, f.renderStatus2, f.notes
		hold, entered := f.hold, f.entered
		f.mu.Unlock()
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if hold != nil {
			<-hold
		}
		w.Header().Set("Content-Type", "application/json")
		if st != 0 {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(rb))
			return
		}
		if status == "" {
			status = "succeeded"
		}
		format, _ := body["format"].(string)
		resp := map[string]any{"job_id": "job1", "trace_id": "tr1", "format": format, "status": status, "notes": notes, "bytes": 12345}
		if status == "succeeded" {
			resp["download_url"] = "/api/creatives/tpl_abc/job1/file"
		}
		_ = json.NewEncoder(w).Encode(resp)
	case r.URL.Path == "/api/creatives/tpl_abc/job1/file" && r.Method == http.MethodGet:
		f.mu.Lock()
		f.downloads++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRenderStudio) client() *api.Client {
	c := api.NewPreview(f.srv.URL)
	c.SetAuth("test-token")
	return c
}

func (f *fakeRenderStudio) renderer(dir string) *bannerRenderer {
	c := api.NewRender(f.srv.URL, bannerRenderClientTimeout) // the client the button really uses
	c.SetAuth("test-token")
	return &bannerRenderer{Client: c, Dir: dir}
}

func renderReport() *bannerReport {
	return &bannerReport{Entry: "index.html", Width: 300, Height: 250, Rows: []bannerRow{
		{Index: 0, Label: "voorbeeld", Values: map[string]string{"titel": "Koude Haven"}},
	}}
}

func TestPreviewRenderUpload(t *testing.T) {
	files := map[string]string{
		"index.html":              "<html></html>",
		"css/a.css":               "a{}",
		".gitignore":              "ignored.txt\n",
		"ignored.txt":             "x",
		".taufinityignore":        "skip.txt\n",
		"skip.txt":                "x",
		".env":                    "SECRET=1",
		".taufinity/preview.json": `{"template_key":"00000000-0000-4000-8000-000000000000"}`,
	}
	t.Run("same_files_as_preview", func(t *testing.T) {
		dir := templateDir(t, files)
		if err := os.Symlink("index.html", filepath.Join(dir, "link.html")); err != nil {
			t.Fatal(err)
		}
		fs := newFakeRenderStudio(t)
		id, err := fs.renderer(dir).Upload(context.Background())
		if err != nil || id != "tpl_abc" {
			t.Fatalf("id=%q err=%v", id, err)
		}
		// the files the preview zips
		fb := newFakeBanner(t)
		if _, err := fb.source(t, dir).Fill(context.Background()); err != nil {
			t.Fatal(err)
		}
		zipped := append([]string(nil), fb.zipNames...)
		sort.Strings(zipped)
		if strings.Join(zipped, ",") != strings.Join(fs.uploadFiles, ",") {
			t.Fatalf("tar %v != zip %v", fs.uploadFiles, zipped)
		}
		if fs.uploadField != "archive" {
			t.Fatalf("field = %q", fs.uploadField)
		}
		if strings.Join(fs.uploadFiles, ",") != ".gitignore,.taufinityignore,css/a.css,index.html" {
			t.Fatalf("files = %v", fs.uploadFiles)
		}
	})
	t.Run("secrets_excluded", func(t *testing.T) {
		dir := templateDir(t, map[string]string{
			"index.html": "x", ".env": "A=1", "credentials.json": "{}", ".npmrc": "t", "id_rsa": "k",
			"a.tfvars": "x", ".taufinity/preview.json": "{}", ".aws/credentials": "x",
		})
		fs := newFakeRenderStudio(t)
		if _, err := fs.renderer(dir).Upload(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, n := range fs.uploadFiles {
			if n != "index.html" {
				t.Errorf("unexpected file in archive: %s", n)
			}
		}
		if len(fs.uploadFiles) != 1 {
			t.Fatalf("files = %v", fs.uploadFiles)
		}
	})
	t.Run("upload_error_shown", func(t *testing.T) {
		dir := templateDir(t, nil)
		fs := newFakeRenderStudio(t)
		fs.uploadStatus, fs.uploadBody = 413, `{"error":"archief is groter dan 32 MB"}`
		_, err := fs.renderer(dir).Upload(context.Background())
		if err == nil || !strings.Contains(err.Error(), "archief is groter dan 32 MB") || !strings.Contains(err.Error(), "uploaden") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPreviewRenderRequest(t *testing.T) {
	ctx := context.Background()
	t.Run("png_body", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		dir := templateDir(t, nil)
		out := filepath.Join(t.TempDir(), "r.png")
		res, err := fs.renderer(dir).Render(ctx, renderReport(), 0, "png", out)
		if err != nil {
			t.Fatal(err)
		}
		b := fs.renders[0]
		if b["archive_id"] != "tpl_abc" || b["width"] != float64(300) || b["height"] != float64(250) || b["format"] != "png" {
			t.Fatalf("body = %v", b)
		}
		if _, ok := b["frames"]; ok {
			t.Fatalf("png has frames: %v", b)
		}
		data, _ := os.ReadFile(out)
		if !bytes.Equal(data, tinyPNG) || res.File != out || res.Status != "succeeded" || fs.downloads != 1 {
			t.Fatalf("res=%+v data=%q downloads=%d", res, data, fs.downloads)
		}
		for _, a := range fs.auths {
			if a != "Bearer test-token" {
				t.Fatalf("auth = %q", a)
			}
		}
	})
	t.Run("gif_body_has_10_frames", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		_, err := fs.renderer(templateDir(t, nil)).Render(ctx, renderReport(), 0, "gif", filepath.Join(t.TempDir(), "r.gif"))
		if err != nil {
			t.Fatal(err)
		}
		if fs.renders[0]["format"] != "gif" || fs.renders[0]["frames"] != float64(10) {
			t.Fatalf("body = %v", fs.renders[0])
		}
	})
	t.Run("variables_from_current_row", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		dir := templateDir(t, map[string]string{"data.csv": "titel;prijs\nKoude Haven;9,95\nWarme Haven;12\n"})
		rep := renderReport()
		rep.Rows = append(rep.Rows, bannerRow{Index: 1, Label: "regel 3"})
		if _, err := fs.renderer(dir).Render(ctx, rep, 1, "png", filepath.Join(t.TempDir(), "r.png")); err != nil {
			t.Fatal(err)
		}
		vars := fs.renders[0]["variables"].(map[string]any)
		if len(vars) != 2 || vars["titel"] != "Warme Haven" || vars["prijs"] != "12" {
			t.Fatalf("variables = %v", vars)
		}
	})
	t.Run("refused_422_shows_server_text", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		fs.renderStatus, fs.renderBody = 422, `{"error":"banner geweigerd: tekst past niet"}`
		_, err := fs.renderer(templateDir(t, nil)).Render(ctx, renderReport(), 0, "png", filepath.Join(t.TempDir(), "r.png"))
		var re *renderError
		if !errors.As(err, &re) || re.Status != 422 || !strings.Contains(err.Error(), "banner geweigerd: tekst past niet") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not_achieved_reported", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		fs.renderStatus2, fs.notes = "not_achieved", "logo te klein"
		res, err := fs.renderer(templateDir(t, nil)).Render(ctx, renderReport(), 0, "png", filepath.Join(t.TempDir(), "r.png"))
		if err != nil || res.Status != "not_achieved" || res.Notes != "logo te klein" || res.File != "" || fs.downloads != 0 {
			t.Fatalf("res=%+v err=%v downloads=%d", res, err, fs.downloads)
		}
	})
}

// startRenderServer is a page server whose Renderer talks to the fake Studio.
func startRenderServer(t *testing.T, fs *fakeRenderStudio) (*bannerServer, string) {
	t.Helper()
	dir := templateDir(t, map[string]string{"index.html": "<html><body>hoi</body></html>"})
	s, base, _ := startBanner(t, renderReport(), map[string]string{"rows/0/index.html": "<html></html>"})
	s.Renderer = fs.renderer(dir)
	return s, base
}

func postRender(t *testing.T, base, token, origin, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/render", strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Preview-Token", token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPreviewRenderEndpointGuards(t *testing.T) {
	const ok = `{"row":0,"format":"png"}`
	t.Run("wrong_method", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		s, base := startRenderServer(t, fs)
		for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			req, _ := http.NewRequest(m, base+"/api/render", nil)
			req.Header.Set("X-Preview-Token", s.token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 405 {
				t.Errorf("%s: %d", m, resp.StatusCode)
			}
		}
		if fs.uploads != 0 {
			t.Fatal("studio was called")
		}
	})
	t.Run("missing_token", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		_, base := startRenderServer(t, fs)
		if code, _ := postRender(t, base, "", "", ok); code != 403 {
			t.Fatalf("code = %d", code)
		}
		if fs.uploads != 0 {
			t.Fatal("studio was called")
		}
	})
	t.Run("wrong_token", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		s, base := startRenderServer(t, fs)
		if code, _ := postRender(t, base, s.token+"x", "", ok); code != 403 {
			t.Fatalf("code = %d", code)
		}
		if code, _ := postRender(t, base, "x", "", ok); code != 403 {
			t.Fatalf("code = %d", code)
		}
		if fs.uploads != 0 {
			t.Fatal("studio was called")
		}
	})
	t.Run("foreign_origin", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		s, base := startRenderServer(t, fs)
		for _, o := range []string{"http://evil.example", "null", s.BannerOrigin()} {
			if code, _ := postRender(t, base, s.token, o, ok); code != 403 {
				t.Errorf("origin %q: code = %d", o, code)
			}
		}
		if fs.uploads != 0 {
			t.Fatal("studio was called")
		}
		if code, body := postRender(t, base, s.token, base, ok); code != 200 {
			t.Fatalf("own origin: %d %s", code, body)
		}
	})
	t.Run("one_at_a_time", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		fs.hold = make(chan struct{})
		fs.entered = make(chan struct{}, 1)
		s, base := startRenderServer(t, fs)
		done := make(chan int, 1)
		go func() { c, _ := postRender(t, base, s.token, "", ok); done <- c }()
		select {
		case <-fs.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("first render never reached studio")
		}
		code, body := postRender(t, base, s.token, "", ok)
		if code != 409 || !strings.Contains(body, "er loopt al een echte render") {
			t.Fatalf("second: %d %s", code, body)
		}
		close(fs.hold)
		if c := <-done; c != 200 {
			t.Fatalf("first: %d", c)
		}
		if code, _ := postRender(t, base, s.token, "", ok); code != 200 {
			t.Fatalf("after: %d", code)
		}
	})
}

func TestPreviewRenderTokenOnlyInPage(t *testing.T) {
	t.Run("page_has_token", func(t *testing.T) {
		s, base, _ := startBanner(t, simpleReport(), map[string]string{"rows/0/index.html": "<html></html>"})
		_, page := httpGet(t, base+"/")
		if !strings.Contains(page, `name="preview-token" content="`+s.token+`"`) || strings.Contains(page, renderTokenPlaceholder) {
			t.Fatal("page lacks the token")
		}
		_, page2 := httpGet(t, base+"/")
		if !strings.Contains(page2, s.token) {
			t.Fatal("second load lacks the token")
		}
		if _, st := httpGet(t, base+"/api/state"); strings.Contains(st, s.token) {
			t.Fatal("state leaks the token")
		}
		if s2 := newBannerServer(); s2.token == s.token || len(s.token) < 32 {
			t.Fatal("token not random per process")
		}
	})
	t.Run("banner_origin_has_no_token", func(t *testing.T) {
		s, _, _ := startBanner(t, simpleReport(), map[string]string{"rows/0/index.html": "<html><head></head><body>x</body></html>"})
		bo := s.BannerOrigin()
		for _, p := range []string{"/rows/0/index.html", "/", "/api/state", "/api/render", "/render/0"} {
			_, body := httpGet(t, bo+p)
			if strings.Contains(body, s.token) {
				t.Errorf("%s leaks the token", p)
			}
		}
		// and the banner origin cannot start a render
		req, _ := http.NewRequest(http.MethodPost, bo+"/api/render", strings.NewReader(`{"row":0,"format":"png"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("banner origin /api/render = %d", resp.StatusCode)
		}
	})
}

func TestPreviewRenderResultServed(t *testing.T) {
	fs := newFakeRenderStudio(t)
	s, base := startRenderServer(t, fs)
	get := func(path string) (*http.Response, []byte) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	render := func(format string) renderResponse {
		code, body := postRender(t, base, s.token, "", `{"row":0,"format":"`+format+`"}`)
		if code != 200 {
			t.Fatalf("render: %d %s", code, body)
		}
		var rr renderResponse
		_ = json.Unmarshal([]byte(body), &rr)
		return rr
	}
	t.Run("png_served", func(t *testing.T) {
		rr := render("png")
		resp, b := get(rr.URL)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(b, tinyPNG) {
			t.Fatalf("%d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), b)
		}
		if rr.Bytes != 12345 || rr.Status != "succeeded" {
			t.Fatalf("rr = %+v", rr)
		}
	})
	t.Run("gif_served", func(t *testing.T) {
		rr := render("gif")
		resp, _ := get(rr.URL)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/gif" {
			t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	})
	t.Run("unknown_404", func(t *testing.T) {
		for _, p := range []string{"/render/99", "/render/abc", "/render/-1", "/render/"} {
			if resp, _ := get(p); resp.StatusCode != 404 {
				t.Errorf("%s: %d", p, resp.StatusCode)
			}
		}
	})
}
