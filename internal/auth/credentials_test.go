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
