package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Table pages resolve against the repo root (the parent of --dir), so a page
// can live where dbt's meta.table_page points, including a ../ path into a
// sibling repo.
func TestResolveTablePages_ReadsContent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "studio")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pageDir := filepath.Join(root, "docs", "table-pages")
	if err := os.MkdirAll(pageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pageDir, "rpt_platform_performance.md"), []byte("# page"), 0o644); err != nil {
		t.Fatal(err)
	}

	commitTablePageFixture(t, root)
	cfg := &providerConfig{AllowedTables: []string{"rpt_platform_performance"}, TablePages: map[string]string{
		"rpt_platform_performance": "docs/table-pages/rpt_platform_performance.md",
	}}
	if err := resolveTablePages(root, cfg, false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := cfg.tablePageContents["rpt_platform_performance"].Markdown; got != "# page" {
		t.Fatalf("content = %q, want the file's content", got)
	}
}

func TestResolveTablePages_UsesExplicitRepoRootForStagedSpec(t *testing.T) {
	workspace := t.TempDir()
	templatesRoot := filepath.Join(workspace, "templates")
	dataRoot := filepath.Join(workspace, "data")
	pageDir := filepath.Join(dataRoot, "docs", "table-pages")
	if err := os.MkdirAll(templatesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pageDir, "platform.md"), []byte("# platform"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitTablePageFixture(t, dataRoot)

	cfg := &providerConfig{AllowedTables: []string{"rpt_platform_performance"}, TablePages: map[string]string{
		"rpt_platform_performance": "../data/docs/table-pages/platform.md",
	}}
	if err := resolveTablePages(templatesRoot, cfg, false); err != nil {
		t.Fatalf("resolve staged spec with explicit repo root: %v", err)
	}
	if got := cfg.tablePageContents["rpt_platform_performance"].Markdown; got != "# platform" {
		t.Fatalf("content = %q, want page from sibling data repo", got)
	}
}

// A missing page file is a hard error on apply (a partial upload would leave
// an agent reading "no table page" for a table the spec claims has one) and a
// warning on dry-run.
func TestResolveTablePages_MissingFile(t *testing.T) {
	root := t.TempDir()
	cfg := &providerConfig{AllowedTables: []string{"rpt_platform_performance"}, TablePages: map[string]string{
		"rpt_platform_performance": "docs/table-pages/missing.md",
	}}
	if err := resolveTablePages(root, cfg, false); err == nil {
		t.Fatal("apply must fail on a missing page file")
	}
	if err := resolveTablePages(root, cfg, true); err != nil {
		t.Fatalf("dry-run must warn, not fail: %v", err)
	}
	if len(cfg.tablePageContents) != 0 {
		t.Fatalf("dry-run contents = %v, want empty", cfg.tablePageContents)
	}
}

// No table_pages in the spec resolves to nothing and uploads nothing.
func TestResolveTablePages_None(t *testing.T) {
	cfg := &providerConfig{}
	if err := resolveTablePages(t.TempDir(), cfg, false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.tablePageContents != nil {
		t.Fatalf("contents = %v, want nil", cfg.tablePageContents)
	}
}

// The server's boundaries are mirrored at provision time so a run fails here
// rather than at the PUT: internal layers never get a page, and one page is
// at most 32 KB.
func TestResolveTablePages_Boundaries(t *testing.T) {
	root := t.TempDir()
	pageDir := filepath.Join(root, "docs", "table-pages")
	if err := os.MkdirAll(pageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 33<<10)
	if err := os.WriteFile(filepath.Join(pageDir, "big.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("internal layer refused", func(t *testing.T) {
		cfg := &providerConfig{AllowedTables: []string{"core_transactions"}, TablePages: map[string]string{"core_transactions": "docs/table-pages/any.md"}}
		if err := resolveTablePages(root, cfg, false); err == nil {
			t.Fatal("a core_ table must be refused")
		}
	})
	t.Run("mart_ allowed", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(pageDir, "mart_failed_shifts.md"), []byte("# page"), 0o644); err != nil {
			t.Fatal(err)
		}
		commitTablePageFixture(t, root)
		cfg := &providerConfig{AllowedTables: []string{"mart_failed_shifts"}, TablePages: map[string]string{"mart_failed_shifts": "docs/table-pages/mart_failed_shifts.md"}}
		if err := resolveTablePages(root, cfg, false); err != nil {
			t.Fatalf("mart_* is a legitimate surface table: %v", err)
		}
	})
	t.Run("oversized page refused", func(t *testing.T) {
		cfg := &providerConfig{AllowedTables: []string{"rpt_x"}, TablePages: map[string]string{"rpt_x": "docs/table-pages/big.md"}}
		if err := resolveTablePages(root, cfg, false); err == nil {
			t.Fatal("a page over 32 KB must be refused")
		}
	})
}

func TestResolveTablePages_RequiresAllowListAndCommittedSource(t *testing.T) {
	root := t.TempDir()
	pageDir := filepath.Join(root, "docs", "table-pages")
	if err := os.MkdirAll(pageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pageDir, "rpt_x.md")
	if err := os.WriteFile(path, []byte("# page"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("not allow-listed", func(t *testing.T) {
		cfg := &providerConfig{TablePages: map[string]string{"rpt_x": "docs/table-pages/rpt_x.md"}}
		if err := resolveTablePages(root, cfg, true); err == nil {
			t.Fatal("dry-run must reject a table page outside allowed_tables")
		}
	})
	t.Run("dirty source", func(t *testing.T) {
		commitTablePageFixture(t, root)
		if err := os.WriteFile(path, []byte("# changed"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := &providerConfig{AllowedTables: []string{"rpt_x"}, TablePages: map[string]string{"rpt_x": "docs/table-pages/rpt_x.md"}}
		if err := resolveTablePages(root, cfg, false); err == nil {
			t.Fatal("apply must reject an uncommitted table page")
		}
	})
}

func commitTablePageFixture(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		runGit(t, root, "init", "-q")
		runGit(t, root, "config", "user.email", "test@example.com")
		runGit(t, root, "config", "user.name", "Test")
	}
	runGit(t, root, "add", "docs/table-pages")
	cmd := exec.Command("git", "-C", root, "diff", "--cached", "--quiet")
	if err := cmd.Run(); err != nil {
		runGit(t, root, "commit", "-qm", "table pages")
	}
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", root}, args...)
	if out, err := exec.Command("git", cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// The fingerprint identifies page content in diffs by size and hash prefix.
func TestPageFingerprint(t *testing.T) {
	got := pageFingerprint("hello")
	if !strings.Contains(got, "5 chars") {
		t.Fatalf("fingerprint = %q, want the size in it", got)
	}
	if pageFingerprint("hello") == pageFingerprint("world") {
		t.Fatal("different content must produce different fingerprints")
	}
}

func TestCommittedPageSource_IgnoresUnrelatedRepoCommits(t *testing.T) {
	root := t.TempDir()
	pageDir := filepath.Join(root, "docs", "table-pages")
	if err := os.MkdirAll(pageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pagePath := filepath.Join(pageDir, "rpt_x.md")
	if err := os.WriteFile(pagePath, []byte("# page"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitTablePageFixture(t, root)
	before, beforeUpdatedAt, err := committedPageSource(pagePath, false)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "README.md")
	runGit(t, root, "commit", "-qm", "unrelated change")
	after, afterUpdatedAt, err := committedPageSource(pagePath, false)
	if err != nil {
		t.Fatal(err)
	}
	if after != before || afterUpdatedAt != beforeUpdatedAt {
		t.Fatalf("page provenance changed after unrelated commit: (%q, %q) -> (%q, %q)", before, beforeUpdatedAt, after, afterUpdatedAt)
	}
}

func TestWriteBQProviderBoundary_ExplicitlyClearsPages(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/admin/bq-providers/42" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newProvisionClient(srv.URL, "key", false)
	if err := writeBQProviderBoundary(c, 42, []byte(`[]`), nil); err != nil {
		t.Fatal(err)
	}
	if got["allowed_tables"] != "[]" || got["table_pages"] != "{}" {
		t.Fatalf("payload = %#v, want explicit empty allowed_tables and table_pages", got)
	}
}
