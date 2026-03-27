package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectConfig_Load(t *testing.T) {
	// Use the test fixture
	fixtureDir := filepath.Join("..", "..", "tests", "fixtures", "template-repo")

	cfg, err := LoadProject(fixtureDir)
	if err != nil {
		t.Fatalf("LoadProject failed: %v", err)
	}

	if cfg.Site != "test_site_nl" {
		t.Errorf("Site = %q, want %q", cfg.Site, "test_site_nl")
	}
	if cfg.Template != "templates/article.html" {
		t.Errorf("Template = %q, want %q", cfg.Template, "templates/article.html")
	}
	if len(cfg.Ignore) != 2 {
		t.Errorf("Ignore has %d items, want 2", len(cfg.Ignore))
	}
}

func TestProjectConfig_LoadMissing(t *testing.T) {
	tmpDir := t.TempDir()

	cfg, err := LoadProject(tmpDir)
	if err != nil {
		t.Fatalf("LoadProject should not error on missing file: %v", err)
	}

	// Should return empty config
	if cfg.Site != "" {
		t.Errorf("Site = %q, want empty", cfg.Site)
	}
}

func TestProjectConfig_FindProjectRoot(t *testing.T) {
	// Create nested directory structure
	tmpDir := t.TempDir()
	nestedDir := filepath.Join(tmpDir, "a", "b", "c")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	// Create taufinity.yaml in root
	configContent := []byte("site: found_it\n")
	if err := os.WriteFile(filepath.Join(tmpDir, "taufinity.yaml"), configContent, 0644); err != nil {
		t.Fatalf("write config failed: %v", err)
	}

	// Find from nested dir
	root, err := FindProjectRoot(nestedDir)
	if err != nil {
		t.Fatalf("FindProjectRoot failed: %v", err)
	}

	if root != tmpDir {
		t.Errorf("FindProjectRoot() = %q, want %q", root, tmpDir)
	}
}

func TestProjectConfig_UnknownKeys(t *testing.T) {
	tmpDir := t.TempDir()
	content := []byte("site: my_site\ntemplate: page.html\nbogus_key: value\nalso_bad: true\n")
	if err := os.WriteFile(filepath.Join(tmpDir, "taufinity.yaml"), content, 0644); err != nil {
		t.Fatalf("write config failed: %v", err)
	}

	cfg, err := LoadProject(tmpDir)
	if err != nil {
		t.Fatalf("LoadProject failed: %v", err)
	}

	if cfg.Site != "my_site" {
		t.Errorf("Site = %q, want %q", cfg.Site, "my_site")
	}
	if len(cfg.Warnings) != 2 {
		t.Errorf("Warnings has %d items, want 2: %v", len(cfg.Warnings), cfg.Warnings)
	}
}

func TestProjectConfig_ValidKeysNoWarnings(t *testing.T) {
	tmpDir := t.TempDir()
	content := []byte("site: my_site\ntemplate: page.html\npreview_data: data.json\nignore:\n  - '*.test'\n")
	if err := os.WriteFile(filepath.Join(tmpDir, "taufinity.yaml"), content, 0644); err != nil {
		t.Fatalf("write config failed: %v", err)
	}

	cfg, err := LoadProject(tmpDir)
	if err != nil {
		t.Fatalf("LoadProject failed: %v", err)
	}

	if len(cfg.Warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", cfg.Warnings)
	}
	if cfg.PreviewData != "data.json" {
		t.Errorf("PreviewData = %q, want %q", cfg.PreviewData, "data.json")
	}
}

func TestProjectConfig_FindProjectRootNotFound(t *testing.T) {
	tmpDir := t.TempDir()

	_, err := FindProjectRoot(tmpDir)
	if err == nil {
		t.Error("FindProjectRoot should error when no taufinity.yaml found")
	}
}
