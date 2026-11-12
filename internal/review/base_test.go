package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs git in dir with a throwaway identity and no global config.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a repository on branch main with one commit (README.md).
func newRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "config", "user.email", "t@example.invalid")
	writeFile(t, dir, "README.md", "hello\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", msg)
}

func TestResolveBase(t *testing.T) {
	ctx := context.Background()

	t.Run("explicit_wins", func(t *testing.T) {
		dir := newRepo(t)
		gitRun(t, dir, "branch", "other")
		gitRun(t, dir, "checkout", "-q", "-b", "feat")
		writeFile(t, dir, "a.txt", "a\n")
		commitAll(t, dir, "a")
		ref, mb, err := ResolveBase(ctx, dir, "other")
		if err != nil || ref != "other" {
			t.Fatalf("ref=%q err=%v", ref, err)
		}
		if want := gitRun(t, dir, "rev-parse", "other"); mb != want {
			t.Fatalf("merge base %q, want %q", mb, want)
		}
	})

	t.Run("upstream_first", func(t *testing.T) {
		dir := newRepo(t)
		gitRun(t, dir, "branch", "up")
		gitRun(t, dir, "checkout", "-q", "-b", "feat")
		gitRun(t, dir, "branch", "--set-upstream-to=up")
		ref, _, err := ResolveBase(ctx, dir, "")
		if err != nil || ref != "up" {
			t.Fatalf("ref=%q err=%v, want the upstream", ref, err)
		}
	})

	t.Run("origin_main_fallback", func(t *testing.T) {
		dir := newRepo(t)
		gitRun(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		gitRun(t, dir, "checkout", "-q", "-b", "feat")
		ref, _, err := ResolveBase(ctx, dir, "")
		if err != nil || ref != "origin/main" {
			t.Fatalf("ref=%q err=%v, want origin/main", ref, err)
		}
	})

	t.Run("main_fallback", func(t *testing.T) {
		dir := newRepo(t)
		gitRun(t, dir, "checkout", "-q", "-b", "feat")
		ref, mb, err := ResolveBase(ctx, dir, "")
		if err != nil || ref != "main" || mb == "" {
			t.Fatalf("ref=%q mb=%q err=%v, want main", ref, mb, err)
		}
	})

	t.Run("none_found_error", func(t *testing.T) {
		dir := newRepo(t)
		gitRun(t, dir, "branch", "-m", "trunk")
		_, _, err := ResolveBase(ctx, dir, "")
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{"upstream", "origin/main", "main", "--base"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
		if _, _, err := ResolveBase(ctx, dir, "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
			t.Errorf("an unknown explicit base should be named in the error, got %v", err)
		}
	})
}

func TestResolveBaseRejectsOptions(t *testing.T) {
	dir := newRepo(t)
	for _, base := range []string{"--output=/tmp/x", "-h", "--upload-pack=touch /tmp/x"} {
		if _, _, err := ResolveBase(context.Background(), dir, base); err == nil || !strings.Contains(err.Error(), "not a ref") {
			t.Errorf("base %q: want a refusal, got %v", base, err)
		}
	}
}
