// provision_prompts.go — provision support for org-scoped customer-tunable
// prompt templates.
//
// Backs the no-deploy prompt-edit path: editing
//
//	<customer>/studio/prompts/<name>.txt
//
// and running `taufinity provision apply` pushes the new body to the
// Studio prompt_templates row for that org. The change takes effect on
// next generation (within the loader's 60s cache TTL — see
// services.PromptLoader in ai-site-gen).
//
// Each .txt file becomes one row. Name = filename minus .txt — matches
// the convention used by site config's content_guidelines_path and by the
// existing templates/prompts/ directory on the server.
//
// Idempotent: the server-side handler uses ON CONFLICT (organization_id,
// name) DO UPDATE, so re-running with no content change is a no-op write
// (same body), and a body change is an in-place update.
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// promptUpsertResponse mirrors the JSON returned by PUT /api/organizations/{id}/prompts/{name}.
type promptUpsertResponse struct {
	ID             uint   `json:"id"`
	OrganizationID uint   `json:"organization_id"`
	Name           string `json:"name"`
	Body           string `json:"body"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// promptNameRe limits names to filename-shaped slugs. Matches the server's
// validation regex (api/handlers/prompts.go::promptNameRe) so we surface
// bad names with a clear local error instead of a 400 from the API.
var promptNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,254}$`)

// applyPrompts reads `<dir>/prompts/*.txt` and PUTs each to the org's
// prompt-templates endpoint. Silently skipped if the directory doesn't
// exist (matches dashboards/knowledge behavior — config is opt-in).
func applyPrompts(c *provisionClient, dir string, orgID uint) error {
	pd := filepath.Join(dir, "prompts")
	if !fileExists(pd) {
		return nil
	}

	entries, err := os.ReadDir(pd)
	if err != nil {
		return fmt.Errorf("read prompts dir %q: %w", pd, err)
	}

	// Deterministic order so dry-run output is comparable across runs.
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	if len(files) == 0 {
		return nil
	}

	var pushed, skipped int
	for _, file := range files {
		name := strings.TrimSuffix(file, ".txt")
		if !promptNameRe.MatchString(name) {
			c.Warn("prompt file %q produces invalid name %q (must match %s); skipping",
				file, name, promptNameRe.String())
			skipped++
			continue
		}

		body, err := os.ReadFile(filepath.Join(pd, file))
		if err != nil {
			return fmt.Errorf("read prompt %q: %w", file, err)
		}
		if len(body) == 0 {
			c.Warn("prompt file %q is empty; skipping", file)
			skipped++
			continue
		}

		payload := struct {
			Body string `json:"body"`
		}{Body: string(body)}
		payloadBytes, _ := json.Marshal(payload)

		path := fmt.Sprintf("/organizations/%d/prompts/%s", orgID, name)
		respBody, status, err := c.put(path, payloadBytes)
		if err != nil || status >= 300 {
			return fmt.Errorf("prompt upsert %q: status=%d err=%v body=%s",
				name, status, err, provisionSummarize(respBody))
		}
		if c.dryRun {
			fmt.Printf("[dry-run] prompt upsert %q (%d bytes)\n", name, len(body))
			continue
		}
		var resp promptUpsertResponse
		if err := json.Unmarshal(respBody, &resp); err != nil {
			// Non-fatal — the upsert succeeded, we just can't parse the
			// confirmation. Surface and continue.
			c.Warn("prompt %q: parse response: %v", name, err)
			pushed++
			continue
		}
		fmt.Printf("UPSERT prompt %q id=%d (%d bytes)\n", resp.Name, resp.ID, len(body))
		pushed++
	}

	fmt.Printf("provision: prompts summary: pushed=%d skipped=%d\n", pushed, skipped)
	return nil
}
