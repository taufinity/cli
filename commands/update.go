package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/buildinfo"
	"github.com/taufinity/cli/internal/pixl"
	"github.com/taufinity/cli/internal/telemetry"
	"github.com/taufinity/cli/internal/updatecheck"
)

const (
	releasesAPIURL  = "https://api.github.com/repos/taufinity/cli/releases/latest"
	downloadTimeout = 5 * time.Minute

	// smokeTestTimeout caps how long we let a binary run `version`.
	// Should return in milliseconds; 3s is generous.
	smokeTestTimeout = 3 * time.Second
)

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

var (
	flagUpdateCheckOnly bool
	flagUpdateRollback  bool
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update taufinity to the latest version",
	Long: `Update downloads and installs the latest taufinity release from GitHub.

Behaviour:
 1. Queries GitHub Releases for the latest version.
 2. Downloads the binary for your platform (OS + architecture).
 3. Verifies the SHA256 checksum against the release checksums file.
 4. Backs up the running binary to <path>.prev.
 5. Replaces the running binary in-place.
 6. Smoke-tests the new binary; on failure restores the backup automatically.

Flags:
  --check     Report current vs latest without installing. Exits 0 if up to
              date, 1 if behind, 2 on network error.
  --rollback  Restore the previous binary from <path>.prev. Useful if the new
              version misbehaves.`,
	RunE: runUpdate,
	Annotations: map[string]string{
		"suppress-update-warning": "true",
	},
}

func init() {
	rootCmd.AddCommand(updateCmd)
	updateCmd.Flags().BoolVar(&flagUpdateCheckOnly, "check", false, "Report version status without installing (exit: 0=current, 1=behind, 2=error)")
	updateCmd.Flags().BoolVar(&flagUpdateRollback, "rollback", false, "Restore the previous binary from <path>.prev")
}

func runUpdate(cmd *cobra.Command, _ []string) error {
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

func runCheck(cmd *cobra.Command) error {
	current := buildinfo.FromBuildtime(Version, GitCommit, BuildTime)
	if current.Dirty {
		Print("Running a dirty build (%s) — staleness check skipped.\n", current.Version)
		return nil
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), updatecheck.DefaultHTTPTimeout)
	defer cancel()

	rel, err := fetchLatestRelease(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to query GitHub: %v\n", err)
		os.Exit(2)
		return nil
	}

	currentVer := current.Version
	latestVer := rel.TagName

	if currentVer == latestVer {
		Print("taufinity %s is up to date.\n", currentVer)
		return nil
	}

	// Dev or pseudo-version builds can't be compared to a release tag.
	if currentVer == "dev" || strings.Contains(currentVer, "(") {
		Print("taufinity is running from source (%s). Latest release: %s\n", currentVer, latestVer)
		return nil
	}

	Print("taufinity is behind: %s → %s. Run: taufinity update\n", currentVer, latestVer)
	os.Exit(1)
	return nil
}

func runInstall(cmd *cobra.Command) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	rel, err := fetchLatestRelease(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("fetch latest release: %w", err)
	}

	current := buildinfo.FromBuildtime(Version, GitCommit, BuildTime)
	Print("Updating from %s\n", current.Version)

	if current.Version == rel.TagName {
		Print("Already up to date at %s.\n", current.Version)
		return nil
	}

	return runDownload(cmd, rel)
}

func runDownload(cmd *cobra.Command, rel *githubRelease) error {
	want := platformAssetName()

	var binaryAsset, checksumAsset *githubAsset
	for i := range rel.Assets {
		switch rel.Assets[i].Name {
		case want:
			binaryAsset = &rel.Assets[i]
		case "checksums.txt":
			checksumAsset = &rel.Assets[i]
		}
	}
	if binaryAsset == nil {
		return fmt.Errorf("no release asset for this platform (%s) in tag %s", want, rel.TagName)
	}

	// Fetch expected checksum before downloading the binary.
	expectedHash := ""
	if checksumAsset != nil {
		csCtx, csCancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		data, err := downloadBytes(csCtx, checksumAsset.BrowserDownloadURL)
		csCancel()
		if err == nil {
			expectedHash = parseChecksum(data, want)
		}
	}

	// Create temp file in the same directory as the running binary so the
	// final rename is on the same filesystem and therefore atomic.
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	currentExe, _ = filepath.EvalSymlinks(currentExe)
	installDir := filepath.Dir(currentExe)

	tmpFile, err := os.CreateTemp(installDir, ".taufinity-update-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", installDir, err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	fmt.Fprintf(cmd.OutOrStdout(), "Downloading %s (%s)…\n", want, rel.TagName)
	dlCtx, dlCancel := context.WithTimeout(cmd.Context(), downloadTimeout)
	gotHash, err := downloadToFile(dlCtx, binaryAsset.BrowserDownloadURL, tmpFile)
	_ = tmpFile.Close()
	dlCancel()
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	if expectedHash != "" {
		if !strings.EqualFold(gotHash, expectedHash) {
			return fmt.Errorf("checksum mismatch: got %s, want %s", gotHash, expectedHash)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "✓ Checksum verified\n")
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Smoke-test the downloaded binary before touching the live one.
	if err := smokeTest(tmpPath); err != nil {
		return fmt.Errorf("downloaded binary failed smoke test: %w", err)
	}

	// Backup then replace.
	backupPath := currentExe + ".prev"
	if err := backupBinary(currentExe, backupPath); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Backed up current binary to %s\n", backupPath)

	if err := os.Rename(tmpPath, currentExe); err != nil {
		// Same-dir temp should always be on the same device, but handle the edge case.
		if copyErr := copyFile(tmpPath, currentExe); copyErr != nil {
			_ = restoreBackup(backupPath, currentExe)
			return fmt.Errorf("install binary (rename: %v, copy: %w)", err, copyErr)
		}
	}

	if err := os.Chmod(currentExe, 0o755); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: chmod %s: %v\n", currentExe, err)
	}

	// Final smoke test on the live binary.
	if err := smokeTest(currentExe); err != nil {
		telemetry.Report(telemetry.Event{
			EventType:    "update.failure",
			ErrorCode:    "smoke_test_post_install",
			ErrorMessage: err.Error(),
		})
		pixl.Fire("v1/update_error", map[string]string{"code": "smoke_test_post_install", "to": rel.TagName})
		fmt.Fprintf(cmd.ErrOrStderr(), "Post-install smoke test failed: %v\n", err)
		if rerr := restoreBackup(backupPath, currentExe); rerr != nil {
			return fmt.Errorf("smoke test failed AND rollback failed: %w (original: %v)", rerr, err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Rolled back to previous binary.\n")
		pixl.Flush(2 * time.Second)
		telemetry.Flush()
		os.Exit(1)
		return nil
	}

	Print("Updated to %s. Run 'taufinity version' to confirm.\n", rel.TagName)
	pixl.Fire("v1/updated", map[string]string{"from": Version, "to": rel.TagName})
	return nil
}

func runRollback() error {
	currentExe, err := os.Executable()
	if err != nil {
		return err
	}
	currentExe, _ = filepath.EvalSymlinks(currentExe)
	backupPath := currentExe + ".prev"

	if _, err := os.Stat(backupPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("nothing to roll back: no backup at %s. (Backups are created automatically on 'taufinity update'.)", backupPath)
		}
		return err
	}

	if err := restoreBackup(backupPath, currentExe); err != nil {
		return fmt.Errorf("restore: %w", err)
	}

	Print("Rolled back to previous binary at %s. Run 'taufinity version' to confirm.\n", currentExe)
	return nil
}

// fetchLatestRelease queries the GitHub Releases API for the newest release.
func fetchLatestRelease(ctx context.Context) (*githubRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPIURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "taufinity-cli-update")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("github returned %d: %s", resp.StatusCode, string(body))
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, errors.New("github returned empty tag_name")
	}
	return &rel, nil
}

// platformAssetName returns the expected filename for the current OS + arch.
func platformAssetName() string {
	name := fmt.Sprintf("taufinity_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// downloadBytes fetches url and returns the body (capped at 1 MiB).
func downloadBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "taufinity-cli-update")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// downloadToFile streams url into dst and returns the lowercase hex SHA256 of
// the downloaded content.
func downloadToFile(ctx context.Context, url string, dst *os.File) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "taufinity-cli-update")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseChecksum finds the SHA256 for filename in a sha256sum-style file.
// Line format: "<hash>  <filename>" (two spaces) or "<hash> <filename>".
func parseChecksum(data []byte, filename string) string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == filename {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

// smokeTest runs binPath with `version` and checks for exit 0.
func smokeTest(binPath string) error {
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("binary not found at %s: %w", binPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), smokeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "version")
	cmd.Env = append(os.Environ(), updatecheck.EnvDisable+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: output: %s", err, string(out))
	}
	return nil
}

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

func restoreBackup(src, dst string) error {
	return os.Rename(src, dst)
}
