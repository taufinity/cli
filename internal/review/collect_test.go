package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectDiff(t *testing.T) {
	ctx := context.Background()

	// featureRepo: main has README.md; feat has one commit adding committed.txt.
	featureRepo := func(t *testing.T) string {
		dir := newRepo(t)
		gitRun(t, dir, "checkout", "-q", "-b", "feat")
		writeFile(t, dir, "committed.txt", "committed\n")
		commitAll(t, dir, "feature")
		return dir
	}

	t.Run("committed_and_uncommitted", func(t *testing.T) {
		dir := featureRepo(t)
		writeFile(t, dir, "README.md", "hello\nchanged\n")
		p, err := Collect(ctx, dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"+++ b/committed.txt", "+changed"} {
			if !strings.Contains(p.Diff, want) {
				t.Errorf("diff lacks %q:\n%s", want, p.Diff)
			}
		}
		if p.Mode != "diff" || p.Title != "feat" || p.BaseRef != "main" || len(p.HeadSHA) != 40 {
			t.Errorf("metadata: %+v", p)
		}
		if p.Files["committed.txt"] != "committed\n" || p.Files["README.md"] != "hello\nchanged\n" {
			t.Errorf("files: %v", p.Files)
		}
	})

	t.Run("untracked_included", func(t *testing.T) {
		dir := featureRepo(t)
		writeFile(t, dir, "new/fresh.go", "package fresh\n")
		p, err := Collect(ctx, dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Diff, "new file mode") || !strings.Contains(p.Diff, "+++ b/new/fresh.go") || p.Files["new/fresh.go"] == "" {
			t.Errorf("untracked file missing:\n%s", p.Diff)
		}
	})

	t.Run("ignored_excluded", func(t *testing.T) {
		dir := featureRepo(t)
		writeFile(t, dir, ".gitignore", "secret.log\n")
		writeFile(t, dir, "secret.log", "ignored\n")
		p, err := Collect(ctx, dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p.Diff, "b/secret.log") || p.Files["secret.log"] != "" {
			t.Errorf("an ignored file was collected:\n%s", p.Diff)
		}
	})

	t.Run("staged_only", func(t *testing.T) {
		dir := featureRepo(t)
		writeFile(t, dir, "staged.txt", "staged\n")
		gitRun(t, dir, "add", "staged.txt")
		writeFile(t, dir, "unstaged.txt", "unstaged\n")
		writeFile(t, dir, "README.md", "hello\nworktree only\n")
		p, err := Collect(ctx, dir, Options{Staged: true})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Diff, "staged.txt") || strings.Contains(p.Diff, "unstaged.txt") || strings.Contains(p.Diff, "worktree only") {
			t.Errorf("staged diff wrong:\n%s", p.Diff)
		}
	})

	t.Run("paths_narrow_the_diff", func(t *testing.T) {
		dir := featureRepo(t)
		writeFile(t, dir, "src/a.go", "package a\n")
		writeFile(t, dir, "docs/b.md", "b\n")
		p, err := Collect(ctx, dir, Options{Paths: []string{"src"}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Diff, "src/a.go") || strings.Contains(p.Diff, "docs/b.md") || strings.Contains(p.Diff, "committed.txt") {
			t.Errorf("paths did not narrow:\n%s", p.Diff)
		}
		if _, ok := p.Files["docs/b.md"]; ok {
			t.Error("files outside the paths were collected")
		}
	})

	t.Run("paths_are_relative_to_dir", func(t *testing.T) {
		dir := featureRepo(t)
		writeFile(t, dir, "src/a.go", "package a\n")
		writeFile(t, dir, "docs/b.md", "b\n")
		p, err := Collect(ctx, filepath.Join(dir, "src"), Options{Paths: []string{"."}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Diff, "src/a.go") || strings.Contains(p.Diff, "docs/b.md") {
			t.Errorf("relative path wrong:\n%s", p.Diff)
		}
	})

	t.Run("deleted_file_not_in_files", func(t *testing.T) {
		dir := featureRepo(t)
		if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
			t.Fatal(err)
		}
		p, err := Collect(ctx, dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Diff, "deleted file mode") {
			t.Errorf("deletion missing from the diff:\n%s", p.Diff)
		}
		if _, ok := p.Files["README.md"]; ok {
			t.Error("a deleted file must not be in Files")
		}
	})

	t.Run("no_changes_empty", func(t *testing.T) {
		dir := newRepo(t)
		p, err := Collect(ctx, dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Diff != "" || len(p.Files) != 0 {
			t.Errorf("expected nothing, got %+v", p)
		}
	})

	t.Run("works_with_bitbucket_remote", func(t *testing.T) {
		dir := featureRepo(t)
		gitRun(t, dir, "remote", "add", "origin", "git@bitbucket.org:acme/site.git")
		p, err := Collect(ctx, dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Repo != "acme/site" || !strings.Contains(p.Diff, "committed.txt") {
			t.Errorf("repo=%q diff=%q", p.Repo, p.Diff)
		}
	})

	t.Run("secrets_masked_and_counted", func(t *testing.T) {
		dir := featureRepo(t)
		writeFile(t, dir, "cfg.txt", "t=ghp_abcdefghijklmnopqrstuvwxyz0123456789\n") // gitleaks:allow -- clearly fake test value
		p, err := Collect(ctx, dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p.Diff, "ghp_") || strings.Contains(p.Files["cfg.txt"], "ghp_") || p.Masked != 2 {
			t.Errorf("masked=%d diff=%q", p.Masked, p.Diff)
		}
	})

	t.Run("files_over_budget_refused", func(t *testing.T) {
		// Small changes to many big files: the diff is tiny, the contents are not.
		dir := newRepo(t)
		big := strings.Repeat("0123456789abcdef\n", 55000) // about 880 KB
		for i := 0; i < 8; i++ {
			writeFile(t, dir, "m"+string(rune('a'+i))+".txt", big)
		}
		commitAll(t, dir, "big")
		gitRun(t, dir, "checkout", "-q", "-b", "feat")
		for i := 0; i < 8; i++ {
			writeFile(t, dir, "m"+string(rune('a'+i))+".txt", big+"x\n")
		}
		_, err := Collect(ctx, dir, Options{})
		if err == nil || !strings.Contains(err.Error(), "narrow") || !strings.Contains(err.Error(), "files to send") {
			t.Fatalf("expected the files budget to refuse this, got %v", err)
		}
	})

	t.Run("non_ascii_paths_are_not_quoted", func(t *testing.T) {
		dir := featureRepo(t)
		writeFile(t, dir, "caf\u00e9.txt", "x\n")
		gitRun(t, dir, "add", "-A")
		p, err := Collect(ctx, dir, Options{Staged: true})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Diff, "+++ b/caf\u00e9.txt") {
			t.Errorf("diff header is quoted:\n%s", p.Diff)
		}
	})

	t.Run("diff_over_limit_refused", func(t *testing.T) {
		dir := featureRepo(t)
		for i := 0; i < 3; i++ {
			writeFile(t, dir, "big"+string(rune('a'+i))+".txt", strings.Repeat("0123456789abcdef\n", 40000))
		}
		if _, err := Collect(ctx, dir, Options{}); err == nil || !strings.Contains(err.Error(), "narrow") {
			t.Fatalf("expected a narrowing hint, got %v", err)
		}
	})
}

func TestCollectWhole(t *testing.T) {
	ctx := context.Background()

	t.Run("unchanged_files_included", func(t *testing.T) {
		dir := newRepo(t)
		writeFile(t, dir, "src/old.go", "package old\n")
		commitAll(t, dir, "old")
		writeFile(t, dir, "src/new.go", "package nw\n")
		p, err := Collect(ctx, dir, Options{Whole: true, Paths: []string{"src"}})
		if err != nil {
			t.Fatal(err)
		}
		if p.Mode != "whole" || p.Files["src/old.go"] == "" || p.Files["src/new.go"] == "" || p.Files["README.md"] != "" {
			t.Errorf("payload: mode=%s files=%v", p.Mode, p.Files)
		}
	})

	t.Run("requires_paths", func(t *testing.T) {
		dir := newRepo(t)
		if _, err := Collect(ctx, dir, Options{Whole: true}); err == nil || !strings.Contains(err.Error(), "path") {
			t.Fatalf("expected a path error, got %v", err)
		}
	})

	t.Run("too_large_refused", func(t *testing.T) {
		dir := newRepo(t)
		for i := 0; i < 3; i++ {
			writeFile(t, dir, "big/f"+string(rune('a'+i))+".txt", strings.Repeat("0123456789abcdef\n", 40000))
		}
		_, err := Collect(ctx, dir, Options{Whole: true, Paths: []string{"big"}})
		if err == nil || !strings.Contains(err.Error(), "1572864") || !strings.Contains(err.Error(), "narrow") {
			t.Fatalf("expected the size and a narrowing hint, got %v", err)
		}
	})

	t.Run("diff_is_new_file_format", func(t *testing.T) {
		dir := newRepo(t)
		writeFile(t, dir, "a.txt", "one\ntwo\n")
		p, err := Collect(ctx, dir, Options{Whole: true, Paths: []string{"a.txt"}})
		if err != nil {
			t.Fatal(err)
		}
		want := "diff --git a/a.txt b/a.txt\nnew file mode 100644\n--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1,2 @@\n+one\n+two\n"
		if p.Diff != want {
			t.Errorf("diff:\n%q\nwant\n%q", p.Diff, want)
		}
	})
}

// TestCollectHead covers --head, which the pre-push hook uses: only the
// commits up to the given revision are reviewed, never the working tree.
func TestCollectHead(t *testing.T) {
	ctx := context.Background()

	// pushRepo: main has README.md; feat has two commits (one.txt, two.txt),
	// then an uncommitted edit to one.txt and an untracked wip.txt.
	pushRepo := func(t *testing.T) (dir, first string) {
		dir = newRepo(t)
		gitRun(t, dir, "checkout", "-q", "-b", "feat")
		writeFile(t, dir, "one.txt", "committed one\n")
		commitAll(t, dir, "one")
		first = gitRun(t, dir, "rev-parse", "HEAD")
		writeFile(t, dir, "two.txt", "committed two\n")
		commitAll(t, dir, "two")
		writeFile(t, dir, "one.txt", "committed one\nUNCOMMITTED\n")
		writeFile(t, dir, "wip.txt", "UNTRACKED\n")
		return dir, first
	}

	t.Run("leaves_out_uncommitted_and_untracked", func(t *testing.T) {
		dir, _ := pushRepo(t)
		head := gitRun(t, dir, "rev-parse", "HEAD")
		p, err := Collect(ctx, dir, Options{Head: head})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"+++ b/one.txt", "+++ b/two.txt"} {
			if !strings.Contains(p.Diff, want) {
				t.Errorf("diff lacks %q:\n%s", want, p.Diff)
			}
		}
		if strings.Contains(p.Diff, "UNCOMMITTED") || strings.Contains(p.Diff, "UNTRACKED") {
			t.Errorf("working tree content in the diff:\n%s", p.Diff)
		}
		if p.Files["one.txt"] != "committed one\n" {
			t.Errorf("one.txt must be the committed content, got %q", p.Files["one.txt"])
		}
		if _, ok := p.Files["wip.txt"]; ok {
			t.Error("an untracked file was sent")
		}
		if p.HeadSHA != head || p.BaseRef != "main" {
			t.Errorf("HeadSHA=%q BaseRef=%q", p.HeadSHA, p.BaseRef)
		}
	})

	t.Run("range_from_the_remote_sha", func(t *testing.T) {
		dir, first := pushRepo(t)
		p, err := Collect(ctx, dir, Options{Head: "HEAD", Base: first})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p.Diff, "one.txt") || !strings.Contains(p.Diff, "+++ b/two.txt") {
			t.Errorf("want only the commit after the remote sha:\n%s", p.Diff)
		}
	})

	t.Run("a_branch_that_is_not_checked_out", func(t *testing.T) {
		dir, _ := pushRepo(t)
		gitRun(t, dir, "branch", "other", "main")
		gitRun(t, dir, "stash", "-q", "-u")
		gitRun(t, dir, "checkout", "-q", "other")
		writeFile(t, dir, "other.txt", "other\n")
		commitAll(t, dir, "other")
		gitRun(t, dir, "checkout", "-q", "feat")
		p, err := Collect(ctx, dir, Options{Head: "other", Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Diff, "+++ b/other.txt") || strings.Contains(p.Diff, "two.txt") {
			t.Errorf("want only the other branch's commit:\n%s", p.Diff)
		}
	})

	t.Run("deleted_file_in_range", func(t *testing.T) {
		dir, _ := pushRepo(t)
		gitRun(t, dir, "rm", "-q", "README.md")
		commitAll(t, dir, "rm")
		p, err := Collect(ctx, dir, Options{Head: "HEAD"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Diff, "-hello") {
			t.Errorf("deletion missing:\n%s", p.Diff)
		}
	})

	t.Run("refuses_bad_combinations", func(t *testing.T) {
		dir, _ := pushRepo(t)
		for name, o := range map[string]Options{
			"staged":  {Head: "HEAD", Staged: true},
			"whole":   {Head: "HEAD", Whole: true, Paths: []string{"."}},
			"option":  {Head: "--output=/tmp/x"},
			"unknown": {Head: "nope"},
		} {
			if _, err := Collect(ctx, dir, o); err == nil {
				t.Errorf("%s: expected an error", name)
			}
		}
	})
}
