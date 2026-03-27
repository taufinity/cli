package commands

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"test-go/cmd/taufinity/internal/api"
	"test-go/cmd/taufinity/internal/auth"
)

var orgCmd = &cobra.Command{
	Use:   "org",
	Short: "Organization commands",
}

var orgListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available organizations",
	RunE:  runOrgList,
}

func init() {
	rootCmd.AddCommand(orgCmd)
	orgCmd.AddCommand(orgListCmd)
}

type orgEntry struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func runOrgList(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())

	resp, err := client.GetWithAuth(context.Background(), "/api/organizations")
	if err != nil {
		return fmt.Errorf("list orgs: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("list orgs failed: %s", string(resp.Body))
	}

	switch GetFormat() {
	case "json":
		fmt.Println(string(resp.Body))
		return nil
	default:
		var orgs []orgEntry
		if err := json.Unmarshal(resp.Body, &orgs); err != nil {
			return fmt.Errorf("decode orgs: %w", err)
		}
		if len(orgs) == 0 {
			PrintLn("No organizations found.")
			return nil
		}

		currentOrg := GetOrg()
		fmt.Printf("%-6s  %-40s  %s\n", "ID", "NAME", "")
		fmt.Printf("%-6s  %-40s  %s\n", "------", "----------------------------------------", "")
		for _, org := range orgs {
			marker := ""
			if fmt.Sprintf("%d", org.ID) == currentOrg {
				marker = " (active)"
			}
			fmt.Printf("%-6d  %-40s  %s\n", org.ID, org.Name, marker)
		}
		return nil
	}
}
