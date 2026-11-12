package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeServiceAccountKey has the shape of a Google service-account key file,
// with no real key in it.
const fakeServiceAccountKey = `{
  "type": "service_account",
  "project_id": "example-project",
  "private_key_id": "0000",
  "private_key": "not-a-real-key",
  "client_email": "x@example-project.iam.gserviceaccount.com"
}
`

// TestNeverSentPatterns has one case per never-sent pattern, each named so
// that only that pattern matches it: removing any one pattern from exclude.go
// fails exactly its case. The file is an untracked new file, which takes the
// same exclusion path as a tracked change.
func TestNeverSentPatterns(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		pattern, file, content string
	}{
		// excludedNames
		{".envrc", ".envrc", "export DB_PASS=x\n"},
		{".netrc", ".netrc", "machine h login u password p\n"},
		{"_netrc", "_netrc", "machine h login u password p\n"},
		{".pgpass", ".pgpass", "h:5432:db:u:p\n"},
		{".npmrc", ".npmrc", "//registry.npmjs.org/:_authToken=x\n"},
		{".pypirc", ".pypirc", "[pypi]\npassword = x\n"},
		{".git-credentials", ".git-credentials", "https://u:p@example.com\n"},
		{".dockercfg", ".dockercfg", "{}\n"},
		{"credentials.json", "app/credentials.json", "{}\n"},
		// Docker's config.json, only under .docker
		{".docker/config.json", "home/.docker/config.json", "{\"auths\":{}}\n"},
		// excludedPrefixes
		{".env.", ".env.local", "DB_PASS=x\n"},
		{".env-", ".env-local", "DB_PASS=x\n"},
		{"id_rsa", "keys/id_rsa", "x\n"},
		{"id_dsa", "id_dsa", "x\n"},
		{"id_ecdsa", "id_ecdsa_sk", "x\n"},
		{"id_ed25519", "id_ed25519", "x\n"},
		// excludedSuffixes
		{"*.env", "deploy/prod.env", "DB_PASS=x\n"},
		{"*.pem", "certs/site.pem", "x\n"},
		{"*.key", "a.key", "x\n"},
		{"*.p12", "x.p12", "x\n"},
		{"*.pfx", "x.pfx", "x\n"},
		{"*.jks", "release.jks", "x\n"},
		{"*.keystore", "debug.keystore", "x\n"},
		{"*.ppk", "deploy.ppk", "x\n"},
		{"*.tfvars", "infra/prod.tfvars", "db_password = \"x\"\n"},
		{"*.tfvars.json", "infra/prod.tfvars.json", "{}\n"},
		{"*.tfstate", "infra/terraform.tfstate", "{}\n"},
		{"*.tfstate.backup", "infra/terraform.tfstate.backup", "{}\n"},
		{"*.sqlite", "app.sqlite", "x\n"},
		{"*.db", "app.db", "x\n"},
		{"*.dump", "x.dump", "x\n"},
		{"*.sql.gz", "x.sql.gz", "x\n"},
		{"*.min.js", "app.min.js", "x\n"},
		{"*.min.css", "app.min.css", "x\n"},
		{"*.map", "app.js.map", "x\n"},
		// serviceAccountKey: by content, under any .json name
		{"service-account key", "example-project-0123456789ab.json", fakeServiceAccountKey},
	}
	for _, c := range cases {
		t.Run(c.pattern, func(t *testing.T) {
			dir := newRepo(t)
			writeFile(t, dir, c.file, c.content)
			p, err := Collect(ctx, dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, s := range p.Skipped {
				if s.Path == c.file && s.Reason == ReasonExcluded {
					found = true
				}
			}
			if !found {
				t.Errorf("%s is not in Skipped as excluded: %v", c.file, p.Skipped)
			}
			if _, ok := p.Files[c.file]; ok {
				t.Errorf("%s is in Files", c.file)
			}
			if strings.Contains(p.Diff, c.file) {
				t.Errorf("%s is in the diff", c.file)
			}
		})
	}
}

// TestNeverSentPatternsKeepLookalikes guards against the patterns growing too
// wide: these share a part of a never-sent name and must still be reviewed.
func TestNeverSentPatternsKeepLookalikes(t *testing.T) {
	ctx := context.Background()
	for file, content := range map[string]string{
		"config.json":         "{}\n",
		"app/config.json":     "{}\n",
		"docs/environment.md": "x\n",
		"keyboard.go":         "package k\n",
		"terraform/main.tf":   "x\n",
		"sa.json":             "{\"type\": \"service_account\", \"client_email\": \"x\"}\n",
	} {
		t.Run(file, func(t *testing.T) {
			dir := newRepo(t)
			writeFile(t, dir, file, content)
			p, err := Collect(ctx, dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := p.Files[file]; !ok {
				t.Errorf("%s was dropped: %v", file, p.Skipped)
			}
		})
	}
}

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

	t.Run("excluded_file_is_never_read", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads unreadable files")
		}
		dir := newRepo(t)
		writeFile(t, dir, ".npmrc", "//registry.npmjs.org/:_authToken=x\n")
		writeFile(t, dir, "a.go", "package a\n")
		// Unreadable: Collect fails if it opens the file before excluding it.
		if err := os.Chmod(filepath.Join(dir, ".npmrc"), 0); err != nil {
			t.Fatal(err)
		}
		p, err := Collect(ctx, dir, Options{})
		if err != nil {
			t.Fatalf("an excluded file was opened: %v", err)
		}
		if !skipped(p, ".npmrc", "excluded") {
			t.Errorf("not skipped: %v", p.Skipped)
		}
	})

	t.Run("normal_file_kept", func(t *testing.T) {
		p := collectNew(t, "main.go", "package main\n")
		if p.Files["main.go"] != "package main\n" || len(p.Skipped) != 0 || !strings.Contains(p.Diff, "+package main") {
			t.Errorf("payload: %+v", p)
		}
	})
}
