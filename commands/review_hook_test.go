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
		if !strings.HasPrefix(string(b), "#!/bin/sh") || !strings.Contains(string(b), `set -- --fail-on critical --head "$local_sha"`) || !strings.Contains(string(b), `taufinity review "$@"`) {
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
		testGit(t, dir, "add", "-A")
		testGit(t, dir, "commit", "-q", "-m", "wip")
		stdin := "refs/heads/feat " + gitOut(t, dir, "rev-parse", "HEAD") + " refs/heads/feat " + zeroSHA + "\n"
		bin := t.TempDir()
		for status, want := range map[string]int{"0": 0, "1": 1, "2": 0} {
			script := "#!/bin/sh\nexit " + status + "\n"
			if err := os.WriteFile(filepath.Join(bin, "taufinity"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(hook)
			cmd.Dir = dir
			cmd.Stdin = strings.NewReader(stdin)
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + os.Getenv("HOME")}
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

const zeroSHA = "0000000000000000000000000000000000000000"

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// TestHookReviewsWhatIsPushed runs the installed hook with the ref lines git
// writes to a pre-push hook and a stand-in taufinity that records its
// arguments. The review must be of the pushed range (--head local sha, --base
// remote sha), never the working tree, and once per pushed ref.
func TestHookReviewsWhatIsPushed(t *testing.T) {
	dir := newReviewTestRepo(t)
	testGit(t, dir, "add", "-A")
	testGit(t, dir, "commit", "-q", "-m", "feat")
	putFile(t, dir, "wip.go", "package main // uncommitted\n")
	local := gitOut(t, dir, "rev-parse", "HEAD")
	remote := gitOut(t, dir, "rev-parse", "main")
	unknown := "1111111111111111111111111111111111111111" // not fetched here

	if _, _, err := runReviewIn(t, dir, "review", "install-hook"); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-push")

	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	// The stand-in drains its stdin like a program that reads it would: if the
	// hook let it inherit the ref lines, the later refs would never be seen.
	fake := "#!/bin/sh\necho \"$@\" >> " + log + "\ncat >/dev/null\nexit ${FAKE_EXIT:-0}\n"
	if err := os.WriteFile(filepath.Join(bin, "taufinity"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, stdin, exit string) (string, int) {
		t.Helper()
		_ = os.Remove(log)
		cmd := exec.Command(hook)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + os.Getenv("HOME"), "FAKE_EXIT=" + exit,
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"}
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(log)
		return string(b), code
	}

	t.Run("one_review_per_ref_of_the_pushed_range", func(t *testing.T) {
		stdin := strings.Join([]string{
			"refs/heads/feat " + local + " refs/heads/feat " + remote,  // update: range remote..local
			"refs/heads/new " + local + " refs/heads/new " + zeroSHA,   // new branch: default base
			"(delete) " + zeroSHA + " refs/heads/old " + remote,        // delete: nothing to review
			"refs/heads/feat " + local + " refs/heads/feat " + unknown, // remote not fetched: default base
		}, "\n") + "\n"
		calls, code := run(t, stdin, "0")
		want := strings.Join([]string{
			"review --fail-on critical --head " + local + " --base " + remote,
			"review --fail-on critical --head " + local,
			"review --fail-on critical --head " + local,
		}, "\n") + "\n"
		if calls != want || code != 0 {
			t.Errorf("exit %d, calls:\n%s\nwant:\n%s", code, calls, want)
		}
	})

	t.Run("a_finding_stops_the_push", func(t *testing.T) {
		calls, code := run(t, "refs/heads/feat "+local+" refs/heads/feat "+remote+"\nrefs/heads/new "+local+" refs/heads/new "+zeroSHA+"\n", "1")
		if code != 1 || strings.Count(calls, "review") != 1 {
			t.Errorf("exit %d after calls:\n%s", code, calls)
		}
	})

	t.Run("nothing_pushed_nothing_reviewed", func(t *testing.T) {
		if calls, code := run(t, "", "0"); calls != "" || code != 0 {
			t.Errorf("exit %d, calls %q", code, calls)
		}
	})
}

// TestReviewHeadLeavesOutTheWorkingTree runs the command the hook runs, with
// --dry-run, and checks the uncommitted change and the untracked file are not
// counted: only the committed file is.
func TestReviewHeadLeavesOutTheWorkingTree(t *testing.T) {
	dir := newReviewTestRepo(t)
	testGit(t, dir, "add", "-A")
	testGit(t, dir, "commit", "-q", "-m", "feat")
	putFile(t, dir, "main.go", "package main\n// uncommitted\n")
	putFile(t, dir, "wip.go", "package main\n")
	head := gitOut(t, dir, "rev-parse", "HEAD")

	out, _, err := runReviewIn(t, dir, "--dry-run", "review", "--head", head, "--base", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "files: 1\n") {
		t.Errorf("want only the committed main.go:\n%s", out)
	}
	all, _, err := runReviewIn(t, dir, "--dry-run", "review", "--base", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "files: 2\n") {
		t.Errorf("without --head the working tree is reviewed (main.go and wip.go):\n%s", all)
	}
}

func TestInstallHookForceReplacesSymlink(t *testing.T) {
	dir := newReviewTestRepo(t)
	hooks := filepath.Join(dir, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(hooks, "pre-push")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runReviewIn(t, dir, "review", "install-hook", "--force"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("symlink target overwritten: %q", b)
	}
}
