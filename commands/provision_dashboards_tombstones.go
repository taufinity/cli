// provision_dashboards_tombstones.go — the only way provision deletes a dashboard.
//
// Deletion is opt-in and explicit. A dashboard is removed only when its slug is
// named in dashboards/_tombstones.json, together with a reason:
//
//	{"slugs": [{"slug": "orders-weekly", "reason": "replaced by orders-daily"}]}
//
// The obvious alternative — "a slug on the server with no local file means
// delete" — is rejected on purpose. Under that rule any stale, partial or
// wrongly-pointed local directory silently destroys live dashboards, and the
// failure looks exactly like a successful run. Requiring the operator to name
// the slug and write down why turns deletion into a reviewable line in a diff.
//
// The cost of this design is that a removed spec file leaves an orphan row on
// the server until someone tombstones it. That is the trade we want: an orphan
// is visible and recoverable, a silent delete is neither.
package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

// tombstonesFileName is the reserved spec file listing slugs to delete. The
// leading underscore keeps it out of the *.json upsert loop, which skips any
// file whose name starts with "_".
const tombstonesFileName = "_tombstones.json"

type dashboardTombstones struct {
	Slugs []dashboardTombstone `json:"slugs"`
}

type dashboardTombstone struct {
	Slug   string `json:"slug"`
	Reason string `json:"reason"`
}

// readDashboardTombstones loads dashboards/_tombstones.json. An absent file is
// not an error — most customers never delete a dashboard, so no tombstones is
// the normal case, not a missing-config case.
func readDashboardTombstones(dashDir string) ([]dashboardTombstone, error) {
	path := filepath.Join(dashDir, tombstonesFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var ts dashboardTombstones
	if err := json.Unmarshal(raw, &ts); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for i, t := range ts.Slugs {
		if t.Slug == "" {
			return nil, fmt.Errorf("%s: entry %d has an empty slug", path, i)
		}
		if t.Reason == "" {
			return nil, fmt.Errorf("%s: tombstone %q has no reason — a delete must say why", path, t.Slug)
		}
	}
	return ts.Slugs, nil
}

// applyDashboardTombstones deletes every tombstoned slug that still exists on
// the server, and removes it from bySlug so the caller's upsert loop sees a
// clean slate. A slug that is already gone is a no-op: tombstones stay in the
// spec after they have been applied (they are the record of the deletion), so
// re-running provision must not turn a completed delete into an error.
//
// Every delete is printed with its reason, before it happens. A destructive
// action that scrolls past silently is one nobody reviews.
//
// Deliberately not gated by --strict. Strict exists to stop provision from
// steamrolling a change someone made in the UI that nobody pulled back into the
// spec — that is drift, and drift means "the local file may be the stale one".
// A tombstone is the opposite: a named slug with a written reason, committed on
// purpose. There is no ambiguity about intent to protect the operator from.
// Creating a brand-new dashboard is skipped by strict for the same reason.
//
// In --dry-run the client prints the DELETE and issues no request, so this is
// safe to run against production to preview.
func applyDashboardTombstones(c *provisionClient, dashDir string, bySlug map[string]provisionDashboardDef) (int, error) {
	tombstones, err := readDashboardTombstones(dashDir)
	if err != nil {
		return 0, err
	}

	var deleted int
	for _, t := range tombstones {
		cur, exists := bySlug[t.Slug]
		if !exists {
			fmt.Printf("DELETE %s — SKIPPED, not on the server (tombstone already applied). Reason on file: %s\n", t.Slug, t.Reason)
			continue
		}

		fmt.Printf("DELETE %s id=%d — reason: %s\n", t.Slug, cur.ID, t.Reason)

		body, status, err := c.write(http.MethodDelete, fmt.Sprintf("/admin/dashboard-definitions/%d", cur.ID), nil)
		// 404 means someone else got there first, which is the outcome we wanted.
		if err != nil || (status >= 300 && status != http.StatusNotFound) {
			return deleted, provisionAPIErr(fmt.Sprintf("delete dashboard %q", t.Slug), status, body, err)
		}
		delete(bySlug, t.Slug)
		deleted++
	}
	return deleted, nil
}
