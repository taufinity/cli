package desktopconfig_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/taufinity/cli/internal/desktopconfig"
)

func TestUpsertServer_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	err := desktopconfig.UpsertServer(path, "taufinity-studio", desktopconfig.RemoteServer{
		Type: "http",
		URL:  "https://studio.taufinity.io/mcp",
		Headers: map[string]string{
			"Authorization": "Bearer abc123",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON written: %v", err)
	}
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers missing or wrong type")
	}
	tau, ok := servers["taufinity-studio"].(map[string]any)
	if !ok {
		t.Fatal("taufinity-studio entry missing")
	}
	if tau["url"] != "https://studio.taufinity.io/mcp" {
		t.Errorf("url = %v", tau["url"])
	}
	if tau["type"] != "http" {
		t.Errorf("type = %v", tau["type"])
	}
}

func TestUpsertServer_PreservesOtherEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	initial := `{
  "mcpServers": {
    "filesystem": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]},
    "github": {"url": "https://api.githubcopilot.com/mcp/", "headers": {"Authorization": "Bearer x"}}
  },
  "experimentalFeatures": {"keepAlive": true}
}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	err := desktopconfig.UpsertServer(path, "taufinity-studio", desktopconfig.RemoteServer{
		Type:    "http",
		URL:     "https://studio.taufinity.io/mcp",
		Headers: map[string]string{"Authorization": "Bearer abc"},
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["filesystem"]; !ok {
		t.Error("filesystem entry was clobbered")
	}
	if _, ok := servers["github"]; !ok {
		t.Error("github entry was clobbered")
	}
	if got["experimentalFeatures"] == nil {
		t.Error("experimentalFeatures was clobbered")
	}
	if _, ok := servers["taufinity-studio"]; !ok {
		t.Error("taufinity-studio not added")
	}
}

func TestUpsertServer_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	if err := desktopconfig.UpsertServer(path, "taufinity-studio", desktopconfig.RemoteServer{
		URL: "https://old.example/mcp",
	}); err != nil {
		t.Fatal(err)
	}
	if err := desktopconfig.UpsertServer(path, "taufinity-studio", desktopconfig.RemoteServer{
		URL: "https://new.example/mcp",
	}); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	tau := got["mcpServers"].(map[string]any)["taufinity-studio"].(map[string]any)
	if tau["url"] != "https://new.example/mcp" {
		t.Errorf("expected overwrite to new URL, got %v", tau["url"])
	}
}

func TestUpsertServer_AtomicWriteUnderRepeatedCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	for i := 0; i < 50; i++ {
		if err := desktopconfig.UpsertServer(path, "taufinity-studio", desktopconfig.RemoteServer{
			Type: "http",
			URL:  "https://studio.taufinity.io/mcp",
		}); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		raw, _ := os.ReadFile(path)
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("iteration %d: invalid JSON: %v", i, err)
		}
	}
}

func TestRemoveServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"taufinity-studio":{"url":"x"},"other":{"url":"y"}}}`), 0o600)

	if err := desktopconfig.RemoveServer(path, "taufinity-studio"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["taufinity-studio"]; ok {
		t.Error("taufinity-studio not removed")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("other was clobbered")
	}
}

func TestRemoveServer_MissingFile_NoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.json")
	if err := desktopconfig.RemoveServer(path, "taufinity-studio"); err != nil {
		t.Errorf("expected no error on missing file, got %v", err)
	}
}

func TestHasServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"taufinity-studio":{"url":"x"}}}`), 0o600)

	got, err := desktopconfig.HasServer(path, "taufinity-studio")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected HasServer to return true")
	}

	got, _ = desktopconfig.HasServer(path, "absent")
	if got {
		t.Error("expected HasServer to return false for absent entry")
	}
}

func TestDefaultClaudeDesktopPath_PerOS(t *testing.T) {
	got, err := desktopconfig.DefaultClaudeDesktopPath()
	// On Linux this returns ErrUnsupportedOS; on darwin/windows it returns a non-empty path.
	if err == nil && got == "" {
		t.Error("expected non-empty path or an error, got both empty/nil")
	}
}
