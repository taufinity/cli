package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The built-in secret excludes are a promise: a template's own ignore files
// must not be able to put a secret back into an upload with a "!" rule.
func TestSecretsCannotBeReincluded(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html":       "<p>hi</p>",
		"credentials.json": `{"k":"v"}`,
		"server.pem":       "-----BEGIN-----",
		".env":             "TOKEN=x",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reinclude := "!credentials.json\n!*.pem\n!.env\n"
	for _, f := range []string{".gitignore", ".taufinityignore"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(reinclude), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	secret := map[string]bool{"credentials.json": true, "server.pem": true, ".env": true}

	t.Run("zip", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "t.zip")
		zf, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CreateZipWithOptions(context.Background(), dir, zf, ZipOptions{}); err != nil {
			t.Fatal(err)
		}
		zf.Close()
		zr, err := zip.OpenReader(out)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			if secret[f.Name] {
				t.Errorf("secret %s is in the zip", f.Name)
			}
		}
	})
	t.Run("tar_gz", func(t *testing.T) {
		names, _ := tarNames(t, dir)
		for _, n := range names {
			if secret[n] {
				t.Errorf("secret %s is in the tar.gz", n)
			}
		}
	})
}

// The tar.gz upload carries no local user or group of the developer.
func TestTarGzHasNoLocalOwner(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>hi</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, hdrs := tarNames(t, dir)
	if len(hdrs) == 0 {
		t.Fatal("no entries")
	}
	for _, h := range hdrs {
		if h.Uname != "" || h.Gname != "" || h.Uid != 0 || h.Gid != 0 {
			t.Errorf("%s carries owner %q/%q %d/%d", h.Name, h.Uname, h.Gname, h.Uid, h.Gid)
		}
	}
}

func tarNames(t *testing.T, dir string) ([]string, []*tar.Header) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "t.tar.gz")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateTarGzWithOptions(context.Background(), dir, f, ZipOptions{}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	rf, _ := os.Open(out)
	defer rf.Close()
	gz, err := gzip.NewReader(rf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	var hdrs []*tar.Header
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names, hdrs = append(names, h.Name), append(hdrs, h)
	}
	sort.Strings(names)
	return names, hdrs
}
