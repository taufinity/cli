package review

import (
	"context"
	"strings"
	"testing"
)

func TestExclusions(t *testing.T) {
	ctx := context.Background()

	// collectNew writes the file as an untracked file and collects.
	collectNew := func(t *testing.T, name, content string) Payload {
		t.Helper()
		dir := newRepo(t)
		writeFile(t, dir, name, content)
		p, err := Collect(ctx, dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	skipped := func(p Payload, name, reason string) bool {
		for _, s := range p.Skipped {
			if s.Path == name && s.Reason == reason {
				return true
			}
		}
		return false
	}
	assertOut := func(t *testing.T, p Payload, name string) {
		t.Helper()
		if strings.Contains(p.Diff, name) {
			t.Errorf("%s is in the diff", name)
		}
		if _, ok := p.Files[name]; ok {
			t.Errorf("%s is in Files", name)
		}
	}

	t.Run("env_files_skipped", func(t *testing.T) {
		for _, n := range []string{".env", ".env.production", "certs/site.pem", "a.key", "x.p12", "x.pfx", "id_rsa", "id_rsa.pub", "app.sqlite", "app.db", "x.dump", "x.sql.gz", "app.min.js", "app.min.css", "app.js.map"} {
			p := collectNew(t, n, "VALUE=1\n")
			if !skipped(p, n, "excluded") {
				t.Errorf("%s not skipped as excluded: %v", n, p.Skipped)
			}
			assertOut(t, p, n)
		}
	})

	t.Run("lockfiles_skipped", func(t *testing.T) {
		for _, n := range []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum", "Cargo.lock", "composer.lock", "web/yarn.lock"} {
			p := collectNew(t, n, "x\n")
			if !skipped(p, n, "excluded") {
				t.Errorf("%s not skipped: %v", n, p.Skipped)
			}
			assertOut(t, p, n)
		}
	})

	t.Run("node_modules_skipped", func(t *testing.T) {
		for _, n := range []string{"node_modules/a/index.js", "vendor/x/y.go", "dist/app.js", "build/out.txt", "web/node_modules/a.js"} {
			p := collectNew(t, n, "x\n")
			if !skipped(p, n, "excluded") {
				t.Errorf("%s not skipped: %v", n, p.Skipped)
			}
			assertOut(t, p, n)
		}
	})

	t.Run("source_dirs_named_build_kept", func(t *testing.T) {
		p := collectNew(t, "cmd/build/main.go", "package main\n")
		if _, ok := p.Files["cmd/build/main.go"]; !ok {
			t.Errorf("nested build/ source was dropped: %v", p.Skipped)
		}
	})

	t.Run("binary_skipped", func(t *testing.T) {
		p := collectNew(t, "logo.png", "\x89PNG\x00\x00data")
		if !skipped(p, "logo.png", "binary") {
			t.Errorf("not skipped as binary: %v", p.Skipped)
		}
		assertOut(t, p, "logo.png")
	})

	t.Run("over_1mib_skipped", func(t *testing.T) {
		p := collectNew(t, "huge.txt", strings.Repeat("a", 1<<20+1))
		if !skipped(p, "huge.txt", "too_large") {
			t.Errorf("not skipped as too_large: %v", p.Skipped)
		}
		assertOut(t, p, "huge.txt")
		p = collectNew(t, "exact.txt", strings.Repeat("a", 1<<20))
		if _, ok := p.Files["exact.txt"]; !ok {
			t.Error("a file of exactly 1 MiB must be kept")
		}
	})

	t.Run("tracked_changes_are_filtered_too", func(t *testing.T) {
		dir := newRepo(t)
		writeFile(t, dir, ".env", "A=1\n")
		writeFile(t, dir, "a.go", "package a\n")
		commitAll(t, dir, "add")
		writeFile(t, dir, ".env", "A=2\n")
		writeFile(t, dir, "a.go", "package a\n// x\n")
		p, err := Collect(ctx, dir, Options{Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		assertOut(t, p, ".env")
		if !skipped(p, ".env", "excluded") || !strings.Contains(p.Diff, "a.go") {
			t.Errorf("skipped=%v diff=%q", p.Skipped, p.Diff)
		}
	})

	t.Run("deleted_secret_not_in_diff", func(t *testing.T) {
		dir := newRepo(t)
		writeFile(t, dir, ".env", "TOKEN_VALUE=abc\n")
		commitAll(t, dir, "env")
		gitRun(t, dir, "checkout", "-q", "-b", "feat")
		gitRun(t, dir, "rm", "-q", ".env")
		commitAll(t, dir, "rm")
		p, err := Collect(ctx, dir, Options{Base: "HEAD~1"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p.Diff, "TOKEN_VALUE") {
			t.Error("a deleted .env leaked its content through the diff")
		}
	})

	t.Run("normal_file_kept", func(t *testing.T) {
		p := collectNew(t, "main.go", "package main\n")
		if p.Files["main.go"] != "package main\n" || len(p.Skipped) != 0 || !strings.Contains(p.Diff, "+package main") {
			t.Errorf("payload: %+v", p)
		}
	})
}
