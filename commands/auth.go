package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/browser"
	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/auth"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage authentication",
	Long:  `Authenticate with the Taufinity API to enable template preview and other commands.`,
}

var authLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with Taufinity",
	Long: `Authenticate with your Taufinity account using device authorization.

This opens a browser where you can approve the CLI access.
The CLI will poll for approval and store the token locally.`,
	RunE: runAuthLogin,
}

var authRevokeAll bool

var authRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Revoke authentication",
	Long: `Remove stored credentials and log out of the CLI.

By default this revokes only the current session. Use --all to revoke every CLI
session for your account ("log out everywhere").`,
	RunE: runAuthRevoke,
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show authentication status",
	Long:  `Display current authentication status and user info.`,
	RunE:  runAuthStatus,
	Annotations: map[string]string{
		// Commonly scripted (CI health checks, login flows). Skip the
		// staleness warning so it doesn't end up in stderr-parsing pipelines.
		"suppress-update-warning": "true",
	},
}

var authTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Print the current access token",
	Long: `Print the current access token for use in scripts or API calls.

Output is just the token with no extra formatting, suitable for:
  curl -H "Authorization: Bearer $(taufinity auth token)" ...`,
	RunE: runAuthToken,
}

func init() {
	rootCmd.AddCommand(authCmd)
	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authRevokeCmd)
	authCmd.AddCommand(authStatusCmd)
	authCmd.AddCommand(authTokenCmd)

	authRevokeCmd.Flags().BoolVar(&authRevokeAll, "all", false, "Revoke all CLI sessions (log out everywhere)")
}

// DeviceCodeResponse matches the API response.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// DeviceCodeStatusResponse matches the API response.
type DeviceCodeStatusResponse struct {
	Status           string     `json:"status"`
	AccessToken      string     `json:"access_token,omitempty"`
	RefreshToken     string     `json:"refresh_token,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	Email            string     `json:"email,omitempty"`
	OrganizationName string     `json:"organization_name,omitempty"`
}

func runAuthLogin(cmd *cobra.Command, args []string) error {
	// Check if already logged in with valid token
	if auth.HasCredentials() {
		creds, err := auth.LoadCredentials()
		if err == nil {
			if !creds.IsExpired() {
				Print("Already logged in as %s, re-authenticating...\n", creds.Email)
			} else {
				Print("Session expired, re-authenticating...\n")
			}
			auth.DeleteCredentials()
		}
	}

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())

	// Request device code
	Print("Requesting device code...\n")
	resp, err := client.PostJSON(context.Background(), "/api/cli/device/code", map[string]string{
		"client_id": "taufinity-cli",
	})
	if err != nil {
		return fmt.Errorf("request device code: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("request device code: %s", string(resp.Body))
	}

	var deviceResp DeviceCodeResponse
	if err := resp.DecodeJSON(&deviceResp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	// Display instructions
	Print("\nTo authenticate, visit:\n")
	Print("  %s\n\n", deviceResp.VerificationURI)
	Print("And enter this code:\n\n")
	Print("  %s\n\n", deviceResp.UserCode)

	// Try to open browser
	if !IsQuiet() {
		if err := browser.OpenURL(deviceResp.VerificationURI + "?code=" + deviceResp.UserCode); err != nil {
			Print("(Could not open browser automatically)\n")
		}
	}

	// Poll for approval
	Print("Waiting for approval...")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(deviceResp.ExpiresIn)*time.Second)
	defer cancel()

	// Add 1 second buffer to avoid rate limiting from timing jitter
	interval := time.Duration(deviceResp.Interval+1) * time.Second
	if interval < 5*time.Second {
		interval = 6 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			Print("\n")
			return fmt.Errorf("authorization timed out")
		case <-ticker.C:
			status, err := pollDeviceStatus(client, deviceResp.DeviceCode)
			if err != nil {
				Print("\n")
				return err
			}

			switch status.Status {
			case "approved":
				Print("\n\nAuthenticated successfully!\n")

				// Save credentials
				creds := &auth.Credentials{
					AccessToken:      status.AccessToken,
					RefreshToken:     status.RefreshToken,
					Email:            status.Email,
					OrganizationName: status.OrganizationName,
				}
				if status.ExpiresAt != nil {
					creds.AccessTokenExpiresAt = *status.ExpiresAt
					creds.ExpiresAt = *status.ExpiresAt
				} else {
					// Server mints a short-lived (1h) CLI access token.
					creds.AccessTokenExpiresAt = time.Now().Add(time.Hour)
					creds.ExpiresAt = creds.AccessTokenExpiresAt
				}

				if err := creds.Save(); err != nil {
					return fmt.Errorf("save credentials: %w", err)
				}

				if status.OrganizationName != "" {
					Print("Logged in as: %s (%s)\n", status.Email, status.OrganizationName)
				} else {
					Print("Logged in as: %s\n", status.Email)
				}
				return nil

			case "denied":
				Print("\n")
				return fmt.Errorf("authorization denied")

			case "expired":
				Print("\n")
				return fmt.Errorf("device code expired")

			case "pending":
				Print(".")
				continue

			default:
				Print("\n")
				return fmt.Errorf("unexpected status: %s", status.Status)
			}
		}
	}
}

func pollDeviceStatus(client *api.Client, deviceCode string) (*DeviceCodeStatusResponse, error) {
	resp, err := client.Get(context.Background(), "/api/cli/device/code/"+deviceCode+"/status")
	if err != nil {
		return nil, fmt.Errorf("poll status: %w", err)
	}
	if !resp.IsSuccess() {
		return nil, fmt.Errorf("poll status: %s", string(resp.Body))
	}

	var status DeviceCodeStatusResponse
	if err := resp.DecodeJSON(&status); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}

	return &status, nil
}

func runAuthRevoke(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		Print("Not logged in.\n")
		return nil
	}

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())

	if authRevokeAll {
		// Authenticated "log out everywhere": revokes every CLI session for the
		// user. getToken auto-refreshes a near-expired access token first.
		if err := client.RevokeAllRefreshTokens(); err != nil {
			Print("Warning: could not revoke all sessions server-side: %v\n", err)
		} else {
			Print("Revoked all CLI sessions.\n")
		}
	} else if creds, err := auth.LoadCredentials(); err == nil && creds.HasRefreshToken() {
		// Best-effort single-session revoke; works even with an expired access
		// token since the refresh token travels in the body.
		_ = client.RevokeRefreshToken(creds.RefreshToken)
	}

	if err := auth.DeleteCredentials(); err != nil {
		return fmt.Errorf("revoke credentials: %w", err)
	}

	Print("Credentials revoked. You are now logged out.\n")
	return nil
}

func runAuthStatus(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		Print("Not logged in.\n")
		Print("Run 'taufinity auth login' to authenticate.\n")
		return nil
	}

	creds, err := auth.LoadCredentials()
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}

	if creds.IsExpired() {
		Print("Status: Expired\n")
		Print("Email: %s\n", creds.Email)
		if creds.OrganizationName != "" {
			Print("Organization: %s\n", creds.OrganizationName)
		}
		Print("Expired: %s\n", creds.ExpiresAt.Format(time.RFC3339))
		Print("\nRun 'taufinity auth login' to re-authenticate.\n")
		return nil
	}

	// Validate with server
	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	resp, err := client.PostJSONWithAuth(context.Background(), "/api/cli/token/validate", nil)
	if err != nil {
		// Token validation failed - likely revoked or invalid
		Print("Status: Invalid\n")
		Print("Email: %s\n", creds.Email)
		if creds.OrganizationName != "" {
			Print("Organization: %s\n", creds.OrganizationName)
		}
		Print("\nSession is no longer valid. Run 'taufinity auth login' to re-authenticate.\n")
		return nil
	}

	if !resp.IsSuccess() {
		Print("Status: Invalid\n")
		Print("Email: %s\n", creds.Email)
		Print("\nSession rejected by server. Run 'taufinity auth login' to re-authenticate.\n")
		return nil
	}

	Print("Status: Authenticated\n")
	Print("Email: %s\n", creds.Email)
	if creds.OrganizationName != "" {
		Print("Organization: %s\n", creds.OrganizationName)
	}
	Print("Expires: %s\n", creds.ExpiresAt.Format(time.RFC3339))
	return nil
}

func runAuthToken(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated - run 'taufinity auth login' first")
	}

	// Route through the client's renewing token path so a near-expiry access
	// token is refreshed (and rotated) before it's handed out. With a 1h access
	// token, reading the stored token directly would frequently emit a stale or
	// expired value into scripts.
	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())

	token, err := client.Token(context.Background())
	if err != nil {
		return err
	}

	fmt.Println(token)
	return nil
}
