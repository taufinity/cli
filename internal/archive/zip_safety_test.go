package archive

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func write(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func zipNames(t *testing.T, dir string, opts ZipOptions) []string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := CreateZipWithOptions(t.Context(), dir, &buf, opts); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

func has(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

func TestZipSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	dir := filepath.Join(root, "tpl")
	write(t, outside, "creds.json.txt", "SECRET")
	write(t, outside, "dir/inner.txt", "x")
	write(t, dir, "index.html", "<html></html>")
	write(t, dir, "img/real.png", "png")

	t.Run("file_link_outside_skipped", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(outside, "creds.json.txt"), filepath.Join(dir, "logo.png")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		var warns []string
		names := zipNames(t, dir, ZipOptions{Warn: func(m string) { warns = append(warns, m) }})
		if has(names, "logo.png") {
			t.Fatalf("symlink uploaded: %v", names)
		}
		if len(warns) != 1 || !strings.Contains(warns[0], "logo.png") || !strings.Contains(warns[0], "symbolische") {
			t.Fatalf("warnings: %v", warns)
		}
	})
	t.Run("dir_link_skipped", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(dir, "linked")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		names := zipNames(t, dir, ZipOptions{Warn: func(string) {}})
		for _, n := range names {
			if strings.HasPrefix(n, "linked") {
				t.Fatalf("linked folder uploaded: %v", names)
			}
		}
	})
	t.Run("regular_files_kept", func(t *testing.T) {
		names := zipNames(t, dir, ZipOptions{Warn: func(string) {}})
		if !has(names, "index.html") || !has(names, "img/real.png") {
			t.Fatalf("names: %v", names)
		}
	})
}

func TestZipExcludesSecrets(t *testing.T) {
	check := func(t *testing.T, dir string, secrets ...string) {
		t.Helper()
		write(t, dir, "index.html", "<html></html>")
		names := zipNames(t, dir, ZipOptions{})
		if !has(names, "index.html") {
			t.Fatalf("index.html missing: %v", names)
		}
		for _, s := range secrets {
			if has(names, s) {
				t.Fatalf("%s was uploaded: %v", s, names)
			}
		}
	}
	t.Run("cli_credentials", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "credentials-studio.taufinity.io.json", "{}")
		write(t, dir, "credentials.json", "{}")
		check(t, dir, "credentials-studio.taufinity.io.json", "credentials.json")
	})
	t.Run("npmrc_netrc", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{".npmrc", ".netrc", ".pypirc"} {
			write(t, dir, n, "x")
		}
		check(t, dir, ".npmrc", ".netrc", ".pypirc")
	})
	t.Run("ssh_keys", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{"id_rsa", "id_ed25519", "id_ecdsa", "id_rsa.pub", ".ssh/config"} {
			write(t, dir, n, "x")
		}
		check(t, dir, "id_rsa", "id_ed25519", "id_ecdsa", "id_rsa.pub", ".ssh/config")
	})
	t.Run("aws_dir", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, ".aws/credentials", "x")
		write(t, dir, ".gcloud/a.json", "x")
		write(t, dir, "key-sa.json", "x")
		write(t, dir, "service-account-prod.json", "x")
		check(t, dir, ".aws/credentials", ".gcloud/a.json", "key-sa.json", "service-account-prod.json")
	})
	t.Run("tfvars", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "prod.tfvars", "x")
		write(t, dir, "terraform.tfstate", "x")
		check(t, dir, "prod.tfvars", "terraform.tfstate")
	})
	t.Run("parent_gitignore", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, repo, ".gitignore", "*.secret\n/banners/tpl/local.txt\n")
		write(t, repo, "banners/.gitignore", "*.draft\n")
		dir := filepath.Join(repo, "banners", "tpl")
		write(t, dir, "a.secret", "x")
		write(t, dir, "b.draft", "x")
		write(t, dir, "local.txt", "x")
		check(t, dir, "a.secret", "b.draft", "local.txt")
	})
}

func TestZipLimits(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a", "1")
	write(t, dir, "b", "2")
	var buf bytes.Buffer
	if _, err := CreateZipWithOptions(t.Context(), dir, &buf, ZipOptions{MaxFiles: 1}); err == nil || !strings.Contains(err.Error(), "te veel") {
		t.Fatalf("err: %v", err)
	}
}
