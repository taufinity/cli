package desktopconfig_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/taufinity/cli/internal/desktopconfig"
)

func writeToggles(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, desktopconfig.ToolTogglesFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "claude_desktop_config.json")
}

func TestSwitchedOffTools_OnlyThatServerDeduplicated(t *testing.T) {
	cfg := writeToggles(t, t.TempDir(), `{"v":3,"owners":{
		"a":["local:studio:y","local:studio:x","local:studio-2:z","local:other:foo"],
		"b":["local:studio:x"]}}`)
	got, err := desktopconfig.SwitchedOffTools(cfg, "studio")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"x", "y"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSwitchedOffTools_NothingToReport(t *testing.T) {
	cases := map[string]string{
		"empty file":      ``,
		"unknown version": `{"v":4,"owners":{"a":["local:studio:x"]}}`,
		"no entries":      `{"v":3,"owners":{}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := desktopconfig.SwitchedOffTools(writeToggles(t, t.TempDir(), body), "studio")
			if err != nil || len(got) != 0 {
				t.Fatalf("got (%v, %v), want none", got, err)
			}
		})
	}
	got, err := desktopconfig.SwitchedOffTools(filepath.Join(t.TempDir(), "claude_desktop_config.json"), "studio")
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file: got (%v, %v), want none", got, err)
	}
}

func TestSwitchedOffTools_InvalidJSON(t *testing.T) {
	if _, err := desktopconfig.SwitchedOffTools(writeToggles(t, t.TempDir(), `{not json`), "x"); err == nil {
		t.Fatal("want parse error")
	}
}

func TestBridgeServers_MatchesOnCommandNotLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude_desktop_config.json")
	cfg := `{"mcpServers":{
		"voorpositiviteit":{"command":"/Users/x/bin/taufinity","args":["--org","3","mcp","stdio"]},
		"brew":{"command":"/opt/homebrew/bin/taufinity","args":["mcp","stdio"]},
		"legacy-http":{"type":"http","url":"https://studio.taufinity.io/mcp"},
		"not-a-bridge":{"command":"/usr/local/bin/taufinity","args":["status"]},
		"other":{"command":"npx","args":["stdio"]}}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := desktopconfig.BridgeServers(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"brew", "voorpositiviteit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
