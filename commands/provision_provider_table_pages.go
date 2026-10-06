package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

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
// agent reading "no table page" for a table the spec claims has one.
func resolveTablePages(repoRoot string, cfg *providerConfig, dryRun bool) error {
	if len(cfg.TablePages) == 0 {
		return nil
	}
	contents := make(map[string]string, len(cfg.TablePages))
	for table, path := range cfg.TablePages {
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
		contents[table] = string(raw)
	}
	cfg.tablePageContents = contents
	return nil
}

// diffTablePages prints what a provision run would change about a provider's
// table pages, comparing the resolved contents with the live provider. Best
// effort: a read failure (provider not created yet, endpoint down) prints a
// note and reports every page as new rather than failing the run.
func diffTablePages(c *provisionClient, providerID int, contents map[string]string) {
	if len(contents) == 0 {
		return
	}
	live := map[string]string{}
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
			fmt.Printf("  table_pages.%s: NEW (%s)\n", table, pageFingerprint(content))
		case old == content:
			fmt.Printf("  table_pages.%s: unchanged\n", table)
		default:
			fmt.Printf("  table_pages.%s: CHANGED %s → %s\n", table, pageFingerprint(old), pageFingerprint(content))
		}
	}
	for table := range live {
		if _, ok := contents[table]; !ok {
			fmt.Printf("  table_pages.%s: REMOVED from spec (stays live until cleared)\n", table)
		}
	}
}

// pageFingerprint identifies page content in diffs without printing the whole
// page: size plus the first 8 hex chars of its sha256.
func pageFingerprint(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%d chars, %s", len(content), hex.EncodeToString(sum[:8]))
}
