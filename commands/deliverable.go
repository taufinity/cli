package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/auth"
)

var deliverableCmd = &cobra.Command{
	Use:     "deliverable",
	Aliases: []string{"dlv"},
	Short:   "Deliverable commands",
	Long:    `Commands for uploading, listing, and deleting customer deliverables.`,
}

var deliverableUploadCmd = &cobra.Command{
	Use:   "upload",
	Short: "Upload a deliverable",
	Long: `Upload a deliverable file to an organization.

Examples:
  # Upload a ZIP file
  taufinity deliverable upload --file ./build.zip --name "Frontend v2" --org 12

  # Upload with description and slug
  taufinity deliverable upload --file ./build.zip --name "Frontend v2" --org 12 --description "Production build" --slug frontend-v2

  # Upload with custom entry file
  taufinity deliverable upload --file ./build.zip --name "Frontend v2" --org 12 --entry-file index.html
`,
	RunE: runDeliverableUpload,
}

var deliverableListCmd = &cobra.Command{
	Use:   "list",
	Short: "List deliverables for the current organization",
	Long: `List all deliverables for the current organization.

Examples:
  taufinity deliverable list
  taufinity deliverable list --format json
  taufinity --org 12 deliverable list
`,
	Args: cobra.NoArgs,
	RunE: runDeliverableList,
}

var deliverableDeleteCmd = &cobra.Command{
	Use:   "delete <uuid>",
	Short: "Delete a deliverable",
	Long: `Delete a deliverable by UUID.

Examples:
  taufinity deliverable delete abc123-def456
`,
	Args: cobra.ExactArgs(1),
	RunE: runDeliverableDelete,
}

var (
	deliverableFile        string
	deliverableName        string
	deliverableOrg         string
	deliverableDescription string
	deliverableSlug        string
	deliverableEntryFile   string
)

func init() {
	rootCmd.AddCommand(deliverableCmd)
	deliverableCmd.AddCommand(deliverableUploadCmd)
	deliverableCmd.AddCommand(deliverableListCmd)
	deliverableCmd.AddCommand(deliverableDeleteCmd)

	deliverableUploadCmd.Flags().StringVarP(&deliverableFile, "file", "f", "", "Path to the deliverable file (required)")
	deliverableUploadCmd.Flags().StringVar(&deliverableName, "name", "", "Deliverable name (required)")
	deliverableUploadCmd.Flags().StringVar(&deliverableOrg, "org", "", "Organization ID (numeric)")
	deliverableUploadCmd.Flags().StringVar(&deliverableDescription, "description", "", "Deliverable description")
	deliverableUploadCmd.Flags().StringVar(&deliverableSlug, "slug", "", "URL slug for the deliverable")
	deliverableUploadCmd.Flags().StringVar(&deliverableEntryFile, "entry-file", "", "Entry file within the archive (e.g. index.html)")

	_ = deliverableUploadCmd.MarkFlagRequired("file")
	_ = deliverableUploadCmd.MarkFlagRequired("name")
}

// deliverableListItem represents a deliverable returned by the list endpoint.
type deliverableListItem struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	EntryFile   string `json:"entry_file"`
	CreatedAt   string `json:"created_at"`
}

func newDeliverableClient() *api.Client {
	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	client.SetDryRun(IsDryRun())
	if org := GetOrg(); org != "" {
		client.SetOrg(org)
	}
	return client
}

func runDeliverableUpload(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}

	// Open and stat the file
	file, err := os.Open(deliverableFile)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	// Build multipart body
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add file part
	part, err := writer.CreateFormFile("file", filepath.Base(deliverableFile))
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return fmt.Errorf("copy file: %w", err)
	}

	// Add form fields
	_ = writer.WriteField("name", deliverableName)
	if deliverableOrg != "" {
		_ = writer.WriteField("organization_id", deliverableOrg)
	}
	if deliverableDescription != "" {
		_ = writer.WriteField("description", deliverableDescription)
	}
	if deliverableSlug != "" {
		_ = writer.WriteField("slug", deliverableSlug)
	}
	if deliverableEntryFile != "" {
		_ = writer.WriteField("entry_file", deliverableEntryFile)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	client := newDeliverableClient()

	Print("Uploading deliverable %q...\n", deliverableName)

	resp, err := client.PostMultipart(context.Background(), "/api/admin/deliverables", &buf, writer.FormDataContentType())
	if err != nil {
		return fmt.Errorf("upload deliverable: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("upload failed: %s", string(resp.Body))
	}

	Print("Deliverable uploaded successfully.\n")

	switch GetFormat() {
	case "json":
		fmt.Println(string(resp.Body))
	default:
		var result deliverableListItem
		if err := json.Unmarshal(resp.Body, &result); err == nil && result.UUID != "" {
			Print("  UUID: %s\n", result.UUID)
			Print("  Slug: %s\n", result.Slug)
		}
	}

	return nil
}

func runDeliverableList(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}

	client := newDeliverableClient()

	resp, err := client.GetWithAuth(context.Background(), "/api/deliverables")
	if err != nil {
		return fmt.Errorf("list deliverables: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("list deliverables failed: %s", string(resp.Body))
	}

	items, err := parseDeliverableList(resp.Body)
	if err != nil {
		return err
	}

	switch GetFormat() {
	case "json":
		fmt.Println(string(resp.Body))
		return nil
	case "yaml":
		return printYAML(items)
	default:
		return printDeliverableTable(items)
	}
}

// parseDeliverableList decodes the backend response, which wraps the list
// in an envelope: {"deliverables": [...]}.
func parseDeliverableList(body []byte) ([]deliverableListItem, error) {
	var envelope struct {
		Deliverables []deliverableListItem `json:"deliverables"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return envelope.Deliverables, nil
}

func printDeliverableTable(items []deliverableListItem) error {

	if len(items) == 0 {
		PrintLn("No deliverables found.")
		return nil
	}

	fmt.Printf("%-36s  %-25s  %-20s  %-25s  %s\n", "UUID", "NAME", "SLUG", "CREATED AT", "DESCRIPTION")
	fmt.Printf("%-36s  %-25s  %-20s  %-25s  %s\n",
		"------------------------------------",
		"-------------------------",
		"--------------------",
		"-------------------------",
		"-----------",
	)
	for _, item := range items {
		name := item.Name
		if len(name) > 25 {
			name = name[:22] + "..."
		}
		slug := item.Slug
		if len(slug) > 20 {
			slug = slug[:17] + "..."
		}
		desc := item.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}
		fmt.Printf("%-36s  %-25s  %-20s  %-25s  %s\n", item.UUID, name, slug, item.CreatedAt, desc)
	}
	return nil
}

func runDeliverableDelete(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}

	uuid := args[0]
	client := newDeliverableClient()

	path := fmt.Sprintf("/api/admin/deliverables/%s", uuid)
	Print("Deleting deliverable %s...\n", uuid)

	resp, err := client.DeleteWithAuth(context.Background(), path)
	if err != nil {
		return fmt.Errorf("delete deliverable: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("delete failed: %s", string(resp.Body))
	}

	Print("Deliverable deleted.\n")
	return nil
}
