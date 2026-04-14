package archive

import (
	"archive/zip"
	"bytes"
	"path/filepath"
	"testing"
)

func TestCreateZip_BasicFolder(t *testing.T) {
	tmpDir := t.TempDir()
	createFile(t, tmpDir, "index.html", "<html></html>")
	createFile(t, tmpDir, "assets/app.js", "console.log('hi');")
	createFile(t, tmpDir, "assets/style.css", "body{}")

	var buf bytes.Buffer
	count, err := CreateZip(tmpDir, &buf)
	if err != nil {
		t.Fatalf("CreateZip failed: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 files, got %d", count)
	}

	files := listZip(t, buf.Bytes())
	for _, want := range []string{"index.html", "assets/app.js", "assets/style.css"} {
		if !contains(files, want) {
			t.Errorf("zip should contain %s, got: %v", want, files)
		}
	}
}

func TestCreateZip_HonorsSecurityIgnores(t *testing.T) {
	tmpDir := t.TempDir()
	createFile(t, tmpDir, "index.html", "keep")
	createFile(t, tmpDir, ".env", "SECRET=leak")
	createFile(t, tmpDir, "secrets/credentials.json", "{}")
	createFile(t, tmpDir, "node_modules/foo/index.js", "bad")
	createFile(t, tmpDir, "private.key", "-----BEGIN-----")

	var buf bytes.Buffer
	if _, err := CreateZip(tmpDir, &buf); err != nil {
		t.Fatalf("CreateZip failed: %v", err)
	}

	files := listZip(t, buf.Bytes())
	if !contains(files, "index.html") {
		t.Errorf("zip should contain index.html, got: %v", files)
	}
	for _, bad := range []string{".env", "secrets/credentials.json", "node_modules/foo/index.js", "private.key"} {
		if contains(files, bad) {
			t.Errorf("zip MUST NOT contain %s, got: %v", bad, files)
		}
	}
}

func TestCreateZip_EmptyDirIsError(t *testing.T) {
	tmpDir := t.TempDir()
	var buf bytes.Buffer
	_, err := CreateZip(tmpDir, &buf)
	if err == nil {
		t.Fatal("expected error for empty directory, got nil")
	}
}

func TestCreateZip_ForwardSlashPaths(t *testing.T) {
	tmpDir := t.TempDir()
	createFile(t, tmpDir, filepath.Join("sub", "dir", "file.txt"), "x")

	var buf bytes.Buffer
	if _, err := CreateZip(tmpDir, &buf); err != nil {
		t.Fatalf("CreateZip failed: %v", err)
	}

	files := listZip(t, buf.Bytes())
	if !contains(files, "sub/dir/file.txt") {
		t.Errorf("zip should use forward slashes, got: %v", files)
	}
}

func listZip(t *testing.T, data []byte) []string {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	var files []string
	for _, f := range r.File {
		files = append(files, f.Name)
	}
	return files
}
