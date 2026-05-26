package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/config"
)

func TestCredentials_SaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	creds := &Credentials{
		AccessToken:  "test-token-123",
		RefreshToken: "refresh-456",
		ExpiresAt:    time.Now().Add(time.Hour),
		Email:        "user@example.com",
	}

	// Save
	if err := creds.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Load
	loaded, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials failed: %v", err)
	}

	if loaded.AccessToken != creds.AccessToken {
		t.Errorf("AccessToken = %q, want %q", loaded.AccessToken, creds.AccessToken)
	}
	if loaded.RefreshToken != creds.RefreshToken {
		t.Errorf("RefreshToken = %q, want %q", loaded.RefreshToken, creds.RefreshToken)
	}
	if loaded.Email != creds.Email {
		t.Errorf("Email = %q, want %q", loaded.Email, creds.Email)
	}
}

func TestCredentials_LoadMissing(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	_, err := LoadCredentials()
	if err == nil {
		t.Error("LoadCredentials should error when no credentials exist")
	}
}

func TestCredentials_Delete(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Create credentials
	creds := &Credentials{
		AccessToken: "to-be-deleted",
		Email:       "user@example.com",
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Delete
	if err := DeleteCredentials(); err != nil {
		t.Fatalf("DeleteCredentials failed: %v", err)
	}

	// Should not exist anymore
	_, err := LoadCredentials()
	if err == nil {
		t.Error("Credentials should be deleted")
	}
}

func TestCredentials_IsExpired(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{
			name:      "expired",
			expiresAt: time.Now().Add(-time.Hour),
			want:      true,
		},
		{
			name:      "not expired",
			expiresAt: time.Now().Add(time.Hour),
			want:      false,
		},
		{
			name:      "expires soon (within buffer)",
			expiresAt: time.Now().Add(30 * time.Second),
			want:      true, // Should be treated as expired
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds := &Credentials{ExpiresAt: tt.expiresAt}
			if got := creds.IsExpired(); got != tt.want {
				t.Errorf("IsExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldRenew_NearExpiry(t *testing.T) {
	c := &Credentials{AccessTokenExpiresAt: time.Now().Add(2 * time.Minute)}
	if !c.ShouldRenew() {
		t.Fatal("token expiring in 2m should renew (5m buffer)")
	}
	c.AccessTokenExpiresAt = time.Now().Add(30 * time.Minute)
	if c.ShouldRenew() {
		t.Fatal("token with 30m left should not renew")
	}
}

func TestHasRefreshToken(t *testing.T) {
	c := &Credentials{}
	if c.HasRefreshToken() {
		t.Fatal("empty creds have no refresh token")
	}
	c.RefreshToken = "x"
	if !c.HasRefreshToken() {
		t.Fatal("should report refresh token present")
	}
}

func TestLoadCredentials_UpgradeShimCopiesLegacyExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Simulate an old creds file: only ExpiresAt set, no AccessTokenExpiresAt.
	legacyExpiry := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	creds := &Credentials{AccessToken: "legacy", ExpiresAt: legacyExpiry}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials failed: %v", err)
	}
	if !loaded.AccessTokenExpiresAt.Equal(legacyExpiry) {
		t.Fatalf("upgrade shim should copy ExpiresAt into AccessTokenExpiresAt: got %v want %v",
			loaded.AccessTokenExpiresAt, legacyExpiry)
	}
}

func TestUpdateTokens_StoresRotatedPair(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	c := &Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := c.UpdateTokens("new-access", "new-refresh", exp, "u@example.com", "Acme"); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	if c.AccessToken != "new-access" || c.RefreshToken != "new-refresh" {
		t.Fatalf("tokens not updated: %+v", c)
	}
	if !c.AccessTokenExpiresAt.Equal(exp) || !c.ExpiresAt.Equal(exp) {
		t.Fatalf("expiry fields not coherent: access=%v legacy=%v", c.AccessTokenExpiresAt, c.ExpiresAt)
	}
	// An empty refresh token must NOT clobber the stored one.
	if err := c.UpdateTokens("newer-access", "", exp, "", ""); err != nil {
		t.Fatalf("UpdateTokens (no refresh): %v", err)
	}
	if c.RefreshToken != "new-refresh" {
		t.Fatalf("empty refresh token should not clobber existing: %q", c.RefreshToken)
	}
}

func TestCredentials_SaveIsAtomic(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	creds := &Credentials{
		AccessToken:  "atomic-access",
		RefreshToken: "atomic-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
		Email:        "user@example.com",
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// The target file must exist, be 0600, and parse back to a complete record.
	loaded, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials failed: %v", err)
	}
	if loaded.AccessToken != "atomic-access" || loaded.RefreshToken != "atomic-refresh" {
		t.Fatalf("incomplete creds after atomic save: %+v", loaded)
	}

	path := credentialsPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credentials: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("credentials perm = %o, want 0600", perm)
	}

	// No temp file should be left behind on a successful save.
	entries, err := os.ReadDir(config.Dir())
	if err != nil {
		t.Fatalf("read config dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind after Save: %s", e.Name())
		}
	}
}

func TestCredentials_SaveOverwriteRemainsParseable(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// First write, then an overwrite that rotates the refresh token. The
	// atomic rename means a reader never sees a half-written file.
	first := &Credentials{AccessToken: "a1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := first.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	second := &Credentials{AccessToken: "a2", RefreshToken: "r2", ExpiresAt: time.Now().Add(time.Hour)}
	if err := second.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	loaded, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials failed: %v", err)
	}
	if loaded.AccessToken != "a2" || loaded.RefreshToken != "r2" {
		t.Fatalf("overwrite not applied atomically: %+v", loaded)
	}

	// Confirm the on-disk file is the canonical path, not a stray temp.
	if _, err := os.Stat(filepath.Join(config.Dir(), "credentials.json")); err != nil {
		t.Fatalf("expected credentials.json at canonical path: %v", err)
	}
}

func TestCredentials_HasCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// No credentials
	if HasCredentials() {
		t.Error("HasCredentials should be false when no credentials exist")
	}

	// Save credentials
	creds := &Credentials{AccessToken: "test"}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Should have credentials now
	if !HasCredentials() {
		t.Error("HasCredentials should be true after saving")
	}
}
