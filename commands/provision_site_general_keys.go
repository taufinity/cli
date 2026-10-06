// provision_site_general_keys.go: the site.yaml keys that Studio keeps in the
// general-settings section (PUT /api/sites/{id}/settings/general):
// category_index_page, redirects and infra_pages_under_prefix.
//
// That PUT replaces every key it receives whole and leaves the others alone.
// So the rules are:
//   - a key absent from site.yaml (or null) is never sent, so it cannot clear
//     what Studio holds;
//   - an explicit empty value ({}, [], false) is sent and replaces the stored
//     value, the same absent-versus-explicit rule provider map fields follow;
//   - before the PUT the live values are read (GET) and diffed per key, so
//     `provision diff` shows exactly what a replace would change, and an
//     unchanged section is a NOOP with no write and no version row.
//
// A Studio whose allowlist predates a key drops it without an error. After a
// real PUT the section is read back and any key that did not stick is a warning.
package commands

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// siteRedirect mirrors the platform's types.RedirectConfig.
type siteRedirect struct {
	// Path is the old URL path relative to the site root ("/spreuken/thema/oud/").
	Path string `yaml:"path" json:"path"`
	// Target is an absolute http(s) URL or a site-relative path.
	Target string `yaml:"target" json:"target"`
	// Title is shown on the stub page. Optional.
	Title string `yaml:"title,omitempty" json:"title,omitempty"`
}

// siteGeneralPayload returns the general-settings keys site.yaml sets, or nil
// when it sets none. Only non-nil values are included (see file comment).
func siteGeneralPayload(sy siteYAML) map[string]any {
	p := map[string]any{}
	if sy.CategoryIndexPage != nil {
		p["category_index_page"] = sy.CategoryIndexPage
	}
	if sy.Redirects != nil {
		p["redirects"] = sy.Redirects
	}
	if sy.InfraPagesUnderPrefix != nil {
		p["infra_pages_under_prefix"] = *sy.InfraPagesUnderPrefix
	}
	if len(p) == 0 {
		return nil
	}
	return p
}

// validateCategoryPages checks the category_pages keys the CLI knows about
// before anything is written. exclude_values must be a list of strings: the
// platform decodes it into []string, so anything else fails the whole PUT.
func validateCategoryPages(pages any) error {
	if pages == nil {
		return nil
	}
	list, ok := pages.([]any)
	if !ok {
		return fmt.Errorf("must be a list of category page entries")
	}
	for i, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("entry %d: must be a mapping", i)
		}
		ev, has := m["exclude_values"]
		if !has || ev == nil {
			continue
		}
		vals, ok := ev.([]any)
		if !ok {
			return fmt.Errorf("entry %d: exclude_values must be a list of strings", i)
		}
		for j, v := range vals {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("entry %d: exclude_values[%d] must be a string", i, j)
			}
		}
	}
	return nil
}

// normalizeJSON round-trips v through JSON so typed site.yaml values compare
// equal to what the API returns.
func normalizeJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// fmtJSONFull formats a diff value without truncation. Strings print bare,
// like fmtJSON.
func fmtJSONFull(v any) string {
	if s, ok := v.(string); ok {
		if s == "" {
			return `""`
		}
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// diffGeneralKeys compares each key of the local payload with the live
// section. Live keys site.yaml does not set are not compared: they are not
// sent, so they do not change.
func diffGeneralKeys(remote, local map[string]any) []fieldChange {
	keys := make([]string, 0, len(local))
	for k := range local {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []fieldChange
	for _, k := range keys {
		rv, has := remote[k]
		if !has {
			out = append(out, fieldChange{Path: k, Old: "(absent)", New: fmtJSONFull(local[k])})
			continue
		}
		out = append(out, diffJSONValuesFmt(k, rv, local[k], fmtJSONFull)...)
	}
	return out
}

// getGeneralSettings reads the live general-settings section of a site.
func getGeneralSettings(c *provisionClient, siteID uint) (map[string]any, error) {
	body, status, err := c.get(fmt.Sprintf("/sites/%d/settings/general", siteID))
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("status=%d body=%s", status, provisionSummarize(body))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// pushSiteGeneralKeys diffs the general-settings keys site.yaml sets against
// the live site and PUTs only those keys when something differs.
func pushSiteGeneralKeys(c *provisionClient, siteID uint, sy siteYAML) error {
	payload := siteGeneralPayload(sy)
	if payload == nil {
		return nil
	}
	local, err := normalizeJSON(payload)
	if err != nil {
		return fmt.Errorf("encode general settings: %w", err)
	}
	localMap := local.(map[string]any)

	keys := make([]string, 0, len(localMap))
	for k := range localMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	label := fmt.Sprintf("general settings site=%d (from site.yaml: %s)", siteID, strings.Join(keys, ", "))

	remote, gerr := getGeneralSettings(c, siteID)
	if gerr != nil {
		c.Warn("site %d: could not read live general settings for a diff (%v); sending %s without one", siteID, gerr, strings.Join(keys, ", "))
	} else {
		changes := diffGeneralKeys(remote, localMap)
		if len(changes) == 0 {
			fmt.Printf("NOOP general settings site=%d (from site.yaml: %s)\n", siteID, strings.Join(keys, ", "))
			return nil
		}
		fmt.Printf("UPDATE %s\n", label)
		for _, ch := range changes {
			fmt.Printf("  %s\n", ch)
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode general settings: %w", err)
	}
	path := fmt.Sprintf("/sites/%d/settings/general", siteID)
	respBody, status, err := c.put(path, body)
	if err != nil || status >= 300 {
		return provisionAPIErr("PUT "+path, status, respBody, err)
	}
	if c.dryRun {
		return nil
	}

	// Read back: a Studio that does not know a key drops it without an error.
	after, err := getGeneralSettings(c, siteID)
	if err != nil {
		c.Warn("site %d: could not read general settings back after the PUT (%v)", siteID, err)
		return nil
	}
	for _, k := range keys {
		got, has := after[k]
		switch {
		case !has:
			c.Warn("site %d: Studio did not store %s; this Studio's general settings do not accept that key yet", siteID, k)
		case !jsonDeepEqual(got, localMap[k]):
			c.Warn("site %d: Studio stored %s as %s, not %s", siteID, k, fmtJSON(got), fmtJSON(localMap[k]))
		}
	}
	fmt.Printf("provision: site %d general settings updated from site.yaml (%s)\n", siteID, strings.Join(keys, ", "))
	return nil
}
