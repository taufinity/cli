package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/taufinity/cli/internal/config"
)

// ExpiryBuffer is subtracted from expiry time to refresh tokens early.
const ExpiryBuffer = time.Minute

// RenewBuffer is how long before access-token expiry we proactively renew it
// from the refresh token. Sized for the short-lived (1h) CLI access token.
const RenewBuffer = 5 * time.Minute

// Credentials holds OAuth tokens for API authentication.
type Credentials struct {
	AccessToken string `json:"access_token"`
	// RefreshToken is a long-lived, rotating credential used to mint new access
	// tokens. Stored in plaintext on disk (0600); the server keeps only its hash.
	RefreshToken string `json:"refresh_token,omitempty"`
	// AccessTokenExpiresAt is the authoritative expiry for the short-lived access
	// token. ExpiresAt is kept for backward-compat with old credential files.
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at,omitempty"`
	ExpiresAt            time.Time `json:"expires_at"`
	Email                string    `json:"email,omitempty"`
	OrganizationName     string    `json:"organization_name,omitempty"`
}

// credentialsPath returns the path to the credentials file.
func credentialsPath() string {
	return filepath.Join(config.Dir(), "credentials.json")
}

// Save writes credentials to disk with secure permissions.
//
// The write is atomic: data is written to a temp file in the same directory
// with 0600 perms, then renamed over the target (atomic on POSIX). This avoids
// the truncate-then-write race where a concurrent taufinity process could read
// a partial file or where two rotating writers could corrupt the stored
// refresh token. The temp file is removed if anything before the rename fails.
func (c *Credentials) Save() error {
	dir := config.Dir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	// Temp file in the SAME directory so os.Rename is a same-filesystem atomic
	// move (cross-device renames fail). Pattern keeps the 0600-only intent.
	tmp, err := os.CreateTemp(dir, "credentials-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp credentials: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we don't make it to a successful rename.
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp credentials: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp credentials: %w", err)
	}

	// Atomic replace of the target.
	if err := os.Rename(tmpName, credentialsPath()); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}

	return nil
}

// LoadCredentials reads credentials from disk.
func LoadCredentials() (*Credentials, error) {
	data, err := os.ReadFile(credentialsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("not logged in (run 'taufinity auth login')")
		}
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}

	// Upgrade shim: old credential files only have ExpiresAt. Treat it as the
	// access-token expiry so ShouldRenew works without a re-login data wipe.
	if creds.AccessTokenExpiresAt.IsZero() && !creds.ExpiresAt.IsZero() {
		creds.AccessTokenExpiresAt = creds.ExpiresAt
	}

	return &creds, nil
}

// DeleteCredentials removes the credentials file.
func DeleteCredentials() error {
	path := credentialsPath()
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil // Already deleted
		}
		return fmt.Errorf("delete credentials: %w", err)
	}
	return nil
}

// HasCredentials returns whether credentials exist on disk.
func HasCredentials() bool {
	_, err := os.Stat(credentialsPath())
	return err == nil
}

// IsExpired returns whether the access token is expired or about to expire.
// Used by the status/login UI flows; the token hot path uses ShouldRenew +
// the client's renewing Token() instead.
func (c *Credentials) IsExpired() bool {
	return time.Now().Add(ExpiryBuffer).After(c.ExpiresAt)
}

// ShouldRenew reports whether the access token is expired or within RenewBuffer
// of expiry, in which case it should be renewed from the refresh token.
func (c *Credentials) ShouldRenew() bool {
	return time.Now().Add(RenewBuffer).After(c.AccessTokenExpiresAt)
}

// HasRefreshToken reports whether a refresh token is stored.
func (c *Credentials) HasRefreshToken() bool {
	return c.RefreshToken != ""
}

// UpdateTokens stores a rotated access+refresh pair and saves to disk. An empty
// refreshToken leaves the existing one intact (the server may omit it). Empty
// email/orgName likewise preserve current values.
func (c *Credentials) UpdateTokens(accessToken, refreshToken string, accessExpiresAt time.Time, email, orgName string) error {
	c.AccessToken = accessToken
	if refreshToken != "" {
		c.RefreshToken = refreshToken
	}
	c.AccessTokenExpiresAt = accessExpiresAt
	c.ExpiresAt = accessExpiresAt // keep legacy field coherent
	if email != "" {
		c.Email = email
	}
	if orgName != "" {
		c.OrganizationName = orgName
	}
	return c.Save()
}
