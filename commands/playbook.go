package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/auth"
)

var playbookCmd = &cobra.Command{
	Use:   "playbook",
	Short: "Playbook commands",
	Long:  `Commands for triggering and monitoring playbook runs.`,
}

var playbookTriggerCmd = &cobra.Command{
	Use:   "trigger <playbook-id>",
	Short: "Trigger a playbook run",
	Long: `Trigger a playbook run by ID.

Examples:
  # Trigger a playbook
  taufinity playbook trigger 11

  # Trigger with input from stdin
  echo '{"dev_work": "sync Odoo contacts"}' | taufinity playbook trigger 11 --input-file -

  # Trigger with input from file
  taufinity playbook trigger 11 --input-file inputs.json

  # Pass dry_run flag to the playbook
  taufinity playbook trigger 11 --dry-run

  # Wait for completion
  taufinity playbook trigger 11 --wait
`,
	Args: cobra.ExactArgs(1),
	RunE: runPlaybookTrigger,
}

var playbookListCmd = &cobra.Command{
	Use:   "list",
	Short: "List playbooks for the current organization",
	Long: `List all playbooks for the current organization.

Examples:
  taufinity playbook list
  taufinity playbook list --format json
  taufinity --org 12 playbook list
`,
	Args: cobra.NoArgs,
	RunE: runPlaybookList,
}

var playbookRunsCmd = &cobra.Command{
	Use:   "runs <playbook-id>",
	Short: "List recent runs for a playbook",
	Long: `List recent runs for a playbook by ID.

Examples:
  taufinity playbook runs 11
  taufinity playbook runs 11 --limit 5
`,
	Args: cobra.ExactArgs(1),
	RunE: runPlaybookRuns,
}

var (
	playbookInputFile   string
	playbookDryRun      bool
	playbookWait        bool
	playbookWaitTimeout time.Duration
	playbookRunsLimit   int
)

func init() {
	rootCmd.AddCommand(playbookCmd)
	playbookCmd.AddCommand(playbookListCmd)
	playbookCmd.AddCommand(playbookTriggerCmd)
	playbookCmd.AddCommand(playbookRunsCmd)

	playbookTriggerCmd.Flags().StringVar(&playbookInputFile, "input-file", "", "Path to JSON input file, or '-' for stdin")
	playbookTriggerCmd.Flags().BoolVar(&playbookDryRun, "dry-run", false, "Pass dry_run flag to the playbook (does not skip the API call)")
	playbookTriggerCmd.Flags().BoolVar(&playbookWait, "wait", false, "Poll until the run completes or fails")
	playbookTriggerCmd.Flags().DurationVar(&playbookWaitTimeout, "timeout", 10*time.Minute, "Timeout when waiting for completion (used with --wait)")

	playbookRunsCmd.Flags().IntVar(&playbookRunsLimit, "limit", 10, "Number of recent runs to show")
}

// playbookTriggerResponse is the API response from triggering a playbook.
// The API may return either run_id (string) or id (int) depending on version.
type playbookTriggerResponse struct {
	RunID  int    `json:"run_id"`
	ID     int    `json:"id"`
	Status string `json:"status"`
}

// effectiveRunID returns the run ID from whichever field is populated.
func (r *playbookTriggerResponse) effectiveRunID() int {
	if r.RunID != 0 {
		return r.RunID
	}
	return r.ID
}

// playbookRun represents a single playbook run.
type playbookRun struct {
	ID          int    `json:"id"`
	PlaybookID  int    `json:"playbook_id"`
	Status      string `json:"status"`
	Output      string `json:"output"`
	Error       string `json:"error"`
	CreatedAt   string `json:"created_at"`
	CompletedAt string `json:"completed_at"`
}

// playbookListItem represents a playbook returned by the list endpoint.
type playbookListItem struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	TriggerType string `json:"trigger_type"`
	Enabled     bool   `json:"enabled"`
	Schedule    string `json:"schedule,omitempty"`
}

func runPlaybookList(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	if org := GetOrg(); org != "" {
		client.SetOrg(org)
	}

	resp, err := client.GetWithAuth(context.Background(), "/api/playbooks")
	if err != nil {
		return fmt.Errorf("list playbooks: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("list playbooks failed: %s", string(resp.Body))
	}

	switch GetFormat() {
	case "json":
		fmt.Println(string(resp.Body))
		return nil
	case "yaml":
		var pbs []playbookListItem
		if err := json.Unmarshal(resp.Body, &pbs); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return printYAML(pbs)
	default:
		return printPlaybookListTable(resp.Body)
	}
}

func printPlaybookListTable(body []byte) error {
	var pbs []playbookListItem
	if err := json.Unmarshal(body, &pbs); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if len(pbs) == 0 {
		PrintLn("No playbooks found.")
		return nil
	}

	fmt.Printf("%-6s  %-30s  %-12s  %-8s  %s\n", "ID", "NAME", "TRIGGER", "ENABLED", "DESCRIPTION")
	fmt.Printf("%-6s  %-30s  %-12s  %-8s  %s\n",
		"------",
		"------------------------------",
		"------------",
		"--------",
		"-----------",
	)
	for _, pb := range pbs {
		name := pb.Name
		if len(name) > 30 {
			name = name[:27] + "..."
		}
		desc := pb.Description
		enabled := "yes"
		if !pb.Enabled {
			enabled = "no"
		}
		fmt.Printf("%-6d  %-30s  %-12s  %-8s  %s\n", pb.ID, name, pb.TriggerType, enabled, desc)
	}
	return nil
}

func runPlaybookTrigger(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}

	playbookID := args[0]

	// Build inputs map
	inputs := map[string]string{}

	// Read and parse input file if specified
	if playbookInputFile != "" {
		var raw []byte
		var err error
		if playbookInputFile == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(playbookInputFile)
		}
		if err != nil {
			return fmt.Errorf("read input file: %w", err)
		}

		// Parse JSON object and convert all values to strings
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return fmt.Errorf("parse input file: %w", err)
		}
		for k, v := range parsed {
			switch val := v.(type) {
			case string:
				inputs[k] = val
			default:
				// Re-encode non-string values as JSON to preserve structure
				b, err := json.Marshal(val)
				if err != nil {
					return fmt.Errorf("encode input key %q: %w", k, err)
				}
				inputs[k] = string(b)
			}
		}
	}

	if playbookDryRun {
		inputs["dry_run"] = "true"
	}

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	if org := GetOrg(); org != "" {
		client.SetOrg(org)
	}

	payload := map[string]any{
		"inputs": inputs,
	}

	path := fmt.Sprintf("/api/playbooks/%s/trigger", playbookID)
	Print("Triggering playbook %s...\n", playbookID)

	resp, err := client.PostJSONWithAuth(context.Background(), path, payload)
	if err != nil {
		return fmt.Errorf("trigger playbook: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("trigger failed: %s", string(resp.Body))
	}

	var result playbookTriggerResponse
	if err := resp.DecodeJSON(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	runID := result.effectiveRunID()
	Print("Run started: %d (status: %s)\n", runID, result.Status)

	if !playbookWait {
		return nil
	}

	// Poll until completion or failure
	Print("Waiting for completion (timeout: %s)...\n", playbookWaitTimeout)
	return pollPlaybookRun(client, playbookID, runID, playbookWaitTimeout)
}

func pollPlaybookRun(client *api.Client, playbookID string, runID int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	path := fmt.Sprintf("/api/playbooks/%s/runs", playbookID)

	for time.Now().Before(deadline) {
		resp, err := client.GetWithAuth(context.Background(), path)
		if err != nil {
			return fmt.Errorf("poll runs: %w", err)
		}
		if !resp.IsSuccess() {
			return fmt.Errorf("get runs failed: %s", string(resp.Body))
		}

		runs, err := decodeRunsArray(resp.Body)
		if err != nil {
			return fmt.Errorf("decode runs response: %w", err)
		}

		// Find our run by ID
		for _, run := range runs {
			if run.ID != runID {
				continue
			}
			switch run.Status {
			case "completed":
				Print("\nRun completed.\n")
				if run.Output != "" {
					fmt.Println(run.Output)
				}
				return nil
			case "failed":
				if run.Error != "" {
					return fmt.Errorf("run failed: %s", run.Error)
				}
				return fmt.Errorf("run failed")
			}
			// Still running — break inner loop and sleep
			break
		}

		Print(".")
		time.Sleep(3 * time.Second)
	}

	return fmt.Errorf("timed out waiting for run %d to complete", runID)
}

func runPlaybookRuns(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}

	playbookID := args[0]
	path := fmt.Sprintf("/api/playbooks/%s/runs?limit=%s", playbookID, strconv.Itoa(playbookRunsLimit))

	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	if org := GetOrg(); org != "" {
		client.SetOrg(org)
	}

	resp, err := client.GetWithAuth(context.Background(), path)
	if err != nil {
		return fmt.Errorf("get runs: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("get runs failed: %s", string(resp.Body))
	}

	switch GetFormat() {
	case "json":
		fmt.Println(string(resp.Body))
		return nil
	case "yaml":
		runs, err := decodeRunsArray(resp.Body)
		if err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return printYAML(runs)
	default:
		return printPlaybookRunsTable(resp.Body)
	}
}

// decodeRunsArray decodes the runs list endpoint response.
// The API returns a JSON array directly: [{...}, {...}]
func decodeRunsArray(body []byte) ([]playbookRun, error) {
	var runs []playbookRun
	if err := json.Unmarshal(body, &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

func printPlaybookRunsTable(body []byte) error {
	runs, err := decodeRunsArray(body)
	if err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if len(runs) == 0 {
		PrintLn("No runs found.")
		return nil
	}

	fmt.Printf("%-6s  %-12s  %-25s  %s\n", "ID", "STATUS", "CREATED AT", "ERROR")
	fmt.Printf("%-6s  %-12s  %-25s  %s\n",
		"------",
		"------------",
		"-------------------------",
		"-----",
	)
	for _, run := range runs {
		errCol := run.Error
		if len(errCol) > 50 {
			errCol = errCol[:47] + "..."
		}
		fmt.Printf("%-6d  %-12s  %-25s  %s\n", run.ID, run.Status, run.CreatedAt, errCol)
	}
	return nil
}
