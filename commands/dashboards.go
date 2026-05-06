package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Dashboard specs in the felix repo live as JSON files. The `dashboards sync`
// subcommand walks a directory, resolves the target org + BigQuery provider,
// and creates or updates each dashboard via the admin API. Every write is
// snapshotted server-side for rollback.
//
// Authentication uses a bootstrap admin API key (X-API-Key header). Supply
// it via --api-key or the SITEGEN_API_KEY env var. The CLI's normal
// Bearer-token auth would require session TOTP elevation for admin endpoints,
// which is not scriptable.

var (
	dashboardsDir      string
	dashboardsOrgSlug  string
	dashboardsProvider string
	dashboardsAPIKey   string
)

var dashboardsCmd = &cobra.Command{
	Use:   "dashboards",
	Short: "Manage Studio dashboard definitions",
	Long: `Commands for creating, updating, and synchronising Studio dashboard
definitions from JSON spec files.

The canonical source of truth lives next to the dbt views that back each
dashboard — e.g. felix/docs/dashboards/felix/*.json for Felix Works.
Use 'taufinity dashboards sync' to push the specs to a Studio instance.

Authentication uses a bootstrap admin API key. Supply it via --api-key or
SITEGEN_API_KEY env var. Session-based admin auth requires TOTP and is not
scriptable.`,
}

var dashboardsSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync dashboard JSON specs to a Studio instance via the admin API",
	Long: `Read every .json file in the spec directory, look up the target
organization and BigQuery provider, and create or update each dashboard
definition via POST/PUT /api/admin/dashboard-definitions.

Every PUT creates a new entity version on the server side, so rollback is
available via the admin version/revert endpoints. A --dry-run mode prints
the plan without mutating anything.

Examples:
  # Local dev (uses SITEGEN_API_KEY env)
  taufinity --api-url http://localhost:8090 dashboards sync \
      --dir docs/dashboards/felix \
      --org-slug felix-works \
      --provider "Felix BigQuery"

  # Prod (explicit key flag)
  taufinity dashboards sync \
      --dir docs/dashboards/felix \
      --org-slug felix-works \
      --provider "Felix BigQuery" \
      --api-key "$BOOTSTRAP_KEY"

  # Dry run to preview
  taufinity dashboards sync --dir docs/dashboards/felix --org-slug felix-works --dry-run`,
	RunE: runDashboardsSync,
}

func init() {
	rootCmd.AddCommand(dashboardsCmd)
	dashboardsCmd.AddCommand(dashboardsSyncCmd)

	dashboardsSyncCmd.Flags().StringVar(&dashboardsDir, "dir", "", "Directory containing *.json spec files (required)")
	dashboardsSyncCmd.Flags().StringVar(&dashboardsOrgSlug, "org-slug", "felix-works", "Target organization slug")
	dashboardsSyncCmd.Flags().StringVar(&dashboardsProvider, "provider", "Felix BigQuery", "BigQuery provider name (system-wide, organization_id IS NULL)")
	dashboardsSyncCmd.Flags().StringVar(&dashboardsAPIKey, "api-key", "", "Bootstrap admin API key (overrides SITEGEN_API_KEY env var)")
	_ = dashboardsSyncCmd.MarkFlagRequired("dir")
}

// dashboardSpec is the JSON shape we expect in each spec file.
// Only the fields we read from disk are typed; everything else passes through.
type dashboardSpec map[string]any

// dashboardDef is the API response shape for a stored dashboard definition.
type dashboardDef struct {
	ID         uint   `json:"id"`
	Slug       string `json:"slug"`
	OrgID      *uint  `json:"org_id"`
	ProviderID uint   `json:"provider_id"`
}

type listDefinitionsResp struct {
	Definitions []dashboardDef `json:"definitions"`
}

type organization struct {
	ID   uint   `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type bqProvider struct {
	ID             uint   `json:"id"`
	Name           string `json:"name"`
	OrganizationID *uint  `json:"organization_id"`
}

type listProvidersResp struct {
	Providers []bqProvider `json:"providers"`
}

func runDashboardsSync(cmd *cobra.Command, args []string) error {
	// Resolve the API key
	key := dashboardsAPIKey
	if key == "" {
		key = os.Getenv("SITEGEN_API_KEY")
	}
	if key == "" {
		return fmt.Errorf("no API key: pass --api-key or set SITEGEN_API_KEY env var")
	}

	apiURL := strings.TrimRight(GetAPIURL(), "/")
	dryRun := IsDryRun()

	// Load spec files
	entries, err := filepath.Glob(filepath.Join(dashboardsDir, "*.json"))
	if err != nil {
		return fmt.Errorf("glob %s: %w", dashboardsDir, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no *.json spec files found in %s", dashboardsDir)
	}
	sort.Strings(entries)

	specs := make([]dashboardSpec, 0, len(entries))
	for _, path := range entries {
		spec, err := loadSpec(path)
		if err != nil {
			return fmt.Errorf("load %s: %w", path, err)
		}
		specs = append(specs, spec)
	}

	// Resolve target org
	orgID, err := resolveOrgID(apiURL, key, dashboardsOrgSlug)
	if err != nil {
		return fmt.Errorf("resolve org %q: %w", dashboardsOrgSlug, err)
	}
	// Resolve BQ provider (system-wide, organization_id IS NULL)
	providerID, err := resolveProviderID(apiURL, key, dashboardsProvider)
	if err != nil {
		return fmt.Errorf("resolve provider %q: %w", dashboardsProvider, err)
	}

	// Fetch existing definitions (scoped to this org)
	existingBySlug, err := fetchExistingDefs(apiURL, key)
	if err != nil {
		return fmt.Errorf("fetch existing definitions: %w", err)
	}

	if !IsQuiet() {
		fmt.Printf("Target: %s\n", apiURL)
		fmt.Printf("  Org:       %s (id=%d)\n", dashboardsOrgSlug, orgID)
		fmt.Printf("  Provider:  %s (id=%d)\n", dashboardsProvider, providerID)
		fmt.Printf("  Spec dir:  %s (%d files)\n", dashboardsDir, len(specs))
		if dryRun {
			fmt.Println("  Mode:      DRY RUN (no writes)")
		}
		fmt.Println()
	}

	// Apply
	var created, updated, failed int
	for _, spec := range specs {
		slug, _ := spec["slug"].(string)
		if slug == "" {
			fmt.Fprintf(os.Stderr, "SKIP: spec missing slug field\n")
			failed++
			continue
		}

		// Inject org_id + provider_id (always set, so specs stay portable)
		spec["org_id"] = orgID
		spec["provider_id"] = providerID

		existing, exists := existingBySlug[slug]
		if dryRun {
			if exists {
				fmt.Printf("  [dry-run] UPDATE %-45s (id=%d)\n", slug, existing.ID)
			} else {
				fmt.Printf("  [dry-run] CREATE %s\n", slug)
			}
			continue
		}

		if exists {
			if err := updateDefinition(apiURL, key, existing.ID, spec); err != nil {
				fmt.Fprintf(os.Stderr, "  UPDATE %-45s FAILED: %v\n", slug, err)
				failed++
			} else {
				fmt.Printf("  UPDATE %-45s ok (id=%d)\n", slug, existing.ID)
				updated++
			}
		} else {
			id, err := createDefinition(apiURL, key, spec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  CREATE %-45s FAILED: %v\n", slug, err)
				failed++
			} else {
				fmt.Printf("  CREATE %-45s ok (id=%d)\n", slug, id)
				created++
			}
		}
	}

	if !IsQuiet() {
		fmt.Println()
		fmt.Printf("Summary: %d created, %d updated, %d failed\n", created, updated, failed)
	}
	if failed > 0 {
		return fmt.Errorf("%d sync operation(s) failed", failed)
	}
	return nil
}

func loadSpec(path string) (dashboardSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var spec dashboardSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return spec, nil
}

// apiCall performs an HTTP call against the Studio admin API using the
// X-API-Key header. Returns the response body or an error.
func apiCall(method, url, apiKey string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-API-Key", apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, nil
}

func resolveOrgID(apiURL, key, slug string) (uint, error) {
	body, status, err := apiCall("GET", apiURL+"/api/admin/organizations", key, nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("GET /organizations returned %d: %s", status, string(body))
	}
	var orgs []organization
	if err := json.Unmarshal(body, &orgs); err != nil {
		return 0, fmt.Errorf("parse orgs JSON: %w", err)
	}
	for _, o := range orgs {
		if o.Slug == slug {
			return o.ID, nil
		}
	}
	return 0, fmt.Errorf("organization with slug %q not found", slug)
}

func resolveProviderID(apiURL, key, name string) (uint, error) {
	body, status, err := apiCall("GET", apiURL+"/api/admin/bq-providers", key, nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("GET /bq-providers returned %d: %s", status, string(body))
	}
	var resp listProvidersResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("parse providers JSON: %w", err)
	}
	for _, p := range resp.Providers {
		if p.Name == name && p.OrganizationID == nil {
			return p.ID, nil
		}
	}
	return 0, fmt.Errorf("system-wide BigQuery provider %q (organization_id IS NULL) not found", name)
}

func fetchExistingDefs(apiURL, key string) (map[string]dashboardDef, error) {
	body, status, err := apiCall("GET", apiURL+"/api/admin/dashboard-definitions", key, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /dashboard-definitions returned %d: %s", status, string(body))
	}
	var resp listDefinitionsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse definitions JSON: %w", err)
	}
	out := make(map[string]dashboardDef, len(resp.Definitions))
	for _, d := range resp.Definitions {
		out[d.Slug] = d
	}
	return out, nil
}

func createDefinition(apiURL, key string, spec dashboardSpec) (uint, error) {
	body, err := json.Marshal(spec)
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}
	respBody, status, err := apiCall("POST", apiURL+"/api/admin/dashboard-definitions", key, body)
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return 0, fmt.Errorf("POST returned %d: %s", status, string(respBody))
	}
	var def dashboardDef
	if err := json.Unmarshal(respBody, &def); err != nil {
		return 0, fmt.Errorf("parse response: %w", err)
	}
	return def.ID, nil
}

func updateDefinition(apiURL, key string, id uint, spec dashboardSpec) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	respBody, status, err := apiCall("PUT", fmt.Sprintf("%s/api/admin/dashboard-definitions/%d", apiURL, id), key, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("PUT returned %d: %s", status, string(respBody))
	}
	return nil
}
