package commands

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A session is paid per template folder: a copy of a folder (which copies
// .taufinity/preview.json with it) must not reuse the original's key.
func TestBannerPreviewKeyPerFolder(t *testing.T) {
	readDoc := func(t *testing.T, dir string) (map[string]string, string) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, bannerStateDir, bannerKeyFile))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]string
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return doc, string(raw)
	}
	t.Run("same_folder_keeps_key", func(t *testing.T) {
		dir := templateDir(t, nil)
		k1, err := templateKeyFor(dir)
		if err != nil {
			t.Fatal(err)
		}
		k2, err := templateKeyFor(dir)
		if err != nil || k2 != k1 {
			t.Fatalf("second read: %q, %v (want %q)", k2, err, k1)
		}
	})
	t.Run("copied_folder_gets_new_key", func(t *testing.T) {
		orig := templateDir(t, nil)
		k1, err := templateKeyFor(orig)
		if err != nil {
			t.Fatal(err)
		}
		cp := filepath.Join(t.TempDir(), "kopie")
		if out, err := exec.Command("cp", "-R", orig, cp).CombinedOutput(); err != nil {
			t.Fatalf("cp: %v %s", err, out)
		}
		k2, err := templateKeyFor(cp)
		if err != nil {
			t.Fatal(err)
		}
		if k2 == k1 {
			t.Fatal("a copied folder reused the original's key")
		}
		if again, _ := templateKeyFor(cp); again != k2 {
			t.Fatalf("the copy's new key did not stick: %q then %q", k2, again)
		}
		if back, _ := templateKeyFor(orig); back != k1 {
			t.Fatalf("the original lost its key: %q, want %q", back, k1)
		}
	})
	t.Run("old_file_without_folder_adopted", func(t *testing.T) {
		dir := templateDir(t, nil)
		path := filepath.Join(dir, bannerStateDir, bannerKeyFile)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		const old = "AbCdEfGhIjKlMnOpQrStUv"
		if err := os.WriteFile(path, []byte(`{"template_key": "`+old+`"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		k, err := templateKeyFor(dir)
		if err != nil || k != old {
			t.Fatalf("got %q, %v; an existing key must be kept", k, err)
		}
		doc, _ := readDoc(t, dir)
		if doc["folder"] == "" || doc["template_key"] != old {
			t.Fatalf("the folder was not recorded: %v", doc)
		}
	})
	t.Run("file_holds_no_path", func(t *testing.T) {
		dir := templateDir(t, nil)
		if _, err := templateKeyFor(dir); err != nil {
			t.Fatal(err)
		}
		_, raw := readDoc(t, dir)
		abs, _ := filepath.Abs(dir)
		home, _ := os.UserHomeDir()
		for _, s := range []string{abs, filepath.Base(abs), home, os.Getenv("USER")} {
			if s != "" && strings.Contains(raw, s) {
				t.Fatalf("the key file contains %q: %s", s, raw)
			}
		}
	})
}
