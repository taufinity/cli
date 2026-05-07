package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/auth"
)

func TestMCPInstallCommand_RegisteredAlongsideLogin(t *testing.T) {
	for _, want := range []string{"install", "login", "uninstall", "print"} {
		cmd, _, err := rootCmd.Find([]string{"mcp", want})
		if err != nil {
			t.Fatalf("mcp %s not registered: %v", want, err)
		}
		if cmd.Name() != want {
			t.Fatalf("mcp %s: got command %q, want %q", want, cmd.Name(), want)
		}
	}
}

// seedCredentials writes a non-expired credentials.json under HOME and returns
// the bearer token. Caller must already have set HOME via t.Setenv.
func seedCredentials(t *testing.T, token string) {
	t.Helper()
	creds := &auth.Credentials{
		AccessToken: token,
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		Email:       "test@example.com",
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
}

// resetGlobals clears the package-level flag globals that persist across
// tests in the same binary. Call at the start of each rootCmd.Execute test.
func resetGlobals(t *testing.T) {
	t.Helper()
	flagAPIURL = ""
	flagSite = ""
	flagOrg = ""
	flagFormat = "table"
	flagQuiet = false
	flagDryRun = false
	flagDebug = false
	flagMCPInstallClient = "claude-desktop"
	flagMCPInstallLabel = "taufinity-studio"
	flagMCPInstallForce = false
}

func TestMCPInstall_PrintsJSONBlockWithBearer(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "test-bearer-xyz")

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stdout)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "print",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput: %s", err, stdout.String())
	}
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers missing")
	}
	tau, ok := servers["taufinity-studio"].(map[string]any)
	if !ok {
		t.Fatalf("taufinity-studio entry missing; got %v", servers)
	}
	if tau["url"] != "https://studio.taufinity.io/mcp" {
		t.Errorf("url = %v", tau["url"])
	}
	if tau["type"] != "http" {
		t.Errorf("type = %v", tau["type"])
	}
	headers, _ := tau["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer test-bearer-xyz" {
		t.Errorf("Authorization = %v", headers["Authorization"])
	}
}

func TestMCPInstall_WritesConfigFile(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "writes-test-token")

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install", "--label", "taufinity-test",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("written config not valid JSON: %v", err)
	}
	servers := got["mcpServers"].(map[string]any)
	tau, ok := servers["taufinity-test"].(map[string]any)
	if !ok {
		t.Fatalf("taufinity-test entry missing")
	}
	headers := tau["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer writes-test-token" {
		t.Errorf("Authorization = %v", headers["Authorization"])
	}
}

func TestMCPInstall_RefusesWhenNotLoggedIn(t *testing.T) {
	resetGlobals(t)
	t.Setenv("HOME", t.TempDir()) // no credentials.json

	cfgDir := t.TempDir()
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", filepath.Join(cfgDir, "x.json"))

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install",
	})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
	if !strings.Contains(err.Error(), "auth login") {
		t.Errorf("error should mention auth login, got: %v", err)
	}
}

func TestMCPUninstall_RemovesNamedEntryPreservesOthers(t *testing.T) {
	resetGlobals(t)
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	initial := `{"mcpServers":{"taufinity-studio":{"url":"x"},"keep-me":{"url":"y"}},"otherTopLevel":true}`
	if err := os.WriteFile(cfgPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "uninstall",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	raw, _ := os.ReadFile(cfgPath)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["taufinity-studio"]; ok {
		t.Error("taufinity-studio still present after uninstall")
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Error("keep-me was clobbered")
	}
	if got["otherTopLevel"] != true {
		t.Error("otherTopLevel was clobbered")
	}
}

func TestMCPUninstall_MissingFileNoError(t *testing.T) {
	resetGlobals(t)
	cfgDir := t.TempDir()
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", filepath.Join(cfgDir, "absent.json"))

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "uninstall",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("uninstall on missing file should be a no-op, got: %v", err)
	}
}

func TestMCPInstall_RefusesOverwriteWithoutForce(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok")

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"mcpServers":{"taufinity-studio":{"url":"existing"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install",
	})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when entry exists without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should mention --force, got: %v", err)
	}
}
