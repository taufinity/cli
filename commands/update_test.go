package commands

import (
	"os"
	"path/filepath"
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

func TestPathsEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "/usr/local/bin", "/usr/local/bin", true},
		{"trailing slash", "/usr/local/bin", "/usr/local/bin/", true},
		{"different", "/usr/local/bin", "/opt/bin", false},
		{"relative vs absolute", "./bin", "/bin", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathsEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("pathsEqual(%q,%q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("abcdef1234567890"); got != "abcdef1" {
		t.Errorf("shortSHA long = %q, want abcdef1", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA short = %q, want abc", got)
	}
	if got := shortSHA(""); got != "" {
		t.Errorf("shortSHA empty = %q", got)
	}
}

func TestSameSHA(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"both full and equal", "abcdef1234567890abcdef1234567890abcdef12", "abcdef1234567890abcdef1234567890abcdef12", true},
		{"short matches long prefix", "abcdef1", "abcdef1234567890", true},
		{"different SHAs", "abcdef1", "1234567", false},
		{"empty", "", "abc", false},
		{"both too short", "abc", "abc", false},
		{"case insensitive", "ABCDEF1", "abcdef1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameSHA(tt.a, tt.b); got != tt.want {
				t.Errorf("sameSHA(%q,%q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestEqualFoldASCII(t *testing.T) {
	if !equalFoldASCII("Abc123", "aBc123") {
		t.Error("case-insensitive equal failed")
	}
	if equalFoldASCII("abc", "abd") {
		t.Error("unequal returned true")
	}
	if equalFoldASCII("abc", "abcd") {
		t.Error("different lengths returned true")
	}
}

func TestResolveGoInstallDir_EmptyGOBIN(t *testing.T) {
	// Simulate `go env GOBIN GOPATH` when GOBIN is unset. The real `go env`
	// emits a blank line for empty values; we shell out to /bin/sh -c to
	// mimic that output without needing the real Go toolchain.
	fake := writeFakeGoEnv(t, "\n/Users/test/go\n")
	got, err := resolveGoInstallDir(fake)
	if err != nil {
		t.Fatalf("resolveGoInstallDir: %v", err)
	}
	want := filepath.Join("/Users/test/go", "bin")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveGoInstallDir_GOBINSet(t *testing.T) {
	fake := writeFakeGoEnv(t, "/Users/test/custom/bin\n/Users/test/go\n")
	got, err := resolveGoInstallDir(fake)
	if err != nil {
		t.Fatalf("resolveGoInstallDir: %v", err)
	}
	if got != "/Users/test/custom/bin" {
		t.Errorf("GOBIN should win; got %q", got)
	}
}

// writeFakeGoEnv writes a tiny shell script that ignores its args and prints
// the given output. We use it to drive resolveGoInstallDir without depending
// on the real `go` binary's environment.
func writeFakeGoEnv(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "go")
	script := "#!/bin/sh\nprintf '%s' " + shellQuote(output) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(s string) string {
	// Wrap in single quotes; replace any embedded single quote with '\''.
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += `'\''`
		} else {
			out += string(r)
		}
	}
	return out + "'"
}

func TestIsDirWritable(t *testing.T) {
	dir := t.TempDir()
	if !isDirWritable(dir) {
		t.Errorf("temp dir %s should be writable", dir)
	}

	// Make the dir read-only and re-test. Skip on platforms where chmod
	// doesn't restrict writes the way we expect (e.g. when the test runs
	// as root, the read-only bit doesn't matter).
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits don't gate writes")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700) //nolint:errcheck // best-effort cleanup
	if isDirWritable(dir) {
		t.Errorf("read-only dir reported writable")
	}

	// Non-existent dir is not writable.
	if isDirWritable(filepath.Join(dir, "does-not-exist")) {
		t.Error("non-existent dir reported writable")
	}
}

func TestBinaryName(t *testing.T) {
	got := binaryName()
	if got != "taufinity" && got != "taufinity.exe" {
		t.Errorf("binaryName = %q, want taufinity or taufinity.exe", got)
	}
}

func TestRunRollback_NoBackup(t *testing.T) {
	// Resolving the test binary's `.prev`. Vanishingly unlikely to exist on
	// a test runner, but use a temp HOME for hygiene anyway.
	t.Setenv("HOME", t.TempDir())

	exe, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable")
	}
	// Make sure no stale .prev exists from a previous run.
	_ = os.Remove(exe + ".prev")

	err = runRollback()
	if err == nil {
		t.Fatal("expected error when no .prev exists")
	}
	if !contains(err.Error(), "no backup") {
		t.Errorf("error message = %q, expected to mention 'no backup'", err.Error())
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
