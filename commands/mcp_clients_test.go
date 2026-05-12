package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClientRegistry_AllClientsResolvable(t *testing.T) {
	// Use a clean HOME so OS-default paths resolve to a stable temp location.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	for _, c := range mcpClientsList {
		t.Run(c.name, func(t *testing.T) {
			path, err := c.resolvePath()
			if err != nil {
				t.Fatalf("%s: resolvePath: %v", c.name, err)
			}
			if path == "" {
				t.Errorf("%s: empty path", c.name)
			}
			if !filepath.IsAbs(path) {
				t.Errorf("%s: path %q is not absolute", c.name, path)
			}
		})
	}
}

func TestVSCodePath_UsesServersKeyNotMCPServers(t *testing.T) {
	c, ok := lookupClient("vscode")
	if !ok {
		t.Fatal("vscode not in registry")
	}
	if c.serversKey != "servers" {
		t.Errorf("vscode serversKey = %q, want %q (VS Code's MCP format diverges)", c.serversKey, "servers")
	}
}

func TestAllOtherClients_UseMCPServersKey(t *testing.T) {
	for _, c := range mcpClientsList {
		if c.name == "vscode" {
			continue
		}
		if c.serversKey != "mcpServers" {
			t.Errorf("%s serversKey = %q, want %q", c.name, c.serversKey, "mcpServers")
		}
	}
}

func TestVSCodePath_PerOS(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", filepath.Join(tmp, "AppData"))

	path, err := vscodePath()
	if err != nil {
		t.Fatalf("vscodePath: %v", err)
	}

	want := map[string]string{
		"darwin":  filepath.Join(tmp, "Library", "Application Support", "Code", "User", "mcp.json"),
		"linux":   filepath.Join(tmp, ".config", "Code", "User", "mcp.json"),
		"windows": filepath.Join(tmp, "AppData", "Code", "User", "mcp.json"),
	}[runtime.GOOS]

	if want == "" {
		t.Skipf("no expectation for OS %q", runtime.GOOS)
	}
	if path != want {
		t.Errorf("vscodePath = %q, want %q", path, want)
	}
}

func TestEnvOverrides_TakePrecedenceOverDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		envVar string
		resolv func() (string, error)
	}{
		{"TAUFINITY_DESKTOP_CONFIG", claudeDesktopPath},
		{"TAUFINITY_CLAUDE_CODE_CONFIG", claudeCodePath},
		{"TAUFINITY_CURSOR_CONFIG", cursorPath},
		{"TAUFINITY_VSCODE_CONFIG", vscodePath},
		{"TAUFINITY_ANTIGRAVITY_CONFIG", antigravityPath},
	}
	for _, tt := range tests {
		t.Run(tt.envVar, func(t *testing.T) {
			want := "/tmp/override-" + tt.envVar + ".json"
			t.Setenv(tt.envVar, want)
			got, err := tt.resolv()
			if err != nil {
				t.Fatalf("%v", err)
			}
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestDetectInstalled_RequiresParentDirToExist(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", filepath.Join(tmp, "Claude", "claude_desktop_config.json"))
	c, _ := lookupClient("claude-desktop")

	// Parent dir doesn't exist → not detected.
	if detectInstalled(c) {
		t.Error("expected not detected when parent dir absent")
	}

	// Create the parent dir → detected.
	if err := os.MkdirAll(filepath.Join(tmp, "Claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !detectInstalled(c) {
		t.Error("expected detected when parent dir present")
	}
}

func TestMCPInstall_All_OnlyInstallsToDetectedClients(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "all-clients-token")
	t.Setenv("TAUFINITY_BINARY_PATH", "/opt/taufinity/bin/taufinity")

	// Detected: cursor (parent dir created). Not detected: everything else.
	cursorPathDir := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(cursorPathDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Point env-var overrides at the temp HOME so no resolver hits the real
	// user's config files. (claude-desktop is the one with a non-HOME default
	// on macOS, hence the explicit override.)
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", filepath.Join(home, "claude-desktop-not-detected.json"))

	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install",
		"--client", "all",
		"--label", "taufinity-all-test",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	cursorCfg := filepath.Join(cursorPathDir, "mcp.json")
	raw, err := os.ReadFile(cursorCfg)
	if err != nil {
		t.Fatalf("cursor config not written: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("cursor config not valid JSON: %v", err)
	}
	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["taufinity-all-test"]; !ok {
		t.Errorf("cursor: entry missing; got %v", servers)
	}

	// Summary on stderr mentions cursor as installed and the others as skipped.
	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "Installed:") || !strings.Contains(stderrStr, "cursor") {
		t.Errorf("stderr summary missing cursor install line:\n%s", stderrStr)
	}
	if !strings.Contains(stderrStr, "Skipped:") {
		t.Errorf("stderr summary missing skipped line:\n%s", stderrStr)
	}
}

func TestMCPInstall_All_ErrorsWhenNothingDetected(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "no-clients-token")
	t.Setenv("TAUFINITY_BINARY_PATH", "/opt/taufinity/bin/taufinity")
	// Point Claude Desktop override at a path under a non-existent parent
	// dir so detectInstalled returns false on macOS.
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", filepath.Join(home, "no-such-dir", "claude_desktop_config.json"))

	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install", "--client", "all", "--label", "taufinity-none",
	})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when no clients detected")
	}
	if !strings.Contains(err.Error(), "no clients detected") {
		t.Errorf("error = %v, want 'no clients detected'", err)
	}
}

func TestMCPInstall_VSCode_WritesUnderServersKey(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	seedCredentials(t, "vscode-token")
	t.Setenv("TAUFINITY_BINARY_PATH", "/opt/taufinity/bin/taufinity")
	vscodeCfg := filepath.Join(home, "vscode-mcp.json")
	t.Setenv("TAUFINITY_VSCODE_CONFIG", vscodeCfg)

	rootCmd.SetArgs([]string{
		"--api-url", "https://studio.taufinity.io",
		"mcp", "install", "--client", "vscode", "--label", "taufinity-vscode",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	raw, err := os.ReadFile(vscodeCfg)
	if err != nil {
		t.Fatalf("read vscode config: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, hasOld := got["mcpServers"]; hasOld {
		t.Error("vscode config wrote under mcpServers; should use servers")
	}
	servers, ok := got["servers"].(map[string]any)
	if !ok {
		t.Fatalf("vscode config missing 'servers' key; got %v", got)
	}
	if _, ok := servers["taufinity-vscode"]; !ok {
		t.Errorf("entry missing under servers: %v", servers)
	}
}

func TestMCPInstall_UnknownClient_ListsValidOptions(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "unknown-client-token")

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })

	rootCmd.SetArgs([]string{
		"mcp", "install", "--client", "bogus",
	})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown client")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown") || !strings.Contains(msg, "claude-desktop") {
		t.Errorf("error message should list valid clients; got: %s", msg)
	}
}

func TestMCPUninstall_All_RemovesFromEveryConfigWithExistingEntry(t *testing.T) {
	resetGlobals(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedCredentials(t, "uninstall-all-token")
	t.Setenv("TAUFINITY_BINARY_PATH", "/opt/taufinity/bin/taufinity")

	// Pre-create configs for two clients only.
	cursorCfg := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursorCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorCfg, []byte(`{"mcpServers":{"taufinity-uninst":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	codeCfg := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(codeCfg, []byte(`{"mcpServers":{"taufinity-uninst":{"command":"x"},"other":{"command":"y"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TAUFINITY_DESKTOP_CONFIG", filepath.Join(home, "missing", "claude_desktop_config.json"))

	rootCmd.SetArgs([]string{
		"mcp", "uninstall",
		"--client", "all",
		"--label", "taufinity-uninst",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// cursor: entry gone.
	raw, _ := os.ReadFile(cursorCfg)
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		t.Fatalf("cursor JSON: %v", err)
	}
	if servers, _ := cur["mcpServers"].(map[string]any); servers != nil {
		if _, ok := servers["taufinity-uninst"]; ok {
			t.Errorf("cursor: entry still present")
		}
	}

	// claude-code: target entry gone, sibling preserved.
	raw, _ = os.ReadFile(codeCfg)
	var code map[string]any
	if err := json.Unmarshal(raw, &code); err != nil {
		t.Fatalf("claude-code JSON: %v", err)
	}
	servers := code["mcpServers"].(map[string]any)
	if _, ok := servers["taufinity-uninst"]; ok {
		t.Error("claude-code: target entry still present")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("claude-code: sibling entry was wrongly removed")
	}
}
