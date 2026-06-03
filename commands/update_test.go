package commands

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBackupBinary_Hardlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bin")
	dst := filepath.Join(dir, "bin.prev")

	if err := os.WriteFile(src, []byte("original content"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := backupBinary(src, dst); err != nil {
		t.Fatalf("backupBinary: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original content" {
		t.Errorf("backup content = %q, want original content", got)
	}
}

func TestBackupBinary_OverwritesStalePrev(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bin")
	dst := filepath.Join(dir, "bin.prev")

	if err := os.WriteFile(src, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("v0-stale"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := backupBinary(src, dst); err != nil {
		t.Fatalf("backupBinary: %v", err)
	}

	got, _ := os.ReadFile(dst)
	if string(got) != "v2" {
		t.Errorf("backup = %q, want v2 (stale prev was overwritten)", got)
	}
}

func TestRestoreBackup_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "bin")
	backup := filepath.Join(dir, "bin.prev")

	if err := os.WriteFile(current, []byte("broken-new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("good-old"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := restoreBackup(backup, current); err != nil {
		t.Fatalf("restoreBackup: %v", err)
	}

	got, _ := os.ReadFile(current)
	if string(got) != "good-old" {
		t.Errorf("after restore = %q, want good-old", got)
	}
	// .prev should be gone — rename consumed it.
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Errorf("backup still present after restore: %v", err)
	}
}

func TestPlatformAssetName(t *testing.T) {
	got := platformAssetName()
	want := "taufinity_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got != want {
		t.Errorf("platformAssetName() = %q, want %q", got, want)
	}
}

func TestParseChecksum(t *testing.T) {
	checksums := []byte(
		"aabbcc  taufinity_linux_amd64\n" +
			"ddeeff  taufinity_darwin_arm64\n" +
			"112233  checksums.txt\n",
	)
	tests := []struct {
		filename string
		want     string
	}{
		{"taufinity_linux_amd64", "aabbcc"},
		{"taufinity_darwin_arm64", "ddeeff"},
		{"checksums.txt", "112233"},
		{"taufinity_windows_amd64.exe", ""},
	}
	for _, tt := range tests {
		if got := parseChecksum(checksums, tt.filename); got != tt.want {
			t.Errorf("parseChecksum(%q) = %q, want %q", tt.filename, got, tt.want)
		}
	}
}

func TestRunRollback_NoBackup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	exe, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable")
	}
	_ = os.Remove(exe + ".prev")

	err = runRollback()
	if err == nil {
		t.Fatal("expected error when no .prev exists")
	}
	if !strings.Contains(err.Error(), "no backup") {
		t.Errorf("error message = %q, expected to mention 'no backup'", err.Error())
	}
}
