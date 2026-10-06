package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// tablePageForbiddenPrefix mirrors the server's boundary (Studio's
// database.IsTablePageEligible): internal layers never get a table page. The
// server re-validates on the PUT, so a drift between the two fails loudly
// rather than provisioning dead content.
var tablePageForbiddenPrefix = regexp.MustCompile(`^(raw_|stg_|core_|int_|base_|snap_)`)

// maxTablePageBytes mirrors the server's limit (32 KB): a page rides directly
// into the model context.
const maxTablePageBytes = 32 << 10

type resolvedTablePage struct {
	Markdown  string `json:"markdown"`
	SHA256    string `json:"sha256"`
	Source    string `json:"source"`
	UpdatedAt string `json:"updated_at"`
}

// Table pages (ADR-016 rule 8): the providerspec carries, per allow-listed
// table, the path of its full table page. Provision reads the files and
// uploads the CONTENT with the provider, so pages are provisioned like every
// other Studio configuration — with dry-run and diff — and never hand-edited
// in the Studio interface.

// resolveTablePages reads every table_pages entry into cfg.tablePageContents.
// Paths resolve against repoRoot (the parent of the --dir studio folder); a
// ../ path into a sibling repo is allowed, so a page can live where dbt's
// meta.table_page points. A missing file is a hard error on apply and a
// warning on dry-run, because uploading a partial set silently would leave an
// agent reading "no table page" for a table the spec claims has one. The
// same boundaries the server enforces are checked here, so a run fails at
// provision time rather than at the PUT: internal layers (raw_, stg_, core_,
// int_, base_, snap_) never get a page, and one page is at most 32 KB (it rides
// directly into the model context).
func resolveTablePages(repoRoot string, cfg *providerConfig, dryRun bool) error {
	if len(cfg.TablePages) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(cfg.AllowedTables))
	for _, table := range cfg.AllowedTables {
		allowed[table] = true
	}
	if parts := strings.SplitN(cfg.EndpointURL, ".", 2); len(parts) == 2 && isRawDataset(parts[1]) {
		return fmt.Errorf("table pages cannot be provisioned for raw dataset %s", parts[1])
	}
	contents := make(map[string]resolvedTablePage, len(cfg.TablePages))
	for table, path := range cfg.TablePages {
		if !allowed[table] {
			return fmt.Errorf("table page for %s: table is not in allowed_tables", table)
		}
		if tablePageForbiddenPrefix.MatchString(strings.ToLower(table)) {
			return fmt.Errorf("table page for %s: internal-layer tables never get a table page", table)
		}
		full := path
		if !filepath.IsAbs(full) {
			full = filepath.Join(repoRoot, full)
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			if dryRun {
				fmt.Printf("  WARN: table page for %s not readable (%s): %v\n", table, full, err)
				continue
			}
			return fmt.Errorf("table page for %s: %w", table, err)
		}
		if len(raw) > maxTablePageBytes {
			return fmt.Errorf("table page for %s is %d bytes; the limit is %d", table, len(raw), maxTablePageBytes)
		}
		if len(raw) == 0 {
			return fmt.Errorf("table page for %s is empty", table)
		}
		source, updatedAt, err := committedPageSource(full, dryRun)
		if err != nil {
			return fmt.Errorf("table page for %s: %w", table, err)
		}
		sum := sha256.Sum256(raw)
		contents[table] = resolvedTablePage{
			Markdown: string(raw), UpdatedAt: updatedAt, Source: source,
			SHA256: hex.EncodeToString(sum[:]),
		}
	}
	cfg.tablePageContents = contents
	return nil
}

// diffTablePages prints what a provision run would change about a provider's
// table pages, comparing the resolved contents with the live provider. Best
// effort: a read failure (provider not created yet, endpoint down) prints a
// note and reports every page as new rather than failing the run.
func diffTablePages(c *provisionClient, providerID int, contents map[string]resolvedTablePage) {
	if len(contents) == 0 {
		return
	}
	live := map[string]resolvedTablePage{}
	body, status, err := c.get(fmt.Sprintf("/admin/bq-providers/%d", providerID))
	if err == nil && status == 200 {
		var raw struct {
			TablePages string `json:"table_pages"`
		}
		if json.Unmarshal(body, &raw) == nil && raw.TablePages != "" {
			_ = json.Unmarshal([]byte(raw.TablePages), &live)
		}
	} else if !c.dryRun {
		fmt.Printf("  note: could not read current table pages (status=%d): diff shows every page as new\n", status)
	}
	for table, content := range contents {
		switch old, ok := live[table]; {
		case !ok:
			fmt.Printf("  table_pages.%s: NEW (%s)\n", table, pageFingerprint(content.Markdown))
		case old.SHA256 == content.SHA256 && old.Source == content.Source:
			fmt.Printf("  table_pages.%s: unchanged\n", table)
		default:
			fmt.Printf("  table_pages.%s: CHANGED %s → %s\n", table, pageFingerprint(old.Markdown), pageFingerprint(content.Markdown))
		}
	}
	for table := range live {
		if _, ok := contents[table]; !ok {
			fmt.Printf("  table_pages.%s: REMOVED\n", table)
		}
	}
}

// writeBQProviderBoundary persists the two fields that jointly define a BQ
// provider's readable surface. Sending an explicit empty object removes pages
// deleted from the providerspec instead of leaving stale content live.
func writeBQProviderBoundary(c *provisionClient, providerID int, allowedJSON []byte, contents map[string]resolvedTablePage) error {
	if contents == nil {
		contents = map[string]resolvedTablePage{}
	}
	pagesJSON, err := json.Marshal(contents)
	if err != nil {
		return fmt.Errorf("marshal table_pages: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"allowed_tables": string(allowedJSON),
		"table_pages":    string(pagesJSON),
	})
	if err != nil {
		return fmt.Errorf("marshal BQ provider boundary: %w", err)
	}
	_, status, err := c.put(fmt.Sprintf("/admin/bq-providers/%d", providerID), payload)
	if err != nil || status >= 300 {
		return fmt.Errorf("update BQ allowed_tables/table_pages: status=%d err=%v", status, err)
	}
	return nil
}

func committedPageSource(path string, dryRun bool) (source, updatedAt string, err error) {
	dir := filepath.Dir(path)
	repoRaw, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		if dryRun {
			fmt.Printf("  WARN: table page is not inside a git repository: %s\n", path)
			return "dry-run:uncommitted:" + filepath.ToSlash(path), "dry-run", nil
		}
		return "", "", fmt.Errorf("source is not inside a git repository")
	}
	repoRoot := strings.TrimSpace(string(repoRaw))
	// Both sides through EvalSymlinks before the Rel: on macOS the temp dir
	// (and /tmp) is a symlink (/var → /private/var), git reports the real
	// path, and a naive Rel then reads "outside its git repository" for a
	// file that is inside it.
	if realPath, perr := filepath.EvalSymlinks(path); perr == nil {
		path = realPath
	}
	if realRepo, rerr := filepath.EvalSymlinks(repoRoot); rerr == nil {
		repoRoot = realRepo
	}
	rel, err := filepath.Rel(repoRoot, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", fmt.Errorf("source path is outside its git repository")
	}
	rel = filepath.ToSlash(rel)
	status, err := exec.Command("git", "-C", repoRoot, "status", "--porcelain", "--", rel).Output()
	if err != nil {
		return "", "", fmt.Errorf("inspect source git status: %w", err)
	}
	if len(status) > 0 {
		if !dryRun {
			return "", "", fmt.Errorf("source has uncommitted changes: %s", rel)
		}
		fmt.Printf("  WARN: table page source has uncommitted changes: %s\n", rel)
	}
	shaRaw, err := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", "", fmt.Errorf("read source commit: %w", err)
	}
	dateRaw, err := exec.Command("git", "-C", repoRoot, "log", "-1", "--format=%cI", "--", rel).Output()
	if err != nil || strings.TrimSpace(string(dateRaw)) == "" {
		return "", "", fmt.Errorf("source is not committed: %s", rel)
	}
	return fmt.Sprintf("%s@%s:%s", filepath.Base(repoRoot), strings.TrimSpace(string(shaRaw)), rel),
		strings.TrimSpace(string(dateRaw)), nil
}

func isRawDataset(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "raw" || name == "raw_data" || strings.HasPrefix(name, "raw_") ||
		strings.HasSuffix(name, "_raw") || strings.Contains(name, "_raw_")
}

// pageFingerprint identifies page content in diffs without printing the whole
// page: size plus the first 8 hex chars of its sha256.
func pageFingerprint(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%d chars, %s", len(content), hex.EncodeToString(sum[:4]))
}
