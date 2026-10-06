package commands

import (
	"os"
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

	cfg := &providerConfig{TablePages: map[string]string{
		"rpt_platform_performance": "docs/table-pages/rpt_platform_performance.md",
	}}
	if err := resolveTablePages(root, cfg, false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := cfg.tablePageContents["rpt_platform_performance"]; got != "# page" {
		t.Fatalf("content = %q, want the file's content", got)
	}
}

// A missing page file is a hard error on apply (a partial upload would leave
// an agent reading "no table page" for a table the spec claims has one) and a
// warning on dry-run.
func TestResolveTablePages_MissingFile(t *testing.T) {
	root := t.TempDir()
	cfg := &providerConfig{TablePages: map[string]string{
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
		cfg := &providerConfig{TablePages: map[string]string{"core_transactions": "docs/table-pages/any.md"}}
		if err := resolveTablePages(root, cfg, false); err == nil {
			t.Fatal("a core_ table must be refused")
		}
	})
	t.Run("mart_ allowed", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(pageDir, "mart_failed_shifts.md"), []byte("# page"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := &providerConfig{TablePages: map[string]string{"mart_failed_shifts": "docs/table-pages/mart_failed_shifts.md"}}
		if err := resolveTablePages(root, cfg, false); err != nil {
			t.Fatalf("mart_* is a legitimate surface table: %v", err)
		}
	})
	t.Run("oversized page refused", func(t *testing.T) {
		cfg := &providerConfig{TablePages: map[string]string{"rpt_x": "docs/table-pages/big.md"}}
		if err := resolveTablePages(root, cfg, false); err == nil {
			t.Fatal("a page over 32 KB must be refused")
		}
	})
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
