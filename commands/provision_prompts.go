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
	// ETag mirrors the response header; the server carries it in the payload
	// because the shared transport does not surface response headers.
	// Empty against an older server, which just means the write is
	// unconditional — the same behaviour as before If-Match existed.
	ETag string `json:"etag"`
}

// promptNameRe limits names to filename-shaped slugs. Matches the server's
// validation regex (api/handlers/prompts.go::promptNameRe) so we surface
// bad names with a clear local error instead of a 400 from the API.
var promptNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,254}$`)

// sitePromptSuffix builds the site-scoped prompt name for a site.
//
// prompt_templates is keyed (organization_id, name) with no site column, so
// site scope is expressed in the name. This is not a new convention: the
// server already resolves "topic_generation__<site_id>" ahead of the plain
// org-level row (ai-site-gen internal/content/topics_prompt.go). Reusing it
// means site-level prompts need no migration and no server change.
//
// Two sites in one org genuinely need different instructions — quizreps_com
// and afvallenvoordummies_nl both sit under alleznet-media, and quiz-diversity
// rules make sense for one and not the other — which is why an org-level
// override is not sufficient.
func sitePromptSuffix(name, siteID string) string {
	return name + "__" + siteID
}

// applySitePrompts reads `<siteDir>/prompts/*.txt` and pushes each as a
// site-scoped override. Same body, validation and idempotency as the
// org-level path; only the stored name differs.
func applySitePrompts(c *provisionClient, siteDir string, orgID uint, siteID string) error {
	return applyPromptsFrom(c, filepath.Join(siteDir, "prompts"), orgID, siteID)
}

// applyPrompts reads `<dir>/prompts/*.txt` and PUTs each to the org's
// prompt-templates endpoint. Silently skipped if the directory doesn't
// exist (matches dashboards/knowledge behavior — config is opt-in).
func applyPrompts(c *provisionClient, dir string, orgID uint) error {
	return applyPromptsFrom(c, filepath.Join(dir, "prompts"), orgID, "")
}

// applyPromptsFrom is the shared body. siteScope is "" for org-level prompts,
// or the site_id for site-scoped ones.
func applyPromptsFrom(c *provisionClient, pd string, orgID uint, siteScope string) error {
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

	var pushed, skipped, unchanged int
	for _, file := range files {
		name := strings.TrimSuffix(file, ".txt")
		if siteScope != "" {
			name = sitePromptSuffix(name, siteScope)
		}
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

		path := fmt.Sprintf("/organizations/%d/prompts/%s", orgID, name)

		// Read the live body first so we can report what changes and make the
		// write conditional. A prompt is pushed wholesale and a bad body
		// degrades generations silently, so "what am I about to overwrite" is
		// worth one GET.
		current, etag, found := fetchPromptBody(c, path)
		if found && current == string(body) {
			fmt.Printf("UNCHANGED prompt %q\n", name)
			unchanged++
			continue
		}
		if found {
			printPromptDiff(name, current, string(body))
		} else {
			fmt.Printf("CREATE prompt %q (%d bytes)\n", name, len(body))
		}

		payload := struct {
			Body string `json:"body"`
		}{Body: string(body)}
		payloadBytes, _ := json.Marshal(payload)

		// If-Match makes a concurrent edit between the GET above and this PUT
		// fail with 412 instead of silently winning. The server treats an
		// absent header as unconditional, so this is additive.
		var headers map[string]string
		if found && etag != "" {
			headers = map[string]string{"If-Match": etag}
		}
		respBody, status, err := c.writeWithHeaders("PUT", path, payloadBytes, headers)
		if status == 412 {
			return fmt.Errorf("prompt %q changed since it was read — re-run to pick up the new body "+
				"(someone else edited it, or a previous apply is still in flight)", name)
		}
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

	scope := "org"
	if siteScope != "" {
		scope = "site " + siteScope
	}
	fmt.Printf("provision: prompts summary (%s): pushed=%d unchanged=%d skipped=%d\n",
		scope, pushed, unchanged, skipped)
	return nil
}

// fetchPromptBody returns the live body and its ETag. found=false means no
// override exists yet (the server falls back to the file template), which is a
// create rather than an error.
func fetchPromptBody(c *provisionClient, path string) (body, etag string, found bool) {
	respBody, status, err := c.get(path)
	if err != nil || status == 404 {
		return "", "", false
	}
	if status >= 300 {
		// Non-fatal: fall through to an unconditional write rather than
		// blocking provisioning on a read failure.
		c.Warn("prompt read %s: status=%d — writing unconditionally", path, status)
		return "", "", false
	}
	var resp promptUpsertResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		c.Warn("prompt read %s: parse: %v — writing unconditionally", path, err)
		return "", "", false
	}
	// ETag comes from the JSON body, not the header: the shared transport
	// returns status and bytes only, and the server mirrors the header value
	// into the payload for exactly this reason.
	return resp.Body, resp.ETag, true
}

// printPromptDiff shows a compact line-level summary of what the push changes.
// Full unified diff would bury the signal — these bodies run to hundreds of
// lines and usually a handful change.
func printPromptDiff(name, oldBody, newBody string) {
	oldLines := strings.Split(oldBody, "\n")
	newLines := strings.Split(newBody, "\n")

	inOld := map[string]bool{}
	for _, l := range oldLines {
		inOld[l] = true
	}
	inNew := map[string]bool{}
	for _, l := range newLines {
		inNew[l] = true
	}

	var added, removed int
	for _, l := range newLines {
		if strings.TrimSpace(l) != "" && !inOld[l] {
			added++
		}
	}
	for _, l := range oldLines {
		if strings.TrimSpace(l) != "" && !inNew[l] {
			removed++
		}
	}

	fmt.Printf("UPDATE prompt %q: %d lines added, %d removed (%d → %d bytes)\n",
		name, added, removed, len(oldBody), len(newBody))

	shown := 0
	for _, l := range newLines {
		if shown >= 3 {
			break
		}
		if strings.TrimSpace(l) != "" && !inOld[l] {
			fmt.Printf("    + %s\n", truncateLine(l, 100))
			shown++
		}
	}
	for _, l := range oldLines {
		if shown >= 6 {
			break
		}
		if strings.TrimSpace(l) != "" && !inNew[l] {
			fmt.Printf("    - %s\n", truncateLine(l, 100))
			shown++
		}
	}
}

func truncateLine(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
