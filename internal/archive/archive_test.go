package archive

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreate_IgnoresSecrets(t *testing.T) {
	fixtureDir := filepath.Join("..", "..", "tests", "fixtures", "template-repo")

	// Create archive
	archivePath := filepath.Join(t.TempDir(), "test.tar.gz")
	if err := Create(fixtureDir, archivePath); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// List files in archive
	files, err := listTarGz(archivePath)
	if err != nil {
		t.Fatalf("listTarGz failed: %v", err)
	}

	// Should include template
	if !contains(files, "templates/article.html") {
		t.Errorf("Archive should contain templates/article.html, got: %v", files)
	}

	// Should include taufinity.yaml
	if !contains(files, "taufinity.yaml") {
		t.Errorf("Archive should contain taufinity.yaml, got: %v", files)
	}

	// Should NOT include secrets/.env (in .gitignore)
	if contains(files, "secrets/.env") {
		t.Errorf("Archive should NOT contain secrets/.env (ignored by .gitignore), got: %v", files)
	}

	// Should NOT include .git directory
	for _, f := range files {
		if strings.HasPrefix(f, ".git/") || f == ".git" {
			t.Errorf("Archive should NOT contain .git, got: %v", files)
		}
	}
}

func TestCreate_HardcodedIgnores(t *testing.T) {
	// Create temp directory with files that should always be ignored
	tmpDir := t.TempDir()

	// Create files
	createFile(t, tmpDir, "good.html", "good content")
	createFile(t, tmpDir, ".env", "SECRET=bad")
	createFile(t, tmpDir, ".env.local", "SECRET=bad")
	createFile(t, tmpDir, "secrets.yaml", "password: bad")
	createFile(t, tmpDir, "credentials.json", "{}")
	createFile(t, tmpDir, "private.key", "-----BEGIN RSA PRIVATE KEY-----")
	createFile(t, tmpDir, "cert.pem", "-----BEGIN CERTIFICATE-----")

	archivePath := filepath.Join(t.TempDir(), "test.tar.gz")
	if err := Create(tmpDir, archivePath); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	files, err := listTarGz(archivePath)
	if err != nil {
		t.Fatalf("listTarGz failed: %v", err)
	}

	// Should include good file
	if !contains(files, "good.html") {
		t.Errorf("Archive should contain good.html, got: %v", files)
	}

	// Should NOT include any sensitive files
	sensitiveFiles := []string{".env", ".env.local", "secrets.yaml", "credentials.json", "private.key", "cert.pem"}
	for _, f := range sensitiveFiles {
		if contains(files, f) {
			t.Errorf("Archive should NOT contain %s (hardcoded ignore), got: %v", f, files)
		}
	}
}

func TestCreate_CustomIgnorePatterns(t *testing.T) {
	tmpDir := t.TempDir()

	// Create files
	createFile(t, tmpDir, "keep.html", "keep this")
	createFile(t, tmpDir, "ignore.test.html", "ignore this")
	os.MkdirAll(filepath.Join(tmpDir, "dev"), 0755)
	createFile(t, tmpDir, "dev/debug.html", "ignore this too")

	// Create taufinity.yaml with custom ignores
	configContent := `site: test
ignore:
  - "*.test.html"
  - "dev/"
`
	createFile(t, tmpDir, "taufinity.yaml", configContent)

	archivePath := filepath.Join(t.TempDir(), "test.tar.gz")
	if err := Create(tmpDir, archivePath); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	files, err := listTarGz(archivePath)
	if err != nil {
		t.Fatalf("listTarGz failed: %v", err)
	}

	// Should include keep.html and taufinity.yaml
	if !contains(files, "keep.html") {
		t.Errorf("Archive should contain keep.html, got: %v", files)
	}
	if !contains(files, "taufinity.yaml") {
		t.Errorf("Archive should contain taufinity.yaml, got: %v", files)
	}

	// Should NOT include ignored patterns
	if contains(files, "ignore.test.html") {
		t.Errorf("Archive should NOT contain ignore.test.html, got: %v", files)
	}
	if containsPrefix(files, "dev/") {
		t.Errorf("Archive should NOT contain dev/, got: %v", files)
	}
}

func TestCollectIgnorePatterns(t *testing.T) {
	fixtureDir := filepath.Join("..", "..", "tests", "fixtures", "template-repo")

	patterns := CollectIgnorePatterns(fixtureDir)

	// Should have hardcoded patterns
	if !contains(patterns, ".git") {
		t.Errorf("patterns should contain .git, got: %v", patterns)
	}
	if !contains(patterns, ".env*") {
		t.Errorf("patterns should contain .env*, got: %v", patterns)
	}

	// Should have patterns from .gitignore
	if !contains(patterns, "secrets/") {
		t.Errorf("patterns should contain secrets/ from .gitignore, got: %v", patterns)
	}

	// Should have patterns from taufinity.yaml
	if !contains(patterns, "*.test.html") {
		t.Errorf("patterns should contain *.test.html from taufinity.yaml, got: %v", patterns)
	}
}

// Helper functions

func createFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}
}

func listTarGz(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var files []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		files = append(files, hdr.Name)
	}
	return files, nil
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func containsPrefix(slice []string, prefix string) bool {
	for _, s := range slice {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
