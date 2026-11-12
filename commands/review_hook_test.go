package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHook(t *testing.T) {
	t.Run("writes_executable_hook", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		out, _, err := runReviewIn(t, dir, "review", "install-hook")
		if err != nil {
			t.Fatal(err)
		}
		hook := filepath.Join(dir, ".git", "hooks", "pre-push")
		fi, err := os.Stat(hook)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("hook is not executable: %v", fi.Mode())
		}
		b, _ := os.ReadFile(hook)
		if !strings.HasPrefix(string(b), "#!/bin/sh") || !strings.Contains(string(b), "taufinity review --fail-on critical") {
			t.Errorf("hook content:\n%s", b)
		}
		if !strings.Contains(out, "pre-push") {
			t.Errorf("stdout %q", out)
		}
	})

	t.Run("refuses_existing", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		hook := filepath.Join(dir, ".git", "hooks", "pre-push")
		if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(hook, []byte("#!/bin/sh\necho mine\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, _, err := runReviewIn(t, dir, "review", "install-hook")
		if err == nil || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("expected a refusal naming --force, got %v", err)
		}
		if b, _ := os.ReadFile(hook); !strings.Contains(string(b), "echo mine") {
			t.Error("the existing hook was changed")
		}
	})

	t.Run("force_overwrites", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		hook := filepath.Join(dir, ".git", "hooks", "pre-push")
		_ = os.MkdirAll(filepath.Dir(hook), 0o755)
		if err := os.WriteFile(hook, []byte("#!/bin/sh\necho mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runReviewIn(t, dir, "review", "install-hook", "--force"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(hook)
		fi, _ := os.Stat(hook)
		if strings.Contains(string(b), "echo mine") || fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("not overwritten as an executable: %v\n%s", fi.Mode(), b)
		}
	})

	t.Run("works_in_worktree", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		testGit(t, dir, "add", "-A")
		testGit(t, dir, "commit", "-q", "-m", "wip")
		wt := filepath.Join(t.TempDir(), "wt")
		testGit(t, dir, "worktree", "add", "-q", "-b", "other", wt)
		if _, _, err := runReviewIn(t, wt, "review", "install-hook"); err != nil {
			t.Fatal(err)
		}
		// Hooks are shared: they live in the main repository's git dir.
		if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "pre-push")); err != nil {
			t.Fatalf("hook not in the shared hooks dir: %v", err)
		}
	})

	t.Run("hook_stops_the_push_only_on_findings", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		if _, _, err := runReviewIn(t, dir, "review", "install-hook"); err != nil {
			t.Fatal(err)
		}
		hook := filepath.Join(dir, ".git", "hooks", "pre-push")
		bin := t.TempDir()
		for status, want := range map[string]int{"0": 0, "1": 1, "2": 0} {
			script := "#!/bin/sh\nexit " + status + "\n"
			if err := os.WriteFile(filepath.Join(bin, "taufinity"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(hook)
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
			err := cmd.Run()
			got := 0
			if ee, ok := err.(*exec.ExitError); ok {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("review exit %s: hook exit %d, want %d", status, got, want)
			}
		}
	})
}
