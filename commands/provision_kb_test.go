package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// kbExportServer serves a two-file knowledge base: one regular file and one
// quote file. Any non-GET request fails the test — export is read-only.
func kbExportServer(t *testing.T, writes *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	files := []map[string]any{
		{
			"id": 1, "uuid": "uuid-price", "name": "price-list.md",
			"file_type": "reference", "purpose": "pricing",
			"tags": []map[string]string{{"name": "pricing"}},
		},
		{
			"id": 2, "uuid": "uuid-quote", "name": "record-42.md",
			"file_type": "quote", "purpose": "golden record",
			"tags": []map[string]string{{"name": "golden"}},
		},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			*writes++
			mu.Unlock()
			t.Errorf("kb-export must not write: %s %s", r.Method, r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/knowledge-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
		case strings.HasPrefix(r.URL.Path, "/api/knowledge-files/"):
			uuid := strings.TrimPrefix(r.URL.Path, "/api/knowledge-files/")
			for _, f := range files {
				if f["uuid"] == uuid {
					out := map[string]any{}
					for k, v := range f {
						out[k] = v
					}
					out["extracted_text_full"] = "content of " + uuid
					_ = json.NewEncoder(w).Encode(out)
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestKBExportHappyPath(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	ts := kbExportServer(t, &writes, &mu)
	defer ts.Close()

	out := t.TempDir()
	c := newProvisionClient(ts.URL, "key", false)
	if err := exportKnowledgeBase(c, 3, out, "", false, false); err != nil {
		t.Fatalf("exportKnowledgeBase: %v", err)
	}
	if writes != 0 {
		t.Errorf("export issued %d write(s), want 0", writes)
	}

	content, err := os.ReadFile(filepath.Join(out, "price-list.md"))
	if err != nil {
		t.Fatalf("content file: %v", err)
	}
	if string(content) != "content of uuid-price" {
		t.Errorf("content = %q", content)
	}

	sidecarBytes, err := os.ReadFile(filepath.Join(out, "price-list.md.yaml"))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	// The sidecar must parse back into exactly the shape the apply path reads.
	var cfg knowledgeFileConfig
	if err := yaml.Unmarshal(sidecarBytes, &cfg); err != nil {
		t.Fatalf("sidecar does not round-trip into knowledgeFileConfig: %v", err)
	}
	if cfg.Name != "price-list.md" || cfg.FileType != "reference" || cfg.ContentPath != "./price-list.md" {
		t.Errorf("sidecar = %+v", cfg)
	}
	if len(cfg.Tags) != 1 || cfg.Tags[0] != "pricing" {
		t.Errorf("tags = %v", cfg.Tags)
	}

	// Quote files hold verbatim customer records; they stay out unless asked for.
	if _, err := os.Stat(filepath.Join(out, "record-42.md")); !os.IsNotExist(err) {
		t.Error("quote file was exported without --include-quotes")
	}
}

func TestKBExportIncludeQuotes(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	ts := kbExportServer(t, &writes, &mu)
	defer ts.Close()

	out := t.TempDir()
	c := newProvisionClient(ts.URL, "key", false)
	if err := exportKnowledgeBase(c, 3, out, "", true, false); err != nil {
		t.Fatalf("exportKnowledgeBase: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "record-42.md")); err != nil {
		t.Errorf("--include-quotes should have exported the quote file: %v", err)
	}
}

func TestKBExportTagFilter(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	ts := kbExportServer(t, &writes, &mu)
	defer ts.Close()

	out := t.TempDir()
	c := newProvisionClient(ts.URL, "key", false)
	if err := exportKnowledgeBase(c, 3, out, "PRICING", false, false); err != nil {
		t.Fatalf("exportKnowledgeBase: %v", err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 2 { // content + sidecar for the one matching file
		t.Errorf("tag filter exported %d file(s), want 2", len(entries))
	}
}

func TestKBExportDryRunWritesNothing(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	ts := kbExportServer(t, &writes, &mu)
	defer ts.Close()

	out := t.TempDir()
	c := newProvisionClient(ts.URL, "key", false)
	if err := exportKnowledgeBase(c, 3, out, "", false, true); err != nil {
		t.Fatalf("dry-run export: %v", err)
	}
	if writes != 0 {
		t.Errorf("dry-run export issued %d API write(s), want 0", writes)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Errorf("dry-run wrote %d file(s) to disk, want 0", len(entries))
	}
}
