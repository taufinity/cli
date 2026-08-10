package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// knowledgeTestServer captures every POST /api/admin/knowledge-files/upsert
// call, returning a scripted action (created/updated/noop) per call in order.
type knowledgeTestServer struct {
	mu      sync.Mutex
	calls   []map[string]any
	actions []string // one per expected call, defaults to "created" if exhausted
}

func (s *knowledgeTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/knowledge-files/upsert" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)

		s.mu.Lock()
		idx := len(s.calls)
		s.calls = append(s.calls, body)
		action := "created"
		if idx < len(s.actions) {
			action = s.actions[idx]
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":        idx + 1,
			"uuid":      "test-uuid",
			"name":      body["title"],
			"file_type": body["file_type"],
			"action":    action,
			"checksum":  "abc123",
		}
		respBytes, _ := json.Marshal(resp)
		_, _ = w.Write(respBytes)
	})
}

func TestProvisionKnowledgeBase_SingleFile_ContentPath(t *testing.T) {
	srv := &knowledgeTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prijslijst-leenen.json"), []byte(`{"price":100}`), 0o644); err != nil {
		t.Fatalf("write content file: %v", err)
	}
	yaml := `name: prijslijst-leenen.json
file_type: price_list
purpose: reference
tags: [wvs-prijslijst, leverancier-leenen]
content_path: ./prijslijst-leenen.json
`
	if err := os.WriteFile(filepath.Join(dir, "leenen.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := provisionKnowledgeBase(c, dir, 12); err != nil {
		t.Fatalf("provisionKnowledgeBase: %v", err)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("want 1 upsert call, got %d", len(srv.calls))
	}
	got := srv.calls[0]
	if got["title"] != "prijslijst-leenen.json" {
		t.Errorf("title: %+v", got)
	}
	if got["content"] != `{"price":100}` {
		t.Errorf("content not loaded from content_path: %+v", got)
	}
	if got["file_type"] != "price_list" {
		t.Errorf("file_type: %+v", got)
	}
}

func TestProvisionKnowledgeBase_MalformedYAML_ReturnsError_NotExit(t *testing.T) {
	srv := &knowledgeTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte(`name: "unclosed`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	err := provisionKnowledgeBase(c, dir, 12)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse, got: %v", err)
	}
}

func TestValidateKnowledgeConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     knowledgeFileConfig
		wantErr string
	}{
		{
			name:    "missing name (non-glob)",
			cfg:     knowledgeFileConfig{Content: "x"},
			wantErr: "name is required",
		},
		{
			name:    "both content and content_path",
			cfg:     knowledgeFileConfig{Name: "n", Content: "x", ContentPath: "./x"},
			wantErr: "not both",
		},
		{
			name:    "neither content nor content_path nor content_glob",
			cfg:     knowledgeFileConfig{Name: "n"},
			wantErr: "content_path` (file on disk)",
		},
		{
			name:    "content_glob with name set",
			cfg:     knowledgeFileConfig{Name: "n", ContentGlob: "*.md"},
			wantErr: "remove the top-level",
		},
		{
			name:    "content_glob with content_path set",
			cfg:     knowledgeFileConfig{ContentGlob: "*.md", ContentPath: "./x"},
			wantErr: "cannot be combined",
		},
		{
			name: "valid content_path entry",
			cfg:  knowledgeFileConfig{Name: "n", ContentPath: "./x"},
		},
		{
			name: "valid content_glob entry",
			cfg:  knowledgeFileConfig{ContentGlob: "*.md"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateKnowledgeConfig("test.yaml", tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestResolveKnowledgeName(t *testing.T) {
	cases := []struct {
		name     string
		tmpl     string
		relNoExt string
		want     string
	}{
		{
			name:     "default template is relpath",
			tmpl:     "",
			relNoExt: "delivery/DEVELOPER",
			want:     "delivery/DEVELOPER",
		},
		{
			name:     "explicit relpath",
			tmpl:     "{{relpath}}",
			relNoExt: "strategic/CTO",
			want:     "strategic/CTO",
		},
		{
			name:     "stem only",
			tmpl:     "{{stem}}",
			relNoExt: "delivery/DEVELOPER",
			want:     "DEVELOPER",
		},
		{
			name:     "dir only, nested",
			tmpl:     "{{dir}}",
			relNoExt: "delivery/DEVELOPER",
			want:     "delivery",
		},
		{
			name:     "dir is empty at top level",
			tmpl:     "{{dir}}",
			relNoExt: "TOPLEVEL",
			want:     "",
		},
		{
			name:     "combined template",
			tmpl:     "role-{{dir}}-{{stem}}",
			relNoExt: "delivery/DEVELOPER",
			want:     "role-delivery-DEVELOPER",
		},
		{
			name:     "stem at top level equals relNoExt",
			tmpl:     "{{stem}}",
			relNoExt: "TOPLEVEL",
			want:     "TOPLEVEL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveKnowledgeName(tc.tmpl, tc.relNoExt)
			if got != tc.want {
				t.Errorf("resolveKnowledgeName(%q, %q) = %q, want %q", tc.tmpl, tc.relNoExt, got, tc.want)
			}
		})
	}
}

func TestProvisionKnowledgeBase_ContentGlob_FanOut(t *testing.T) {
	srv := &knowledgeTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Layout:
	//   root/roles/delivery/DEVELOPER.md
	//   root/roles/strategic/CTO.md
	//   root/knowledge-base/roles.yaml   (content_glob: ../roles/**/*.md)
	root := t.TempDir()
	rolesDir := filepath.Join(root, "roles")
	if err := os.MkdirAll(filepath.Join(rolesDir, "delivery"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rolesDir, "strategic"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rolesDir, "delivery", "DEVELOPER.md"), []byte("developer role doc"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rolesDir, "strategic", "CTO.md"), []byte("cto role doc"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	kbDir := filepath.Join(root, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	yaml := `content_glob: ../roles/**/*.md
file_type: template
purpose: reference
tags: [org-context, roles]
`
	if err := os.WriteFile(filepath.Join(kbDir, "roles.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := provisionKnowledgeBase(c, kbDir, 12); err != nil {
		t.Fatalf("provisionKnowledgeBase: %v", err)
	}

	if len(srv.calls) != 2 {
		t.Fatalf("want 2 upsert calls (one per matched file), got %d: %+v", len(srv.calls), srv.calls)
	}

	byTitle := map[string]map[string]any{}
	for _, call := range srv.calls {
		byTitle[call["title"].(string)] = call
	}
	dev, ok := byTitle["delivery/DEVELOPER"]
	if !ok {
		t.Fatalf("expected a call titled delivery/DEVELOPER, got titles: %v", keysOf(byTitle))
	}
	if dev["content"] != "developer role doc" {
		t.Errorf("delivery/DEVELOPER content: %+v", dev)
	}
	if dev["file_type"] != "template" {
		t.Errorf("delivery/DEVELOPER file_type not propagated from shared config: %+v", dev)
	}
	cto, ok := byTitle["strategic/CTO"]
	if !ok {
		t.Fatalf("expected a call titled strategic/CTO, got titles: %v", keysOf(byTitle))
	}
	if cto["content"] != "cto role doc" {
		t.Errorf("strategic/CTO content: %+v", cto)
	}
}

func TestProvisionKnowledgeBase_ContentGlob_CustomNameTemplate(t *testing.T) {
	srv := &knowledgeTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "definition-of-done.md"), []byte("dod content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	kbDir := filepath.Join(root, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	yaml := `content_glob: ../skills/*.md
name_template: "org-skill-{{stem}}"
file_type: template
`
	if err := os.WriteFile(filepath.Join(kbDir, "skills.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := provisionKnowledgeBase(c, kbDir, 12); err != nil {
		t.Fatalf("provisionKnowledgeBase: %v", err)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("want 1 upsert call, got %d", len(srv.calls))
	}
	if srv.calls[0]["title"] != "org-skill-definition-of-done" {
		t.Errorf("title with custom name_template: %+v", srv.calls[0])
	}
}

func TestProvisionKnowledgeBase_ContentGlob_NoMatches_WarnsNotFails(t *testing.T) {
	srv := &knowledgeTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `content_glob: ./nonexistent/*.md
file_type: template
`
	if err := os.WriteFile(filepath.Join(dir, "empty.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := provisionKnowledgeBase(c, dir, 12); err != nil {
		t.Fatalf("provisionKnowledgeBase should not fail on zero matches: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("expected zero upserts, got %d", len(srv.calls))
	}
	if c.WarningCount() != 1 {
		t.Errorf("expected 1 warning for zero matches, got %d", c.WarningCount())
	}
}

func TestProvisionKnowledgeBase_MixedStaticAndGlob_TallyAggregates(t *testing.T) {
	srv := &knowledgeTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	root := t.TempDir()
	docsDir := filepath.Join(root, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "a.md"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "b.md"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	kbDir := filepath.Join(root, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "docs.yaml"), []byte("content_glob: ../docs/*.md\n"), 0o644); err != nil {
		t.Fatalf("write glob yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "static.yaml"), []byte("name: static-entry\ncontent: hello\n"), 0o644); err != nil {
		t.Fatalf("write static yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := provisionKnowledgeBase(c, kbDir, 12); err != nil {
		t.Fatalf("provisionKnowledgeBase: %v", err)
	}

	// 2 from the glob (a.md, b.md) + 1 static = 3 total upserts.
	if len(srv.calls) != 3 {
		t.Fatalf("want 3 upsert calls total, got %d", len(srv.calls))
	}
}

func TestProvisionKnowledgeBase_ContentGlob_SkipsMatchedDirectories(t *testing.T) {
	srv := &knowledgeTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// A directory literally named "zzz.md" alongside real .md files: without
	// doublestar.WithFilesOnly(), this used to reach os.ReadFile and fail
	// with "is a directory" after earlier matches had already been upserted.
	root := t.TempDir()
	docsDir := filepath.Join(root, "docs")
	if err := os.MkdirAll(filepath.Join(docsDir, "zzz.md"), 0o755); err != nil {
		t.Fatalf("mkdir dir-named-like-a-match: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "aaa.md"), []byte("real file"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	kbDir := filepath.Join(root, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "docs.yaml"), []byte("content_glob: ../docs/*.md\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := provisionKnowledgeBase(c, kbDir, 12); err != nil {
		t.Fatalf("provisionKnowledgeBase should skip the directory match, not fail: %v", err)
	}
	if len(srv.calls) != 1 {
		t.Fatalf("want 1 upsert call (the real file only), got %d: %+v", len(srv.calls), srv.calls)
	}
	if srv.calls[0]["title"] != "aaa" {
		t.Errorf("title: %+v", srv.calls[0])
	}
}

func TestProvisionKnowledgeBase_ContentGlob_NameCollision_FailsFastNoUpserts(t *testing.T) {
	srv := &knowledgeTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// docs/a/README.md and docs/b/README.md both resolve to "README" under
	// {{stem}} — must fail before either is upserted, not silently let the
	// second overwrite the first.
	root := t.TempDir()
	docsDir := filepath.Join(root, "docs")
	if err := os.MkdirAll(filepath.Join(docsDir, "a"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(docsDir, "b"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "a", "README.md"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "b", "README.md"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	kbDir := filepath.Join(root, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	yaml := `content_glob: ../docs/**/*.md
name_template: "{{stem}}"
`
	if err := os.WriteFile(filepath.Join(kbDir, "docs.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	err := provisionKnowledgeBase(c, kbDir, 12)
	if err == nil {
		t.Fatal("expected a name-collision error, got nil")
	}
	if !strings.Contains(err.Error(), "produces the same name") {
		t.Errorf("error should explain the collision, got: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("collision must be caught before any upsert — got %d calls: %+v", len(srv.calls), srv.calls)
	}
}

func TestProvisionKnowledgeBase_ContentGlob_EmptyNameTemplate_FailsFast(t *testing.T) {
	srv := &knowledgeTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	root := t.TempDir()
	docsDir := filepath.Join(root, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "TOPLEVEL.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	kbDir := filepath.Join(root, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	yaml := `content_glob: ../docs/*.md
name_template: "{{dir}}"
`
	if err := os.WriteFile(filepath.Join(kbDir, "docs.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	err := provisionKnowledgeBase(c, kbDir, 12)
	if err == nil {
		t.Fatal("expected an empty-name error, got nil")
	}
	if !strings.Contains(err.Error(), "resolved to an empty name") {
		t.Errorf("error should explain the empty name, got: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("empty name must be caught before any upsert — got %d calls", len(srv.calls))
	}
}

// keysOf is a small test helper for readable failure messages.
func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
