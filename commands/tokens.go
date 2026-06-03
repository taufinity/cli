package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/auth"
)

var tokensCmd = &cobra.Command{
	Use:   "tokens",
	Short: "Manage personal API tokens",
}

var tokensCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Mint a personal API token for the current user",
	RunE:  runTokensCreate,
}

var tokensListCmd = &cobra.Command{
	Use:   "list",
	Short: "List personal API tokens",
	RunE:  runTokensList,
}

var tokensRevokeCmd = &cobra.Command{
	Use:   "revoke <id>",
	Short: "Revoke a personal API token by ID",
	Args:  cobra.ExactArgs(1),
	RunE:  runTokensRevoke,
}

var (
	tokenName    string
	tokenExpires string
)

func init() {
	tokensCreateCmd.Flags().StringVar(&tokenName, "name", "", "Token name")
	tokensCreateCmd.Flags().StringVar(&tokenExpires, "expires", "365d", "Expiry (e.g. 90d, 365d, 1y)")

	tokensCmd.AddCommand(tokensCreateCmd, tokensListCmd, tokensRevokeCmd)
	rootCmd.AddCommand(tokensCmd)
}

func runTokensCreate(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated — run 'taufinity auth login' first")
	}

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())

	body := map[string]interface{}{
		"name":       tokenName,
		"expires_in": tokenExpires,
	}
	resp, err := client.PostJSONWithAuth(context.Background(), "/api/api-keys/personal", body)
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	if resp.StatusCode != 201 {
		return fmt.Errorf("server error %d: %s", resp.StatusCode, string(resp.Body))
	}

	var result struct {
		Key       string `json:"key"`
		KeyPrefix string `json:"key_prefix"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	fmt.Printf("Token created (shown once — store it securely):\n\n  %s\n\n", result.Key)
	fmt.Printf("Prefix: %s  Expires: %s\n", result.KeyPrefix, result.ExpiresAt)
	return nil
}

func runTokensList(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated — run 'taufinity auth login' first")
	}

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())

	resp, err := client.GetWithAuth(context.Background(), "/api/api-keys/personal")
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("list tokens failed: %s", string(resp.Body))
	}

	var result struct {
		Tokens []struct {
			ID         uint       `json:"id"`
			Name       string     `json:"name"`
			KeyPrefix  string     `json:"key_prefix"`
			ExpiresAt  *time.Time `json:"expires_at"`
			LastUsedAt *time.Time `json:"last_used_at"`
			CreatedAt  time.Time  `json:"created_at"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(result.Tokens) == 0 {
		fmt.Println("No personal tokens found.")
		return nil
	}

	fmt.Printf("%-6s  %-30s  %-10s  %-22s  %s\n", "ID", "Name", "Prefix", "Expires", "Last used")
	for _, t := range result.Tokens {
		exp := "never"
		if t.ExpiresAt != nil {
			exp = t.ExpiresAt.Format("2006-01-02")
		}
		lu := "never"
		if t.LastUsedAt != nil {
			lu = t.LastUsedAt.Format("2006-01-02 15:04")
		}
		fmt.Printf("%-6d  %-30s  %-10s  %-22s  %s\n", t.ID, t.Name, t.KeyPrefix, exp, lu)
	}
	return nil
}

func runTokensRevoke(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated — run 'taufinity auth login' first")
	}

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())

	resp, err := client.DeleteWithAuth(context.Background(), fmt.Sprintf("/api/api-keys/personal/%s", args[0]))
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if resp.StatusCode == 204 {
		fmt.Printf("Token %s revoked.\n", args[0])
		return nil
	}
	return fmt.Errorf("revoke failed %d: %s", resp.StatusCode, string(resp.Body))
}
