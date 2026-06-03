package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/auth"
)

var asUserCmd = &cobra.Command{
	Use:   "as-user <user-id> -- <command>",
	Short: "Run a command as a specific user (headless impersonation)",
	Long: `Issues a short-lived impersonation token and injects it as TAUFINITY_IMPERSONATION_TOKEN.

Requires either:
  - An active CLI elevation (taufinity auth elevate), OR
  - TAUFINITY_CI_KEY env var (for CI pipelines)

The token is auto-revoked after the command exits (unless --no-revoke is passed).`,
	Args: cobra.MinimumNArgs(1),
	RunE: runAsUser,
}

var (
	asUserOrgID    uint
	asUserTTL      int
	asUserReason   string
	asUserNoRevoke bool
	asUserCIKey    string
)

func init() {
	asUserCmd.Flags().UintVar(&asUserOrgID, "org-id", 0, "Organization ID (required)")
	asUserCmd.Flags().IntVar(&asUserTTL, "ttl", 60, "Impersonation token TTL in minutes")
	asUserCmd.Flags().StringVar(&asUserReason, "reason", "", "Reason for impersonation (logged)")
	asUserCmd.Flags().BoolVar(&asUserNoRevoke, "no-revoke", false, "Keep token alive after command exits")
	asUserCmd.Flags().StringVar(&asUserCIKey, "ci-key", "", "CI admin key (overrides TAUFINITY_CI_KEY env var)")
	rootCmd.AddCommand(asUserCmd)
}

func runAsUser(cmd *cobra.Command, args []string) error {
	// Parse user ID (first positional arg).
	userID, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid user-id %q", args[0])
	}

	// Everything after the user-id (or after --) is the sub-command.
	subcmd := args[1:]

	if asUserOrgID == 0 {
		return fmt.Errorf("--org-id is required")
	}

	// Determine which bearer credential to use.
	ciKey := asUserCIKey
	if ciKey == "" {
		ciKey = os.Getenv("TAUFINITY_CI_KEY")
	}

	var bearerToken string
	if ciKey != "" {
		bearerToken = ciKey
	} else {
		// Fall back to CLI elevation token.
		elToken, _, expiresAt, loadErr := auth.LoadElevationToken()
		if loadErr != nil {
			return fmt.Errorf("load elevation token: %w", loadErr)
		}
		if elToken == "" {
			return fmt.Errorf("no elevation token found — run 'taufinity auth elevate' first, or set TAUFINITY_CI_KEY")
		}
		if time.Now().After(expiresAt) {
			return fmt.Errorf("elevation token expired — run 'taufinity auth elevate' to renew")
		}
		remaining := time.Until(expiresAt)
		if remaining < 15*time.Minute {
			fmt.Fprintf(os.Stderr, "[impersonate] Warning: elevation expires in %s — consider renewing\n",
				remaining.Round(time.Minute))
		}
		bearerToken = elToken
	}

	// Request an impersonation token.
	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	client.SetAuth(bearerToken)

	resp, err := client.PostJSONWithAuth(context.Background(), "/api/auth/impersonate-token", map[string]interface{}{
		"user_id":     userID,
		"org_id":      asUserOrgID,
		"ttl_minutes": asUserTTL,
		"reason":      asUserReason,
		"no_revoke":   asUserNoRevoke,
	})
	if err != nil {
		return fmt.Errorf("impersonate-token: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("impersonate-token failed %d: %s", resp.StatusCode, string(resp.Body))
	}

	var result struct {
		Token     string    `json:"token"`
		SessionID uint      `json:"session_id"`
		ExpiresAt time.Time `json:"expires_at"`
		UserID    uint      `json:"user_id"`
		OrgID     uint      `json:"org_id"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[impersonate] token issued for user %d in org %d, expires %s (session %d)\n",
		result.UserID, result.OrgID, result.ExpiresAt.Local().Format("15:04"), result.SessionID)

	// Auto-revoke on exit (best-effort), unless --no-revoke.
	if !asUserNoRevoke {
		defer func() {
			revokeClient := api.New(GetAPIURL())
			revokeClient.SetAuth(bearerToken)
			rResp, rErr := revokeClient.DeleteWithAuth(context.Background(),
				fmt.Sprintf("/api/auth/impersonate-token/%d", result.SessionID))
			if rErr != nil {
				fmt.Fprintf(os.Stderr, "[impersonate] warning: auto-revoke failed: %v\n", rErr)
				return
			}
			rResp.Body = rResp.Body // consumed already by httpclient
			if rResp.StatusCode == 204 || rResp.StatusCode == 200 {
				fmt.Fprintf(os.Stderr, "[impersonate] token revoked\n")
			}
		}()
	}

	// If no sub-command supplied, just print the token.
	if len(subcmd) == 0 {
		fmt.Printf("TAUFINITY_IMPERSONATION_TOKEN=%s\n", result.Token)
		return nil
	}

	// Run the sub-command with the impersonation token injected.
	exe := exec.Command(subcmd[0], subcmd[1:]...)
	exe.Stdout = os.Stdout
	exe.Stderr = os.Stderr
	exe.Stdin = os.Stdin
	exe.Env = append(os.Environ(), fmt.Sprintf("TAUFINITY_IMPERSONATION_TOKEN=%s", result.Token))
	return exe.Run()
}
