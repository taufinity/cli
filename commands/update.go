package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/buildinfo"
	"github.com/taufinity/cli/internal/updatecheck"
)

const updateModulePath = "github.com/taufinity/cli/cmd/taufinity@latest"

// goInstallTimeout is generous because module download + compile can take
// 60s+ on first invocation when GOMODCACHE is cold.
const goInstallTimeout = 5 * time.Minute

// smokeTestTimeout caps how long we let the newly-installed binary run.
// `taufinity version` should return in milliseconds; 3s is forgiving.
const smokeTestTimeout = 3 * time.Second

var (
	flagUpdateCheckOnly bool
	flagUpdateRollback  bool
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update taufinity to the latest version",
	Long: `Update reinstalls taufinity from source via go install.

Behaviour:
 1. Verifies that Go is installed (required to compile).
 2. Backs up the currently-running binary to <path>.prev (when the install
    target directory matches the running binary's directory).
 3. Runs go install github.com/taufinity/cli/cmd/taufinity@latest.
 4. Smoke-tests the new binary by running it with the 'version' subcommand.
    If the smoke test fails, the backup is automatically restored.
 5. Warns if the new binary was installed to a directory other than where the
    currently-running binary lives (your PATH probably needs adjusting, or
    the previous install was via 'make install' to a custom prefix).

Flags:
  --check     Report current vs latest without installing. Exits 0 if up to
              date, 1 if behind, 2 on network error.
  --rollback  Restore the previous binary from <path>.prev. Useful if the new
              version misbehaves.`,
	RunE: runUpdate,
	Annotations: map[string]string{
		// The update flow already prints whatever it needs to; the staleness
		// hint would be redundant (and confusing if the staleness check is
		// what nudged the user to run update in the first place).
		"suppress-update-warning": "true",
	},
}

func init() {
	rootCmd.AddCommand(updateCmd)
	updateCmd.Flags().BoolVar(&flagUpdateCheckOnly, "check", false, "Report version status without installing (exit: 0=current, 1=behind, 2=error)")
	updateCmd.Flags().BoolVar(&flagUpdateRollback, "rollback", false, "Restore the previous binary from <path>.prev")
}

func runUpdate(cmd *cobra.Command, args []string) error {
	if flagUpdateCheckOnly && flagUpdateRollback {
		return errors.New("--check and --rollback cannot be combined")
	}

	if flagUpdateRollback {
		return runRollback()
	}
	if flagUpdateCheckOnly {
		return runCheck(cmd)
	}
	return runInstall(cmd)
}

// runCheck does no installation. It queries GitHub and compares to the running
// binary's commit. Exit codes are script-friendly: 0=current, 1=behind, 2=err.
func runCheck(cmd *cobra.Command) error {
	current := buildinfo.FromBuildtime(Version, GitCommit, BuildTime)
	if current.Dirty {
		Print("Running a dirty build (%s) — staleness check skipped.\n", current.Version)
		return nil
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), updatecheck.DefaultHTTPTimeout)
	defer cancel()

	// We bypass the cache here on purpose — `--check` is an explicit user ask
	// for a fresh answer, not a "use what's cached" call.
	latest, err := fetchLatestSHADirect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to query GitHub: %v\n", err)
		os.Exit(2)
		return nil
	}

	currentShort := shortSHA(current.Commit)
	latestShort := shortSHA(latest)

	if current.Commit == "unknown" {
		fmt.Fprintf(os.Stderr, "Cannot determine current commit (binary built without VCS info). Latest: %s\n", latestShort)
		os.Exit(2)
		return nil
	}

	if sameSHA(current.Commit, latest) {
		Print("taufinity %s is up to date.\n", current.Version)
		return nil
	}

	Print("taufinity is behind: %s → %s. Run: taufinity update\n", currentShort, latestShort)
	os.Exit(1)
	return nil
}

// runInstall is the main update path.
func runInstall(cmd *cobra.Command) error {
	// 1. Pre-flight: is go installed?
	goBin, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("go is not installed or not on PATH. Install Go from https://go.dev/dl/ and try again")
	}

	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	currentExe, _ = filepath.EvalSymlinks(currentExe) // best-effort
	currentDir := filepath.Dir(currentExe)

	current := buildinfo.FromBuildtime(Version, GitCommit, BuildTime)
	Print("Updating from %s (%s)\n", current.Version, shortSHA(current.Commit))

	// 1a. No-op short-circuit: if we already have the latest commit, skip
	// the whole install dance. We bypass the cache here because the user
	// asked explicitly for an update — they want fresh data, not what was
	// true up to 24h ago. A network failure here is NOT fatal: we just
	// proceed with the install (their intent overrides our optimisation).
	if current.Commit != "unknown" && !current.Dirty {
		checkCtx, cancel := context.WithTimeout(cmd.Context(), updatecheck.DefaultHTTPTimeout)
		latest, fetchErr := fetchLatestSHADirect(checkCtx)
		cancel()
		if fetchErr == nil && sameSHA(current.Commit, latest) {
			Print("Already up to date at %s (%s). Skipping install.\n", current.Version, shortSHA(current.Commit))
			return nil
		}
	}

	// 2. Resolve install target dir. We prefer the directory the user is
	// already running taufinity from — if it's writable, we override GOBIN
	// for the `go install` subprocess so the new binary lands in the same
	// place as the running one. That keeps PATH stable across updates and
	// avoids the "installed to ~/go/bin, but you're running from ~/bin"
	// trap that's common when users initially installed via `make install`
	// or a custom prefix.
	var installDir string
	var gobinOverride string
	if isDirWritable(currentDir) {
		installDir = currentDir
		gobinOverride = currentDir
	} else {
		installDir, err = resolveGoInstallDir(goBin)
		if err != nil {
			return fmt.Errorf("resolve go install dir: %w", err)
		}
	}
	newExe := filepath.Join(installDir, binaryName())

	// 1a. Backup, only when the new binary will overwrite the running one.
	var backupPath string
	if pathsEqual(installDir, currentDir) {
		backupPath = currentExe + ".prev"
		if err := backupBinary(currentExe, backupPath); err != nil {
			return fmt.Errorf("backup current binary to %s: %w", backupPath, err)
		}
		Print("Backed up current binary to %s\n", backupPath)
	}

	// 2. go install.
	installCtx, cancel := context.WithTimeout(cmd.Context(), goInstallTimeout)
	defer cancel()
	installCmd := exec.CommandContext(installCtx, goBin, "install", updateModulePath)
	installCmd.Stdout = cmd.OutOrStdout()
	installCmd.Stderr = cmd.ErrOrStderr()
	installCmd.Env = os.Environ()
	if gobinOverride != "" {
		// Last-wins for repeated env keys means appending after os.Environ()
		// reliably overrides any pre-existing GOBIN.
		installCmd.Env = append(installCmd.Env, "GOBIN="+gobinOverride)
	}
	if err := installCmd.Run(); err != nil {
		// go install failed before touching the new binary — leave the
		// backup in place but no need to restore (the original is still in
		// place at currentExe).
		return fmt.Errorf("go install failed: %w", err)
	}

	// 2a. Smoke test.
	if err := smokeTest(newExe); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Smoke test failed for %s: %v\n", newExe, err)
		if backupPath != "" {
			if rerr := restoreBackup(backupPath, currentExe); rerr != nil {
				return fmt.Errorf("smoke test failed AND rollback failed: %w (original error: %v)", rerr, err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Rolled back to previous binary.\n")
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "Note: the running binary at %s is unaffected — only the new install at %s is broken.\n", currentExe, newExe)
		}
		os.Exit(1)
		return nil
	}

	// 3. Path mismatch warning.
	if !pathsEqual(filepath.Dir(currentExe), installDir) {
		fmt.Fprintf(cmd.ErrOrStderr(), `
Installed new taufinity to: %s
But you're running from:    %s
The next 'taufinity' invocation will still use the old binary.
Fix: put %s ahead of %s in your PATH, OR run 'make install' from a clone of
github.com/taufinity/cli to install to %s directly.
`, newExe, currentExe, installDir, filepath.Dir(currentExe), filepath.Dir(currentExe))
		return nil
	}

	Print("Updated. Run 'taufinity version' to confirm.\n")
	return nil
}

// runRollback swaps <currentExe>.prev back into place.
func runRollback() error {
	currentExe, err := os.Executable()
	if err != nil {
		return err
	}
	currentExe, _ = filepath.EvalSymlinks(currentExe)
	backupPath := currentExe + ".prev"

	if _, err := os.Stat(backupPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("nothing to roll back: no backup at %s. (Backups are created automatically on 'taufinity update' when the install dir matches the running binary's dir.)", backupPath)
		}
		return err
	}

	if err := restoreBackup(backupPath, currentExe); err != nil {
		return fmt.Errorf("restore: %w", err)
	}

	Print("Rolled back to previous binary at %s. Run 'taufinity version' to confirm.\n", currentExe)
	return nil
}

// backupBinary writes src to dst, preferring a hardlink (instant on same FS)
// and falling back to a byte copy. The destination is removed first so that a
// stale `.prev` from a previous run is replaced.
func backupBinary(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// restoreBackup atomically swaps src over dst.
func restoreBackup(src, dst string) error {
	// os.Rename is atomic on POSIX when src and dst are on the same filesystem.
	// In our case they're siblings, so they always are.
	return os.Rename(src, dst)
}

// resolveGoInstallDir asks `go env` where the binary will land. GOBIN takes
// precedence; otherwise GOPATH+/bin.
//
// `go env KEY1 KEY2` prints one value per line in argument order — including a
// blank line for empty values. We split first and trim per-line so a blank
// GOBIN doesn't collapse the response down to a single token.
func resolveGoInstallDir(goBin string) (string, error) {
	out, err := exec.Command(goBin, "env", "GOBIN", "GOPATH").Output()
	if err != nil {
		return "", err
	}
	lines := bytes.Split(out, []byte("\n"))
	if len(lines) < 2 {
		return "", fmt.Errorf("unexpected go env output: %q", out)
	}
	gobin := string(bytes.TrimSpace(lines[0]))
	gopath := string(bytes.TrimSpace(lines[1]))
	if gobin != "" {
		return gobin, nil
	}
	if gopath == "" {
		return "", fmt.Errorf("both GOBIN and GOPATH are empty; cannot determine install dir")
	}
	return filepath.Join(gopath, "bin"), nil
}

// isDirWritable reports whether the current process can create files in dir.
// We probe by creating and immediately removing a tempfile — `unix.Access`
// would be faster but isn't portable, and the cost (one syscall pair) is
// invisible compared to the `go install` we're about to run.
func isDirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".taufinity-writetest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// binaryName accounts for Windows .exe suffixes if the CLI ever ships there.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "taufinity.exe"
	}
	return "taufinity"
}

// pathsEqual normalises two directory paths and compares them. We resolve
// symlinks where possible; if a path doesn't exist (or resolution fails) we
// fall back to lexical Clean.
func pathsEqual(a, b string) bool {
	a = cleanDir(a)
	b = cleanDir(b)
	return a == b
}

func cleanDir(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}

// smokeTest runs the new binary with `version` and checks for exit 0 within
// smokeTestTimeout. We deliberately use `version` (not `--help`) because it
// touches the actual package init paths (config load, buildinfo resolution).
func smokeTest(binPath string) error {
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("new binary not found at %s: %w", binPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), smokeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "version")
	// Suppress the staleness check inside the smoke-test child — we don't
	// want it doing network I/O during our test.
	cmd.Env = append(os.Environ(), updatecheck.EnvDisable+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: output: %s", err, string(out))
	}
	return nil
}

// fetchLatestSHADirect duplicates the work of updatecheck.fetchSHA — kept
// here so commands/update doesn't need to expose internal updatecheck APIs.
// For --check we explicitly bypass the cache.
func fetchLatestSHADirect(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updatecheck.DefaultAPIURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "taufinity-cli-update")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("github returned %d: %s", resp.StatusCode, string(body))
	}

	var payload struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.SHA == "" {
		return "", errors.New("github returned empty sha")
	}
	return payload.SHA, nil
}

// shortSHA returns the first 7 chars of a SHA, or the full string if shorter.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func sameSHA(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n < 7 {
		return false
	}
	return equalFoldASCII(a[:n], b[:n])
}

// equalFoldASCII is a fast lowercase-equal for SHAs (no Unicode folding needed).
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
