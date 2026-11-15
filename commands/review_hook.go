package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var reviewHookForce bool

var reviewHookCmd = &cobra.Command{
	Use:   "install-hook",
	Short: "Run `taufinity review` before every git push",
	Long: `Write a pre-push hook that runs 'taufinity review --fail-on critical' on
what is being pushed.

For each pushed ref, the hook reviews the commits from the remote's current
commit up to the local commit that git passes to the hook, with
'taufinity review --head <local sha> --base <remote sha>'. A new branch is
compared with the default base (the upstream, origin/main or main). Uncommitted
and untracked files are never part of it, and deleting a remote branch
reviews nothing.

The push stops when the review finds a critical issue. When the review cannot
run (offline, not signed in, code review not switched on) the hook says so and
lets the push go. An existing pre-push hook is never overwritten unless you pass --force. Skip the hook for one
push with 'git push --no-verify'.`,
	Args: cobra.NoArgs,
	RunE: runReviewInstallHook,
}

func init() {
	reviewCmd.AddCommand(reviewHookCmd)
	reviewHookCmd.Flags().BoolVar(&reviewHookForce, "force", false, "Overwrite an existing pre-push hook")
}

// reviewHookScript reviews what is pushed, not the working tree. git writes
// one line per ref to the hook's stdin: <local ref> <local sha> <remote ref>
// <remote sha>, with an all-zero sha for a ref that does not exist on that
// side (zero local: a delete; zero remote: a new branch).
const reviewHookScript = `#!/bin/sh
# Installed by 'taufinity review install-hook'. Skip once with: git push --no-verify
# Reviews the commits being pushed (remote sha to local sha), never uncommitted files.
if ! command -v taufinity >/dev/null 2>&1; then
  echo "taufinity is not installed, skipping the pre-push review" >&2
  exit 0
fi
while read -r local_ref local_sha remote_ref remote_sha; do
  case "$local_sha" in
    *[!0]*) ;;
    *) continue ;; # deleting a remote ref: nothing to review
  esac
  set -- --fail-on critical --head "$local_sha"
  case "$remote_sha" in
    *[!0]*)
      # The remote commit is the base when it is known here; otherwise (not
      # fetched yet) the review falls back to the upstream, origin/main or main.
      if git cat-file -e "$remote_sha^{commit}" 2>/dev/null; then
        set -- "$@" --base "$remote_sha"
      fi
      ;;
  esac
  # stdin stays with the loop: the review must not consume the ref lines.
  taufinity review "$@" </dev/null
  status=$?
  if [ "$status" -eq 1 ]; then
    exit 1 # the push stops: no need to review the other refs
  fi
  if [ "$status" -ne 0 ]; then
    echo "the pre-push review of $local_ref could not run (exit $status), pushing anyway" >&2
  fi
done
exit 0
`

func runReviewInstallHook(cmd *cobra.Command, args []string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	// --git-path resolves worktrees (the hooks live in the shared git dir) and
	// core.hooksPath.
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-path", "hooks").Output()
	if err != nil {
		return fmt.Errorf("not a git repository: run this inside the repository you want to protect")
	}
	hooksDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(hooksDir) {
		hooksDir = filepath.Join(dir, hooksDir)
	}
	hook := filepath.Join(hooksDir, "pre-push")

	if _, err := os.Lstat(hook); err == nil && !reviewHookForce {
		return fmt.Errorf("%s already exists; pass --force to overwrite it", hook)
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	// Replace, never write through, an existing file or symlink.
	if err := os.Remove(hook); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(hook, []byte(reviewHookScript), 0o755); err != nil {
		return err
	}
	// WriteFile keeps the mode of a file it overwrites.
	if err := os.Chmod(hook, 0o755); err != nil {
		return err
	}
	Print("Installed the pre-push hook at %s\n", hook)
	return nil
}
