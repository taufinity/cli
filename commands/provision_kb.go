// provision_kb.go — move knowledge-base content between Studio instances.
//
//	provision kb-export --dir DIR --org SLUG [--tag T] [--include-quotes]
//	    Dumps every knowledge file in the org to DIR: the content as
//	    DIR/<name>, plus a DIR/<name>.yaml sidecar in exactly the schema the
//	    apply path reads back.
//
//	provision kb-import --dir DIR --org SLUG
//	    Walks DIR for *.yaml sidecars and pushes them through the same upsert
//	    path apply uses — one code path for every knowledge write, so the
//	    content-checksum short-circuit gives idempotency for free.
//
// Both commands target the instance in --api-url and authenticate with the
// admin API key, same as the rest of provision. Export is a read-only operation
// against the source instance; the only writes it makes are to local disk.
//
// Files of type `quote` are excluded from export by default. They tend to hold
// verbatim customer records, and an export directory is easy to commit to git
// by accident. --include-quotes opts in deliberately.
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	kbDir           string
	kbOrgSlug       string
	kbAPIKey        string
	kbTag           string
	kbIncludeQuotes bool
	kbForce         bool
)

var provisionKBExportCmd = &cobra.Command{
	Use:   "kb-export",
	Short: "Export an org's knowledge base to a local directory",
	Long: `Dump every knowledge file in the org to --dir: the extracted content
as <name>, plus a <name>.yaml sidecar carrying name, file_type, purpose and
tags. The output directory is directly importable with 'provision kb-import'
and is the same layout 'provision apply' reads from knowledge-base/.

Files of type 'quote' are skipped unless --include-quotes is passed.`,
	RunE: runProvisionKBExport,
}

var provisionKBImportCmd = &cobra.Command{
	Use:   "kb-import",
	Short: "Import a knowledge-base directory into an org",
	Long: `Walk --dir for *.yaml sidecars and upsert each one into the target org.
Idempotent: the server short-circuits to a no-op when the content checksum is
unchanged.`,
	RunE: runProvisionKBImport,
}

func init() {
	provisionCmd.AddCommand(provisionKBExportCmd)
	provisionCmd.AddCommand(provisionKBImportCmd)

	for _, cmd := range []*cobra.Command{provisionKBExportCmd, provisionKBImportCmd} {
		cmd.Flags().StringVar(&kbDir, "dir", "", "Knowledge-base directory (required)")
		cmd.Flags().StringVar(&kbOrgSlug, "org", "", "Organization slug (required)")
		cmd.Flags().StringVar(&kbAPIKey, "api-key", "", "Bootstrap admin API key (overrides TAUFINITY_ADMIN_TOKEN env)")
		_ = cmd.MarkFlagRequired("dir")
		_ = cmd.MarkFlagRequired("org")
	}
	provisionKBExportCmd.Flags().StringVar(&kbTag, "tag", "", "Only export files carrying this tag")
	provisionKBExportCmd.Flags().BoolVar(&kbIncludeQuotes, "include-quotes", false, "Include file_type=quote (may contain verbatim customer records)")
	provisionKBExportCmd.Flags().BoolVar(&kbForce, "force", false, "Allow writing into a non-empty --dir")
}

// resolveKBAPIKey mirrors resolveProvisionAPIKey but reads the kb-command flag.
func resolveKBAPIKey() (string, error) {
	if kbAPIKey != "" {
		return kbAPIKey, nil
	}
	return resolveProvisionAPIKey()
}

// ─── Export ──────────────────────────────────────────────────────────────────

// kbListResponse / kbListItem / kbTagItem are declared in provision_images.go —
// the same list endpoint backs both features.

// kbGetResponse is GET /api/knowledge-files/{uuid}. The full extracted text is
// admin-only, so a token without admin scope gets an empty content field here.
type kbGetResponse struct {
	kbListItem
	ExtractedTextFull string `json:"extracted_text_full,omitempty"`
}

func runProvisionKBExport(cmd *cobra.Command, args []string) error {
	key, err := resolveKBAPIKey()
	if err != nil {
		return err
	}
	dryRun := IsDryRun()

	// Refuse to write into a populated directory unless asked: pointing --dir at
	// an existing tree would otherwise overwrite files with no warning.
	if !dryRun {
		if entries, err := os.ReadDir(kbDir); err == nil && len(entries) > 0 && !kbForce {
			return fmt.Errorf("kb-export: %s is not empty (%d entries) — pass --force to overwrite, or pick a fresh --dir",
				kbDir, len(entries))
		}
		if err := os.MkdirAll(kbDir, 0o755); err != nil {
			return fmt.Errorf("kb-export: mkdir %s: %w", kbDir, err)
		}
	}

	// dryRun=false on the client: export issues no API writes, and a dry-run
	// client would stub out GETs it needs.
	c := newProvisionClient(GetAPIURL(), key, false)
	orgID, err := resolveProvisionOrgID(c, kbOrgSlug)
	if err != nil {
		return fmt.Errorf("kb-export: resolve org %q: %w", kbOrgSlug, err)
	}

	return exportKnowledgeBase(c, orgID, kbDir, kbTag, kbIncludeQuotes, dryRun)
}

func exportKnowledgeBase(c *provisionClient, orgID uint, outDir, tagFilter string, includeQuotes, dryRun bool) error {
	body, status, err := c.getForOrg(fmt.Sprintf("/knowledge-files?organization_id=%d&limit=500", orgID), orgID)
	if err != nil || status != 200 {
		return provisionAPIErr("kb-export: list", status, body, err)
	}
	var list kbListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		// Some endpoints return a bare array rather than an envelope.
		if err2 := json.Unmarshal(body, &list.Files); err2 != nil {
			return fmt.Errorf("kb-export: parse list: %w (body=%s)", err, provisionSummarize(body))
		}
	}
	if len(list.Files) == 0 {
		fmt.Printf("kb-export: no knowledge files in org %d\n", orgID)
		return nil
	}

	matched := make([]kbListItem, 0, len(list.Files))
	for _, f := range list.Files {
		if !includeQuotes && f.FileType == "quote" {
			continue
		}
		if tagFilter != "" && !hasKBTag(f.Tags, tagFilter) {
			continue
		}
		matched = append(matched, f)
	}
	fmt.Printf("kb-export: %d of %d files match\n", len(matched), len(list.Files))

	var written, failed int
	for _, f := range matched {
		if dryRun {
			fmt.Printf("  [dry-run] would export %s (file_type=%s, %d tags)\n", f.Name, f.FileType, len(f.Tags))
			written++
			continue
		}
		if err := exportOneKnowledgeFile(c, orgID, outDir, f); err != nil {
			c.Warn("kb-export: %q: %v", f.Name, err)
			failed++
			continue
		}
		written++
	}

	fmt.Printf("kb-export: done — written=%d failed=%d (dir=%s, dry-run=%v)\n", written, failed, outDir, dryRun)
	if failed > 0 {
		return fmt.Errorf("kb-export: %d file(s) failed", failed)
	}
	return nil
}

func exportOneKnowledgeFile(c *provisionClient, orgID uint, outDir string, f kbListItem) error {
	body, status, err := c.getForOrg("/knowledge-files/"+f.UUID, orgID)
	if err != nil || status != 200 {
		return provisionAPIErr("get", status, body, err)
	}
	var detail kbGetResponse
	if err := json.Unmarshal(body, &detail); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if detail.ExtractedTextFull == "" {
		return fmt.Errorf("empty content — the full text is admin-only, check the token has admin scope")
	}

	// The file name comes from the server, so keep it inside outDir: a name
	// containing path separators would otherwise write outside the export dir.
	base := filepath.Base(f.Name)
	if base == "." || base == string(filepath.Separator) || strings.TrimSpace(base) == "" {
		return fmt.Errorf("unusable file name %q", f.Name)
	}

	contentPath := filepath.Join(outDir, base)
	if err := os.WriteFile(contentPath, []byte(detail.ExtractedTextFull), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", contentPath, err)
	}

	tagNames := make([]string, 0, len(f.Tags))
	for _, t := range f.Tags {
		tagNames = append(tagNames, t.Name)
	}
	sidecar := knowledgeFileConfig{
		Name:        f.Name,
		FileType:    f.FileType,
		Purpose:     f.Purpose,
		Tags:        tagNames,
		ContentPath: "./" + base,
	}
	yamlBytes, err := yaml.Marshal(sidecar)
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	yamlPath := filepath.Join(outDir, base+".yaml")
	if err := os.WriteFile(yamlPath, yamlBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", yamlPath, err)
	}

	fmt.Printf("  exported %s (file_type=%s, %d tags, %d bytes)\n",
		f.Name, f.FileType, len(tagNames), len(detail.ExtractedTextFull))
	return nil
}

func hasKBTag(tags []kbTagItem, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(t.Name, want) {
			return true
		}
	}
	return false
}

// ─── Import ──────────────────────────────────────────────────────────────────

func runProvisionKBImport(cmd *cobra.Command, args []string) error {
	key, err := resolveKBAPIKey()
	if err != nil {
		return err
	}
	c := newProvisionClient(GetAPIURL(), key, IsDryRun())
	orgID, err := resolveProvisionOrgID(c, kbOrgSlug)
	if err != nil {
		return fmt.Errorf("kb-import: resolve org %q: %w", kbOrgSlug, err)
	}
	if err := provisionKnowledgeBase(c, kbDir, orgID); err != nil {
		return fmt.Errorf("kb-import: %w", err)
	}
	fmt.Printf("kb-import: done (dry-run=%v)\n", IsDryRun())
	return nil
}
