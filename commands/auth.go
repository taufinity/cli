package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pkg/browser"
	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/auth"
	"github.com/taufinity/cli/internal/telemetry"
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

var authElevateCmd = &cobra.Command{
	Use:   "elevate",
	Short: "Elevate CLI session via TOTP (grants impersonation for 16h by default)",
	RunE:  runAuthElevate,
}

var authElevateStatusCmd = &cobra.Command{
	Use:   "elevate-status",
	Short: "Show current CLI elevation status",
	RunE:  runAuthElevateStatus,
}

var authRevokeElevationCmd = &cobra.Command{
	Use:   "revoke-elevation",
	Short: "Revoke CLI elevation (server-side + local)",
	RunE:  runAuthRevokeElevation,
}

var elevationTTL string

func init() {
	rootCmd.AddCommand(authCmd)
	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authRevokeCmd)
	authCmd.AddCommand(authStatusCmd)
	authCmd.AddCommand(authTokenCmd)
	authCmd.AddCommand(authElevateCmd)
	authCmd.AddCommand(authElevateStatusCmd)
	authCmd.AddCommand(authRevokeElevationCmd)

	authRevokeCmd.Flags().BoolVar(&authRevokeAll, "all", false, "Revoke all CLI sessions (log out everywhere)")
	authElevateCmd.Flags().StringVar(&elevationTTL, "ttl", "16h", "Elevation TTL (e.g. 4h, 16h, 24h)")
}

func runAuthElevate(cmd *cobra.Command, args []string) error {
	// Parse TTL
	ttlDur, err := time.ParseDuration(elevationTTL)
	if err != nil {
		return fmt.Errorf("invalid TTL %q: %w", elevationTTL, err)
	}
	ttlMin := int(ttlDur.Minutes())
	if ttlMin < 60 {
		ttlMin = 60
	}
	if ttlMin > 1440 {
		ttlMin = 1440
	}

	fmt.Fprint(os.Stdout, "Verification code: ")
	reader := bufio.NewReader(os.Stdin)
	code, _ := reader.ReadString('\n')
	code = strings.TrimSpace(code)

	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated — run 'taufinity auth login' first")
	}

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())

	resp, err := client.PostJSONWithAuth(context.Background(), "/api/auth/cli-elevate", map[string]interface{}{
		"totp_code":   code,
		"ttl_minutes": ttlMin,
	})
	if err != nil {
		return fmt.Errorf("cli-elevate: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("elevation failed %d: %s", resp.StatusCode, string(resp.Body))
	}

	var result struct {
		Token     string    `json:"token"`
		SessionID uint      `json:"session_id"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if err := auth.SaveElevationToken(result.Token, result.SessionID, result.ExpiresAt); err != nil {
		return fmt.Errorf("save elevation token: %w", err)
	}

	fmt.Printf("Elevated for %s (until %s). Run 'taufinity auth elevate-status' to check.\n",
		elevationTTL, result.ExpiresAt.Local().Format("15:04"))
	return nil
}

func runAuthElevateStatus(cmd *cobra.Command, args []string) error {
	token, sessionID, expiresAt, err := auth.LoadElevationToken()
	if err != nil {
		return fmt.Errorf("load elevation token: %w", err)
	}
	if token == "" {
		fmt.Println("No active elevation. Run 'taufinity auth elevate' to elevate.")
		return nil
	}
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		fmt.Printf("Elevation expired at %s. Run 'taufinity auth elevate' to renew.\n",
			expiresAt.Local().Format("15:04"))
		return nil
	}
	fmt.Printf("Elevated — session %d, expires at %s (%s remaining).\n",
		sessionID, expiresAt.Local().Format("15:04"), remaining.Round(time.Minute))
	if remaining < 15*time.Minute {
		fmt.Fprintln(os.Stderr, "Warning: elevation expires soon. Run 'taufinity auth elevate' to renew.")
	}
	return nil
}

func runAuthRevokeElevation(cmd *cobra.Command, args []string) error {
	token, sessionID, _, err := auth.LoadElevationToken()
	if err != nil {
		return fmt.Errorf("load elevation token: %w", err)
	}
	if token == "" {
		fmt.Println("No elevation token found.")
		return nil
	}

	// Revoke server-side first using the elevation token as bearer.
	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	client.SetAuth(token)

	resp, err := client.DeleteWithAuth(context.Background(),
		fmt.Sprintf("/api/auth/cli-elevate/%d", sessionID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: server-side revoke failed: %v — local token NOT removed.\n", err)
		return fmt.Errorf("server-side revoke failed, local token kept")
	}
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "Warning: server returned %d: %s — local token NOT removed.\n",
			resp.StatusCode, string(resp.Body))
		return fmt.Errorf("server revoke returned %d", resp.StatusCode)
	}

	// Remove local token only after server confirms.
	if err := auth.RemoveElevationToken(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove local elevation token: %v\n", err)
	}
	fmt.Println("Elevation revoked.")
	return nil
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
			reportAuthFailure("auth_timeout", "authorization timed out")
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
				reportAuthFailure("auth_denied", "authorization denied")
				return fmt.Errorf("authorization denied")

			case "expired":
				Print("\n")
				reportAuthFailure("device_code_expired", "device code expired")
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

// reportAuthFailure reports an auth failure event to telemetry, attaching the
// stored email when the CLI is already authenticated (e.g. token refresh failure).
func reportAuthFailure(code, message string) {
	e := telemetry.Event{
		EventType:    "auth.failure",
		ErrorCode:    code,
		ErrorMessage: message,
	}
	if creds, err := auth.LoadCredentials(); err == nil {
		e.Email = creds.Email
	}
	telemetry.Report(e)
}
