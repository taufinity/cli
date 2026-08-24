package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/auth"
)

var knowledgeCmd = &cobra.Command{
	Use:     "knowledge",
	Aliases: []string{"kf"},
	Short:   "Knowledge file commands",
	Long: `Commands for listing and reading an organization's knowledge files.

Knowledge files are the org's own content store: uploaded documents, scraped
pages, and anything playbooks write, such as meeting notes. Everything here is
read-only; writing goes through the portal or a playbook.`,
}

var knowledgeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List knowledge files",
	Long: `List knowledge files for the organization.

Examples:
  taufinity knowledge list --org 12
  taufinity knowledge list --org 12 --tags meeting-transcripts
  taufinity knowledge list --org 12 --q "DSlab" --format json
`,
	Args: cobra.NoArgs,
	RunE: runKnowledgeList,
}

var knowledgeGetCmd = &cobra.Command{
	Use:   "get <uuid>",
	Short: "Show a knowledge file's metadata",
	Args:  cobra.ExactArgs(1),
	RunE:  runKnowledgeGet,
}

var knowledgeContentCmd = &cobra.Command{
	Use:   "content <uuid>",
	Short: "Print a knowledge file's contents",
	Long: `Print the decrypted contents of one knowledge file.

Reads /api/knowledge-files/{uuid}/content, which is org-member level: any member
of the organization can read its own files. That is deliberately a lower bar than
'get', whose extracted_text_full field is admin-only.

Examples:
  taufinity knowledge content 4c61faa4-... --org 12
  taufinity knowledge content 4c61faa4-... --org 12 -o notes.md
`,
	Args: cobra.ExactArgs(1),
	RunE: runKnowledgeContent,
}

var (
	knowledgeOrg    string
	knowledgeTags   string
	knowledgeQuery  string
	knowledgeLimit  int
	knowledgeOutput string
)

func init() {
	rootCmd.AddCommand(knowledgeCmd)
	knowledgeCmd.AddCommand(knowledgeListCmd)
	knowledgeCmd.AddCommand(knowledgeGetCmd)
	knowledgeCmd.AddCommand(knowledgeContentCmd)

	for _, c := range []*cobra.Command{knowledgeListCmd, knowledgeGetCmd, knowledgeContentCmd} {
		c.Flags().StringVar(&knowledgeOrg, "org", "", "Organization ID (numeric)")
	}
	knowledgeListCmd.Flags().StringVar(&knowledgeTags, "tags", "", "Comma-separated tag names")
	knowledgeListCmd.Flags().StringVar(&knowledgeQuery, "q", "", "Search query")
	knowledgeListCmd.Flags().IntVar(&knowledgeLimit, "limit", 50, "Maximum files to list")
	knowledgeContentCmd.Flags().StringVarP(&knowledgeOutput, "output", "o", "", "Write to a file instead of stdout")
}

// newKnowledgeClient mirrors newDeliverableClient: the subcommands declare their
// own --org, which shadows the persistent flag, so the value has to be pushed
// onto the client explicitly or the request stays scoped to the session's
// current organization.
func newKnowledgeClient() *api.Client {
	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	client.SetDryRun(IsDryRun())
	org := GetOrg()
	if knowledgeOrg != "" {
		org = knowledgeOrg
	}
	if org != "" {
		client.SetOrg(org)
	}
	return client
}

type knowledgeFileItem struct {
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
	MimeType   string `json:"mime_type"`
	TokenCount int    `json:"token_count"`
	UpdatedAt  string `json:"updated_at"`
	Tags       []struct {
		Name string `json:"name"`
	} `json:"tags"`
}

func runKnowledgeList(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}

	q := url.Values{}
	if knowledgeTags != "" {
		q.Set("tags", knowledgeTags)
	}
	if knowledgeQuery != "" {
		q.Set("q", knowledgeQuery)
	}
	if knowledgeLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", knowledgeLimit))
	}
	path := "/api/knowledge-files"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	resp, err := newKnowledgeClient().GetWithAuth(context.Background(), path)
	if err != nil {
		return fmt.Errorf("list knowledge files: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("list knowledge files failed: %s", string(resp.Body))
	}

	if GetFormat() == "json" {
		fmt.Println(string(resp.Body))
		return nil
	}

	var payload struct {
		Files []knowledgeFileItem `json:"files"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return fmt.Errorf("decode knowledge files: %w", err)
	}
	if len(payload.Files) == 0 {
		PrintLn("No knowledge files found.")
		return nil
	}
	fmt.Printf("%-38s  %-7s  %-24s  %s\n", "UUID", "TOKENS", "TAGS", "NAME")
	for _, f := range payload.Files {
		tags := make([]string, 0, len(f.Tags))
		for _, t := range f.Tags {
			tags = append(tags, t.Name)
		}
		fmt.Printf("%-38s  %-7d  %-24s  %s\n", f.UUID, f.TokenCount,
			truncate(strings.Join(tags, ","), 24), f.Name)
	}
	return nil
}

func runKnowledgeGet(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}
	resp, err := newKnowledgeClient().GetWithAuth(context.Background(), "/api/knowledge-files/"+args[0])
	if err != nil {
		return fmt.Errorf("get knowledge file: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("get knowledge file failed: %s", string(resp.Body))
	}
	fmt.Println(string(resp.Body))
	return nil
}

func runKnowledgeContent(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}
	resp, err := newKnowledgeClient().GetWithAuth(context.Background(),
		"/api/knowledge-files/"+args[0]+"/content")
	if err != nil {
		return fmt.Errorf("read knowledge file: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("read knowledge file failed: %s", string(resp.Body))
	}
	if knowledgeOutput != "" {
		if err := os.WriteFile(knowledgeOutput, resp.Body, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", knowledgeOutput, err)
		}
		Print("Wrote %d bytes to %s\n", len(resp.Body), knowledgeOutput)
		return nil
	}
	_, err = os.Stdout.Write(resp.Body)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
