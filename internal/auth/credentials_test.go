package auth

import (
	"os"
	"testing"
	"time"
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
