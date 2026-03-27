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

// ValidationInterval is how often we should validate the token with the server.
const ValidationInterval = 15 * time.Minute

// Credentials holds OAuth tokens for API authentication.
type Credentials struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
	Email            string    `json:"email,omitempty"`
	OrganizationName string    `json:"organization_name,omitempty"`
	LastValidatedAt  time.Time `json:"last_validated_at,omitempty"`
}

// credentialsPath returns the path to the credentials file.
func credentialsPath() string {
	return filepath.Join(config.Dir(), "credentials.json")
}

// Save writes credentials to disk with secure permissions.
func (c *Credentials) Save() error {
	dir := config.Dir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	// Write with restrictive permissions (owner read/write only)
	if err := os.WriteFile(credentialsPath(), data, 0600); err != nil {
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
func (c *Credentials) IsExpired() bool {
	return time.Now().Add(ExpiryBuffer).After(c.ExpiresAt)
}

// GetValidToken returns the access token if valid, or an error if expired.
func (c *Credentials) GetValidToken() (string, error) {
	if c.IsExpired() {
		return "", fmt.Errorf("token expired at %s", c.ExpiresAt.Format(time.RFC3339))
	}
	return c.AccessToken, nil
}

// NeedsValidation returns whether the token should be validated with the server.
// Returns true if never validated or last validation was more than ValidationInterval ago.
func (c *Credentials) NeedsValidation() bool {
	if c.LastValidatedAt.IsZero() {
		return true
	}
	return time.Since(c.LastValidatedAt) > ValidationInterval
}

// UpdateValidatedAt updates the last validated timestamp and saves to disk.
func (c *Credentials) UpdateValidatedAt() error {
	c.LastValidatedAt = time.Now()
	return c.Save()
}

// Update updates credentials with new values and saves to disk.
func (c *Credentials) Update(accessToken string, expiresAt time.Time, email, orgName string) error {
	c.AccessToken = accessToken
	c.ExpiresAt = expiresAt
	c.Email = email
	c.OrganizationName = orgName
	c.LastValidatedAt = time.Now()
	return c.Save()
}
