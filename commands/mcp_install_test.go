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
	flagMCPInstallTransport = ""
	flagMCPUninstallClient = "claude-desktop"
	flagMCPUninstallLabel = "taufinity-studio"
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

func TestMCPInstall_WritesStdioEntryByDefaultForClaudeDesktop(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "writes-test-token")
	t.Setenv("TAUFINITY_BINARY_PATH", "/opt/taufinity/bin/taufinity")

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"--org", "3",
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
	tau := got["mcpServers"].(map[string]any)["taufinity-test"].(map[string]any)
	if _, wrong := tau["headers"]; wrong {
		t.Errorf("stdio entry must not include 'headers' field (bearer must not be embedded in Claude Desktop config); got %v", tau)
	}
	if _, wrong := tau["type"]; wrong {
		t.Errorf("stdio entry must not include 'type' field; got %v", tau)
	}
	if tau["command"] != "/opt/taufinity/bin/taufinity" {
		t.Errorf("command = %v, want /opt/taufinity/bin/taufinity", tau["command"])
	}
	args, _ := tau["args"].([]any)
	want := []string{"--org", "3", "mcp", "stdio"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %v, want %q", i, args[i], w)
		}
	}
}

func TestMCPInstall_HTTPTransportOverrideEmbedsBearer(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "writes-test-token-http")

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install", "--label", "taufinity-test", "--transport", "http",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	raw, _ := os.ReadFile(cfgPath)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	tau := got["mcpServers"].(map[string]any)["taufinity-test"].(map[string]any)
	if tau["type"] != "http" {
		t.Errorf("--transport http should produce type=http, got %v", tau["type"])
	}
	headers := tau["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer writes-test-token-http" {
		t.Errorf("Authorization = %v", headers["Authorization"])
	}
}

func TestMCPInstall_StdioOmitsOrgWhenUnset(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok")
	t.Setenv("TAUFINITY_BINARY_PATH", "/usr/bin/taufinity")
	t.Setenv("TAUFINITY_ORG", "")

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	raw, _ := os.ReadFile(cfgPath)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	tau := got["mcpServers"].(map[string]any)["taufinity-studio"].(map[string]any)
	args, _ := tau["args"].([]any)
	want := []string{"mcp", "stdio"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v (no --org when unset)", args, want)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %v, want %q", i, args[i], w)
		}
	}
}

func TestMCPInstall_StdioEmbedsLocalhostAPIURL(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok")
	t.Setenv("TAUFINITY_BINARY_PATH", "/usr/bin/taufinity")

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "http://localhost:8090",
		"--org", "12",
		"mcp", "install", "--label", "taufinity-local",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	raw, _ := os.ReadFile(cfgPath)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	tau := got["mcpServers"].(map[string]any)["taufinity-local"].(map[string]any)
	args, _ := tau["args"].([]any)
	want := []string{"--org", "12", "--api-url", "http://localhost:8090", "mcp", "stdio"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %v, want %q", i, args[i], w)
		}
	}
}

func TestMCPInstall_ForceReplacesLegacyHTTPEntryWithStdio(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok-upgrade")
	t.Setenv("TAUFINITY_BINARY_PATH", "/usr/local/bin/taufinity")

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	legacy := `{"mcpServers":{"taufinity-studio":{"type":"http","url":"https://studio.taufinity.io/mcp","headers":{"Authorization":"Bearer LEGACY"}},"keep-me":{"command":"x"}}}`
	if err := os.WriteFile(cfgPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"--org", "3",
		"mcp", "install", "--force",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	raw, _ := os.ReadFile(cfgPath)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	servers := got["mcpServers"].(map[string]any)
	tau := servers["taufinity-studio"].(map[string]any)
	if _, leftover := tau["headers"]; leftover {
		t.Errorf("legacy 'headers' field was not removed during upgrade; got %v", tau)
	}
	if _, leftover := tau["type"]; leftover {
		t.Errorf("legacy 'type' field was not removed during upgrade; got %v", tau)
	}
	if tau["command"] != "/usr/local/bin/taufinity" {
		t.Errorf("stdio command missing or wrong: %v", tau["command"])
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Error("unrelated 'keep-me' entry was clobbered during upgrade")
	}
}

func TestMCPInstall_RefusalHintsLegacyHTTPShape(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok")
	t.Setenv("TAUFINITY_BINARY_PATH", "/usr/bin/taufinity")

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	legacy := `{"mcpServers":{"taufinity-studio":{"type":"http","url":"x","headers":{"Authorization":"Bearer L"}}}}`
	if err := os.WriteFile(cfgPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install",
	})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected refusal without --force")
	}
	msg := err.Error()
	if !strings.Contains(msg, "legacy HTTP transport") {
		t.Errorf("expected upgrade hint mentioning 'legacy HTTP transport', got: %v", err)
	}
	if !strings.Contains(msg, "--force") {
		t.Errorf("expected --force suggestion in error: %v", err)
	}
}

func TestMCPInstall_RejectsBogusOrgInStdio(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok")
	t.Setenv("TAUFINITY_BINARY_PATH", "/usr/bin/taufinity")
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", filepath.Join(t.TempDir(), "x.json"))

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"--org", "3; rm -rf ~",
		"mcp", "install",
	})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for bogus --org embedded into stdio args")
	}
	if !strings.Contains(err.Error(), "invalid org") {
		t.Errorf("error should mention invalid org, got: %v", err)
	}
}

func TestMCPInstall_TransportAutoMatchesDefault(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok")
	t.Setenv("TAUFINITY_BINARY_PATH", "/usr/bin/taufinity")

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "claude_desktop_config.json")
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install", "--transport", "auto",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	raw, _ := os.ReadFile(cfgPath)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	tau := got["mcpServers"].(map[string]any)["taufinity-studio"].(map[string]any)
	if _, ok := tau["command"]; !ok {
		t.Errorf("--transport=auto on claude-desktop should produce stdio shape (with 'command'), got %v", tau)
	}
}

func TestMCPInstall_InvalidTransport_IsRejected(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok")

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install", "--transport", "ws",
	})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid --transport")
	}
	if !strings.Contains(err.Error(), "stdio") || !strings.Contains(err.Error(), "http") {
		t.Errorf("error should explain valid values, got: %v", err)
	}
}

func TestMCPInstall_PrintRejectsExplicitStdioTransport(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok")

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install", "--client", "print", "--transport", "stdio",
	})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error: --client print with --transport stdio is incompatible")
	}
	if !strings.Contains(err.Error(), "print") || !strings.Contains(err.Error(), "stdio") {
		t.Errorf("error should reference the incompatible combo, got: %v", err)
	}
}

// TestMCPPrint_DoesNotLeakClientFlagIntoSubsequentInstall verifies that
// 'mcp print' restores the package-level flagMCPInstallClient when it
// finishes, so a subsequent 'mcp install' invocation in the same process
// (or test binary) doesn't silently emit HTTP because the print flow
// flipped the flag.
func TestMCPPrint_DoesNotLeakClientFlagIntoSubsequentInstall(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "tok-print-leak")

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stdout)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })

	rootCmd.SetArgs([]string{"--api-url", "https://studio.taufinity.io", "mcp", "print"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp print: %v", err)
	}
	if flagMCPInstallClient != "claude-desktop" {
		t.Fatalf("flagMCPInstallClient leaked: got %q, want claude-desktop (the default)", flagMCPInstallClient)
	}
}

func TestIsTempPath_Boundary(t *testing.T) {
	cases := map[string]bool{
		"/private/var/folders/abc/exe/taufinity": true,
		"/tmp/taufinity":                         true,
		"/usr/local/bin/taufinity":               false,
		"/opt/homebrew/bin/taufinity":            false,
		os.TempDir() + "/taufinity":              true,
	}
	for path, wantTemp := range cases {
		if got := isTempPath(path); got != wantTemp {
			t.Errorf("isTempPath(%q) = %v, want %v", path, got, wantTemp)
		}
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
	t.Setenv("TAUFINITY_BINARY_PATH", "/usr/bin/taufinity")

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
