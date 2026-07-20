// provision_dashboards.go — the dashboard spec model shared by apply and pull,
// plus the field-level diff that lets apply skip dashboards that did not change.
//
// Why a diff at all: a dashboard PUT replaces the whole row and writes a new
// entity version. Without a diff, every apply rewrites every dashboard, which
// (a) buries real changes in a wall of version noise and (b) silently reverts
// anything an operator changed in the Studio UI since the spec was last pulled.
// diffFields gives apply a NOOP path: only dashboards whose fields actually
// differ from the server are written.
//
// The wire format needs care. The list endpoint returns a trimmed record, so a
// per-dashboard detail GET is required for an accurate diff. The detail endpoint
// returns columns/filters/default_sort/layout/client_group_filter as
// JSON-encoded *strings* rather than raw objects, so those are unwrapped back to
// canonical JSON before comparing against the local file. Static filters have
// two names — "source_filter" (object, on disk) and "static_filters" (string,
// on the wire) — and are normalized through normalizedStaticFilters().
package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// provisionDashboardDef is the union of the on-disk spec shape and the detail
// wire shape. Named provisionDashboardDef to avoid colliding with the minimal
// dashboardDef in dashboards.go, which models only the list response.
type provisionDashboardDef struct {
	ID                 uint            `json:"id,omitempty"`
	Slug               string          `json:"slug"`
	Name               string          `json:"name"`
	Description        string          `json:"description,omitempty"`
	SourceView         string          `json:"source_view"`

	// Provider selects a non-primary provider by its slug (see
	// providerConfig.Slug), for a directory with more than one BQ provider
	// under providers/ (e.g. two datasets in different projects). Empty means
	// the primary provider — the root provider.yaml — same as before this
	// field existed; every dashboard spec without it behaves unchanged.
	Provider string `json:"provider,omitempty"`
	Columns            json.RawMessage `json:"columns"`
	Filters            json.RawMessage `json:"filters,omitempty"`
	DefaultChart       string          `json:"default_chart,omitempty"`
	DefaultSort        json.RawMessage `json:"default_sort,omitempty"`
	Layout             json.RawMessage `json:"layout,omitempty"`
	MaxRows            int             `json:"max_rows,omitempty"`
	Position           int             `json:"position,omitempty"`
	HiddenFromOverview bool            `json:"hidden_from_overview,omitempty"`
	ExportEnabled      bool            `json:"export_enabled,omitempty"`

	// ClientGroupFilter declares which column scopes data for portal
	// (client-group) users, e.g. {"column":"client_name"}. Local specs carry it
	// as an object; the API returns it as a JSON-encoded string.
	ClientGroupFilter json.RawMessage `json:"client_group_filter,omitempty"`

	// Static filters arrive under two names:
	//   local file: "source_filter": {...}   (object)
	//   remote API: "static_filters": "{}"   (JSON-encoded string)
	// Use normalizedStaticFilters() for the canonical form.
	StaticFilterFile   json.RawMessage `json:"source_filter,omitempty"`
	StaticFilterRemote string          `json:"static_filters,omitempty"`

	// Breadcrumb is display-only. Apply does not push or compare it, but pull
	// preserves it so a snapshot never silently drops a field the server holds.
	Breadcrumb string `json:"breadcrumb,omitempty"`
}

// normalizedStaticFilters returns the canonical form, treating "" and "{}" as
// equivalent to absent.
func (d provisionDashboardDef) normalizedStaticFilters() json.RawMessage {
	if len(d.StaticFilterFile) > 0 {
		if s := string(d.StaticFilterFile); s != "{}" && s != "null" {
			return d.StaticFilterFile
		}
	}
	if d.StaticFilterRemote != "" && d.StaticFilterRemote != "{}" {
		return json.RawMessage(d.StaticFilterRemote)
	}
	return nil
}

// diffFields lists the field names that differ between the local spec and the
// server record. An empty result means semantic equivalence, and apply NOOPs.
//
// The coercions below mirror the API's own defaults so that a field the spec
// omits does not read as a difference against the default the server filled in:
//   - default_chart: local "" equals remote "table"
//   - max_rows: local 0 equals remote 5000
func diffFields(local, remote provisionDashboardDef) []string {
	var diffs []string
	if local.Name != remote.Name {
		diffs = append(diffs, "name")
	}
	if local.Description != remote.Description {
		diffs = append(diffs, "description")
	}
	if local.SourceView != remote.SourceView {
		diffs = append(diffs, "source_view")
	}
	if local.DefaultChart != remote.DefaultChart && !(local.DefaultChart == "" && remote.DefaultChart == "table") {
		diffs = append(diffs, "default_chart")
	}
	if local.MaxRows != remote.MaxRows && !(local.MaxRows == 0 && remote.MaxRows == 5000) {
		diffs = append(diffs, "max_rows")
	}
	if local.Position != remote.Position {
		diffs = append(diffs, "position")
	}
	if local.HiddenFromOverview != remote.HiddenFromOverview {
		diffs = append(diffs, "hidden_from_overview")
	}
	if local.ExportEnabled != remote.ExportEnabled {
		diffs = append(diffs, "export_enabled")
	}
	if !rawEqual(local.Columns, remote.Columns) {
		diffs = append(diffs, "columns")
	}
	if !rawEqual(local.Filters, remote.Filters) {
		diffs = append(diffs, "filters")
	}
	if !rawEqual(local.DefaultSort, remote.DefaultSort) {
		diffs = append(diffs, "default_sort")
	}
	if !rawEqual(local.Layout, remote.Layout) {
		diffs = append(diffs, "layout")
	}
	if !rawEqual(local.normalizedStaticFilters(), remote.normalizedStaticFilters()) {
		diffs = append(diffs, "static_filters")
	}
	if !rawEqual(local.ClientGroupFilter, remote.ClientGroupFilter) {
		diffs = append(diffs, "client_group_filter")
	}
	return diffs
}

// rawEqual compares two RawMessages semantically: both are re-marshalled so
// key order and whitespace do not create phantom diffs. Empty and null are
// treated as the same absent value.
func rawEqual(a, b json.RawMessage) bool {
	aEmpty := len(bytes.TrimSpace(a)) == 0 || string(bytes.TrimSpace(a)) == "null"
	bEmpty := len(bytes.TrimSpace(b)) == 0 || string(bytes.TrimSpace(b)) == "null"
	if aEmpty && bEmpty {
		return true
	}
	if aEmpty != bEmpty {
		return false
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return bytes.Equal(ab, bb)
}

// decodeRemoteStringFields converts JSON-string-encoded fields returned by the
// detail endpoint back to canonical raw JSON so they compare like-for-like
// against the local file.
func decodeRemoteStringFields(d *provisionDashboardDef) {
	d.Columns = decodeRemoteString(d.Columns)
	d.Filters = decodeRemoteString(d.Filters)
	d.DefaultSort = decodeRemoteString(d.DefaultSort)
	d.Layout = decodeRemoteString(d.Layout)
	d.ClientGroupFilter = decodeRemoteString(d.ClientGroupFilter)
}

// decodeRemoteString unwraps a JSON-encoded string back to its raw JSON content
// (`"[{\"k\":1}]"` becomes `[{"k":1}]`). Values that are already objects/arrays,
// or strings whose content is not itself JSON, are returned untouched — that
// guards against manufacturing an invalid RawMessage that would break rawEqual.
func decodeRemoteString(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return raw
	}
	if s == "" {
		return nil
	}
	var probe any
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		return raw
	}
	return json.RawMessage(s)
}

// rawStrOrNull emits raw JSON for the fields the admin API accepts as raw JSON
// (columns, filters, default_sort, layout, static_filters, client_group_filter).
// Stringifying them would make the server try to decode a quoted JSON string
// into a typed struct, which fails.
func rawStrOrNull(r json.RawMessage) any {
	if len(r) == 0 || string(r) == "null" {
		return nil
	}
	return r
}

// ─── List + detail ───────────────────────────────────────────────────────────

// tryUnmarshalDashboardList accepts the three list-envelope shapes the API uses:
// {"definitions": [...]}, {"data": [...]}, or a bare array. The envelope is
// detected by peeking at the top-level keys rather than by trial unmarshal, so
// an empty `{}` (org with no dashboards) does not fall through to the bare-array
// branch and fail.
func tryUnmarshalDashboardList(body []byte, out *[]provisionDashboardDef) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return json.Unmarshal(body, out)
	}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			return err
		}
		if raw, ok := top["definitions"]; ok {
			return json.Unmarshal(raw, out)
		}
		if raw, ok := top["data"]; ok {
			return json.Unmarshal(raw, out)
		}
		*out = nil
		return nil
	}
	return fmt.Errorf("unexpected response shape: %s", provisionSummarize(body))
}

// listDashboardDefs fetches every dashboard definition visible to the org.
func listDashboardDefs(c *provisionClient, orgID uint) ([]provisionDashboardDef, error) {
	body, status, err := c.getForOrg("/admin/dashboard-definitions", orgID)
	if err != nil || status != 200 {
		return nil, provisionAPIErr("list dashboards", status, body, err)
	}
	var existing []provisionDashboardDef
	if err := tryUnmarshalDashboardList(body, &existing); err != nil {
		return nil, fmt.Errorf("parse dashboards: %w (body=%s)", err, provisionSummarize(body))
	}
	return existing, nil
}

// getDashboardDetail fetches the full record for one dashboard. The list
// endpoint omits layout, static filters and the full columns shape, so an
// accurate diff needs the detail GET.
func getDashboardDetail(c *provisionClient, orgID, id uint) (*provisionDashboardDef, error) {
	body, status, err := c.getForOrg(fmt.Sprintf("/admin/dashboard-definitions/%d", id), orgID)
	if err != nil || status != 200 {
		return nil, provisionAPIErr(fmt.Sprintf("get dashboard %d", id), status, body, err)
	}
	// Peek at the envelope rather than trying a bare unmarshal first: a bare
	// unmarshal against a wrapped payload succeeds and yields an empty struct,
	// which would read as "every field changed".
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("parse dashboard %d: %w (body=%s)", id, err, provisionSummarize(body))
	}
	var raw json.RawMessage
	if dataRaw, hasData := top["data"]; hasData {
		raw = dataRaw
	} else if _, hasSlug := top["slug"]; hasSlug {
		raw = body
	} else {
		return nil, fmt.Errorf("dashboard %d response had an unrecognized envelope (body=%s)",
			id, provisionSummarize(body))
	}
	var d provisionDashboardDef
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse dashboard %d: %w (body=%s)", id, err, provisionSummarize(body))
	}
	if d.Slug == "" {
		return nil, fmt.Errorf("dashboard %d response had an empty slug (body=%s)", id, provisionSummarize(body))
	}
	decodeRemoteStringFields(&d)
	return &d, nil
}

// ─── Pull ────────────────────────────────────────────────────────────────────

// provisionDashboardFileShape is the canonical on-disk JSON shape. Field order
// mirrors what apply reads from dashboards/*.json, so a pull → apply round-trip
// is a NOOP and git diffs stay small.
type provisionDashboardFileShape struct {
	Slug               string          `json:"slug"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	SourceView         string          `json:"source_view"`
	Columns            json.RawMessage `json:"columns"`
	Filters            json.RawMessage `json:"filters,omitempty"`
	DefaultChart       string          `json:"default_chart,omitempty"`
	DefaultSort        json.RawMessage `json:"default_sort,omitempty"`
	HiddenFromOverview bool            `json:"hidden_from_overview,omitempty"`
	ExportEnabled      bool            `json:"export_enabled,omitempty"`
	MaxRows            int             `json:"max_rows,omitempty"`
	Position           int             `json:"position,omitempty"`
	Layout             json.RawMessage `json:"layout,omitempty"`
	// Static filters are written back under the file-form key.
	StaticFilters     json.RawMessage `json:"source_filter,omitempty"`
	ClientGroupFilter json.RawMessage `json:"client_group_filter,omitempty"`
	Breadcrumb        string          `json:"breadcrumb,omitempty"`
}

func (d *provisionDashboardDef) toFileShape() provisionDashboardFileShape {
	return provisionDashboardFileShape{
		Slug:               d.Slug,
		Name:               d.Name,
		Description:        d.Description,
		SourceView:         d.SourceView,
		Columns:            d.Columns,
		Filters:            d.Filters,
		DefaultChart:       d.DefaultChart,
		DefaultSort:        d.DefaultSort,
		HiddenFromOverview: d.HiddenFromOverview,
		ExportEnabled:      d.ExportEnabled,
		MaxRows:            d.MaxRows,
		Position:           d.Position,
		Layout:             d.Layout,
		StaticFilters:      d.normalizedStaticFilters(),
		ClientGroupFilter:  d.ClientGroupFilter,
		Breadcrumb:         d.Breadcrumb,
	}
}

// localDashboardFiles maps slug → file path for every dashboard spec in dir.
// Underscore-prefixed files are metadata, not specs, and are skipped.
// Subdirectories are not recursed, so archived specs there stay out of scope.
func localDashboardFiles(dir string) (map[string]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(matches))
	for _, m := range matches {
		if strings.HasPrefix(filepath.Base(m), "_") {
			continue
		}
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", m, err)
		}
		var d struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("parse %s: %w", m, err)
		}
		if d.Slug != "" {
			out[d.Slug] = m
		}
	}
	return out, nil
}

// writeDashboardFile writes the file shape as indented JSON with a trailing
// newline, matching the existing spec formatting.
func writeDashboardFile(path string, d provisionDashboardFileShape) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// pullProvisionDashboards refreshes the local dashboard specs in dir from the
// server.
//
// Only slugs that already have a local file are refreshed: pull captures
// server-side edits back into tracked specs, it does not mirror every dashboard
// onto disk. A dashboard that exists server-side but is untracked locally is
// reported and skipped. After a pull, `provision diff` reports NOOP for the
// refreshed dashboards.
func pullProvisionDashboards(c *provisionClient, orgID uint, dir string, dryRun bool) error {
	existing, err := listDashboardDefs(c, orgID)
	if err != nil {
		return err
	}

	localBySlug, err := localDashboardFiles(dir)
	if err != nil {
		return err
	}

	pulled, skipped := 0, 0
	for _, d := range existing {
		path, tracked := localBySlug[d.Slug]
		if !tracked {
			fmt.Printf("SKIP   %s (on the server, no local file)\n", d.Slug)
			skipped++
			continue
		}
		detail, err := getDashboardDetail(c, orgID, d.ID)
		if err != nil {
			return fmt.Errorf("get detail %q: %w", d.Slug, err)
		}
		if dryRun {
			fmt.Printf("WOULD PULL %s id=%d → %s\n", d.Slug, d.ID, path)
			pulled++
			continue
		}
		if err := writeDashboardFile(path, detail.toFileShape()); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("PULL   %s id=%d → %s\n", d.Slug, d.ID, path)
		pulled++
	}

	verb := "pulled"
	if dryRun {
		verb = "would pull"
	}
	fmt.Printf("provision: dashboards pull summary: %s=%d skipped=%d (on the server, not tracked locally)\n",
		verb, pulled, skipped)
	return nil
}
