package desktopconfig_test

import (
	"encoding/json"
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

func readToggles(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, desktopconfig.ToolTogglesFile))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON written: %v", err)
	}
	return got
}

func TestClearToolToggles_RemovesOnlyThatServer(t *testing.T) {
	dir := t.TempDir()
	cfg := writeToggles(t, dir, `{"v":3,"extra":true,"owners":{
		"acct-a":["local:voorpositiviteit:query_insights","local:voorpositiviteit:list_articles","local:other:foo"],
		"acct-b":["local:voorpositiviteit:query_insights","local:voorpositiviteit-staging:bar"]}}`)

	n, err := desktopconfig.ClearToolToggles(cfg, "voorpositiviteit")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("removed = %d, want 3", n)
	}

	got := readToggles(t, dir)
	want := map[string]any{
		"acct-a": []any{"local:other:foo"},
		"acct-b": []any{"local:voorpositiviteit-staging:bar"},
	}
	if !reflect.DeepEqual(got["owners"], want) {
		t.Fatalf("owners = %v, want %v", got["owners"], want)
	}
	if got["extra"] != true || got["v"] != float64(3) {
		t.Fatalf("top-level keys not preserved: %v", got)
	}
}

func TestClearToolToggles_NothingToDoLeavesFileAlone(t *testing.T) {
	dir := t.TempDir()
	body := `{"v":3,"owners":{"acct":["local:other:foo"]}}`
	cfg := writeToggles(t, dir, body)

	n, err := desktopconfig.ClearToolToggles(cfg, "voorpositiviteit")
	if err != nil || n != 0 {
		t.Fatalf("got (%d, %v), want (0, nil)", n, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, desktopconfig.ToolTogglesFile))
	if string(raw) != body {
		t.Fatalf("file rewritten without changes: %s", raw)
	}
}

func TestClearToolToggles_MissingFile(t *testing.T) {
	n, err := desktopconfig.ClearToolToggles(filepath.Join(t.TempDir(), "claude_desktop_config.json"), "x")
	if err != nil || n != 0 {
		t.Fatalf("got (%d, %v), want (0, nil)", n, err)
	}
}

func TestClearToolToggles_UnknownVersionUntouched(t *testing.T) {
	dir := t.TempDir()
	body := `{"v":4,"owners":{"acct":["local:voorpositiviteit:query_insights"]}}`
	cfg := writeToggles(t, dir, body)

	n, err := desktopconfig.ClearToolToggles(cfg, "voorpositiviteit")
	if err != nil || n != 0 {
		t.Fatalf("got (%d, %v), want (0, nil)", n, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, desktopconfig.ToolTogglesFile))
	if string(raw) != body {
		t.Fatalf("unknown layout was rewritten: %s", raw)
	}
}

func TestClearToolToggles_InvalidJSON(t *testing.T) {
	cfg := writeToggles(t, t.TempDir(), `{not json`)
	if _, err := desktopconfig.ClearToolToggles(cfg, "x"); err == nil {
		t.Fatal("want parse error")
	}
}
