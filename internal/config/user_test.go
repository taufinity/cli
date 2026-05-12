package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserConfig_SaveAndLoad(t *testing.T) {
	// Use temp directory for test
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Create config
	cfg := &UserConfig{
		Site:   "test_site_nl",
		APIURL: "http://localhost:8090",
	}

	// Save
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify file exists
	configPath := filepath.Join(tmpDir, ".config", "taufinity", "config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatalf("Config file not created at %s", configPath)
	}

	// Load
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Verify values
	if loaded.Site != cfg.Site {
		t.Errorf("Site = %q, want %q", loaded.Site, cfg.Site)
	}
	if loaded.APIURL != cfg.APIURL {
		t.Errorf("APIURL = %q, want %q", loaded.APIURL, cfg.APIURL)
	}
}

func TestUserConfig_Set(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Set a value
	if err := Set("site", "new_site_nl"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Load and verify
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Site != "new_site_nl" {
		t.Errorf("Site = %q, want %q", cfg.Site, "new_site_nl")
	}
}

func TestUserConfig_Get(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Set values
	cfg := &UserConfig{
		Site:   "voorpositiviteit_nl",
		APIURL: "https://studio.taufinity.io",
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Get each value
	tests := []struct {
		key  string
		want string
	}{
		{"site", "voorpositiviteit_nl"},
		{"api_url", "https://studio.taufinity.io"},
	}

	for _, tt := range tests {
		got, err := Get(tt.key)
		if err != nil {
			t.Errorf("Get(%q) error: %v", tt.key, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Get(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestUserConfig_GetUnknownKey(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	_, err := Get("unknown_key")
	if err == nil {
		t.Error("Get(unknown_key) should return error")
	}
}

func TestUserConfig_List(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	cfg := &UserConfig{
		Site:   "test_site",
		APIURL: "http://test.com",
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	list, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if list["site"] != "test_site" {
		t.Errorf("list[site] = %q, want %q", list["site"], "test_site")
	}
	if list["api_url"] != "http://test.com" {
		t.Errorf("list[api_url] = %q, want %q", list["api_url"], "http://test.com")
	}
}

func TestUserConfig_UpdateCheck(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Empty is allowed (default-on).
	if err := Set("update_check", ""); err != nil {
		t.Fatalf("Set empty failed: %v", err)
	}

	// false disables.
	if err := Set("update_check", "false"); err != nil {
		t.Fatalf("Set false failed: %v", err)
	}
	got, err := Get("update_check")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "false" {
		t.Errorf("Get(update_check) = %q, want false", got)
	}

	// true re-enables.
	if err := Set("update_check", "true"); err != nil {
		t.Fatalf("Set true failed: %v", err)
	}

	// Invalid value rejected.
	if err := Set("update_check", "maybe"); err == nil {
		t.Error("Set(update_check, maybe) should error")
	}

	// Unset clears.
	if err := Unset("update_check"); err != nil {
		t.Fatalf("Unset: %v", err)
	}
	got, _ = Get("update_check")
	if got != "" {
		t.Errorf("after Unset, Get = %q, want empty", got)
	}

	// List includes the key.
	list, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, ok := list["update_check"]; !ok {
		t.Error("List missing update_check key")
	}
}

func TestDir(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	dir := Dir()
	expected := filepath.Join(tmpDir, ".config", "taufinity")
	if dir != expected {
		t.Errorf("Dir() = %q, want %q", dir, expected)
	}
}
