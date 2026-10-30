package auth

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	// path is the file these credentials were loaded from (or are bound to), so
	// Save writes a renewed pair back to the same host's file.
	path string
}

// DefaultHost is production Studio. Its credentials keep the original file
// name, credentials.json, so existing logins survive the upgrade.
const DefaultHost = "studio.taufinity.io"

// Credentials are kept per Studio host. They used to be one file for every
// host, which meant a staging login replaced the production session, and a
// staging process (e.g. a staging MCP bridge) "renewing" the production
// refresh token against staging got a 401 and deleted the production file.
var (
	hostMu        sync.RWMutex
	currentAPIURL string // "" = production
)

// UseAPIURL sets the Studio API URL whose credentials the package-level
// helpers (LoadCredentials, HasCredentials, DeleteCredentials, Save on fresh
// Credentials) read and write. The CLI calls it once, after resolving
// --api-url / --env / TAUFINITY_API_URL.
func UseAPIURL(apiURL string) {
	hostMu.Lock()
	currentAPIURL = apiURL
	hostMu.Unlock()
}

func currentURL() string {
	hostMu.RLock()
	defer hostMu.RUnlock()
	return currentAPIURL
}

// CredentialsPathFor returns the credentials file for a Studio API URL:
// credentials.json for production (or an unparseable/empty URL), and
// credentials-<host>.json for any other host, e.g.
// credentials-staging-studio.taufinity.io.json or credentials-localhost_8080.json.
func CredentialsPathFor(apiURL string) string {
	return filepath.Join(config.Dir(), credentialsFileName(apiURL))
}

func credentialsFileName(apiURL string) string {
	host := ""
	if u, err := url.Parse(strings.TrimSpace(apiURL)); err == nil {
		host = strings.ToLower(u.Host)
	}
	if host == "" || host == DefaultHost || host == DefaultHost+":443" {
		return "credentials.json"
	}
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return "credentials-" + b.String() + ".json"
}

// credentialsPath returns the credentials file for the current API URL.
func credentialsPath() string {
	return CredentialsPathFor(currentURL())
}

// Path is the file these credentials are bound to.
func (c *Credentials) Path() string {
	if c.path == "" {
		return credentialsPath()
	}
	return c.path
}

// BindTo binds fresh credentials (e.g. from a new login) to apiURL's file.
func (c *Credentials) BindTo(apiURL string) {
	c.path = CredentialsPathFor(apiURL)
}

// Save writes credentials to disk with secure permissions.
//
// The write is atomic: data is written to a temp file in the same directory
// with 0600 perms, then renamed over the target (atomic on POSIX). This avoids
// the truncate-then-write race where a concurrent taufinity process could read
// a partial file or where two rotating writers could corrupt the stored
// refresh token. The temp file is removed if anything before the rename fails.
func (c *Credentials) Save() error {
	target := c.Path()
	dir := filepath.Dir(target)
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
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp credentials: %w", err)
	}

	// Atomic replace of the target.
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	c.path = target

	return nil
}

// LoadCredentials reads the credentials for the current API URL.
func LoadCredentials() (*Credentials, error) {
	return loadFrom(credentialsPath())
}

// LoadCredentialsFor reads the credentials for apiURL.
func LoadCredentialsFor(apiURL string) (*Credentials, error) {
	return loadFrom(CredentialsPathFor(apiURL))
}

func loadFrom(path string) (*Credentials, error) {
	data, err := os.ReadFile(path)
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
	creds.path = path

	// Upgrade shim: old credential files only have ExpiresAt. Treat it as the
	// access-token expiry so ShouldRenew works without a re-login data wipe.
	if creds.AccessTokenExpiresAt.IsZero() && !creds.ExpiresAt.IsZero() {
		creds.AccessTokenExpiresAt = creds.ExpiresAt
	}

	return &creds, nil
}

// DeleteCredentials removes the credentials file for the current API URL.
func DeleteCredentials() error {
	return deleteAt(credentialsPath())
}

// DeleteFile removes the file these credentials are bound to.
func (c *Credentials) DeleteFile() error {
	return deleteAt(c.Path())
}

func deleteAt(path string) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil // Already deleted
		}
		return fmt.Errorf("delete credentials: %w", err)
	}
	return nil
}

// HasCredentials returns whether credentials exist on disk for the current API URL.
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
