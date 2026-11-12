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
	Long: `Write a pre-push hook that runs 'taufinity review --fail-on critical'.

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

const reviewHookScript = `#!/bin/sh
# Installed by 'taufinity review install-hook'. Skip once with: git push --no-verify
if ! command -v taufinity >/dev/null 2>&1; then
  echo "taufinity is not installed, skipping the pre-push review" >&2
  exit 0
fi
taufinity review --fail-on critical
status=$?
if [ "$status" -eq 1 ]; then
  exit 1
fi
if [ "$status" -ne 0 ]; then
  echo "the pre-push review could not run (exit $status), pushing anyway" >&2
fi
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
