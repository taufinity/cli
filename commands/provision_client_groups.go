package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// clientGroupConfig is the YAML representation of one ClientGroup under
// the studio/access/client-groups/ directory.
//
// Example (studio/access/client-groups/efteling.yaml):
//
//	slug: efteling
//	name: Efteling
//	identifiers:
//	  - Efteling
//	  - "Efteling B.V."
//	  - EFT
//	members:
//	  - rik@efteling.com
//	nav_config:
//	  profile: client_reporting
//	  pin: [reports]
//	  hide: [widgets]
type clientGroupConfig struct {
	Slug        string         `yaml:"slug"`
	Name        string         `yaml:"name"`
	Identifiers []string       `yaml:"identifiers"`
	Members     []string       `yaml:"members"` // emails
	NavConfig   *navConfigYAML `yaml:"nav_config,omitempty"`
}

// navConfigYAML mirrors navprofile.Config for YAML parsing.
type navConfigYAML struct {
	Profile string   `yaml:"profile" json:"profile"`
	Hide    []string `yaml:"hide,omitempty" json:"hide,omitempty"`
	Pin     []string `yaml:"pin,omitempty" json:"pin,omitempty"`
	Extra   []string `yaml:"extra,omitempty" json:"extra,omitempty"`
}

// upsertClientGroupWithOpts creates the ClientGroup if it doesn't exist
// (slug match) and then reconciles its identifiers and members.
// Idempotent on slug. When pruneIdentifiers is true, identifiers
// present in the live group but NOT in cfg.Identifiers are REMOVED via
// the DELETE endpoint — explicit opt-in because removing identifiers
// shrinks the allowed_client_identifiers set for every group member
// (data-loss risk if the YAML is incomplete by accident).
//
// Calls the admin API at /admin/organizations/{org_id}/client-groups.
func upsertClientGroupWithOpts(c *provisionClient, orgID uint, cfg clientGroupConfig, pruneIdentifiers bool) error {
	if cfg.Slug == "" || cfg.Name == "" {
		return fmt.Errorf("client group config requires slug and name")
	}

	// Step 1: resolve or create the group. Look up by slug first — if it
	// exists we reuse the ID. Otherwise POST.
	group, err := resolveClientGroupBySlug(c, orgID, cfg.Slug)
	if err != nil {
		return fmt.Errorf("lookup client group %q: %w", cfg.Slug, err)
	}
	if group == nil {
		payload, _ := json.Marshal(map[string]any{
			"slug":        cfg.Slug,
			"name":        cfg.Name,
			"identifiers": cfg.Identifiers,
		})
		endpoint := fmt.Sprintf("/organizations/%d/client-groups", orgID)
		body, status, err := c.post(endpoint, payload)
		if err != nil || status >= 300 {
			return fmt.Errorf("create client group %q: status=%d err=%v body=%s",
				cfg.Slug, status, err, string(body))
		}
		var created struct {
			ID uint `json:"id"`
		}
		if err := json.Unmarshal(body, &created); err != nil || created.ID == 0 {
			return fmt.Errorf("create client group %q: bad response", cfg.Slug)
		}
		group = &resolvedGroup{ID: created.ID}
		fmt.Printf("provision: created client group %q (id=%d)\n", cfg.Slug, group.ID)
	} else {
		fmt.Printf("provision: client group %q already present (id=%d)\n", cfg.Slug, group.ID)
	}

	// Step 2: reconcile identifiers. Add any in cfg.Identifiers that
	// aren't already on the group. When pruneIdentifiers is true,
	// surplus identifiers (in the live group but not in YAML) are
	// removed via DELETE — opt-in because the operation shrinks the
	// allowed set for every member.
	if err := reconcileIdentifiers(c, orgID, group.ID, group.IdentifierSet, cfg.Identifiers); err != nil {
		return err
	}
	if pruneIdentifiers {
		if err := pruneSurplusIdentifiers(c, orgID, group.ID, group.IdentifierSet, cfg.Identifiers); err != nil {
			return err
		}
	}

	// Step 3: reconcile members by email. Skips users already in the group.
	for _, email := range cfg.Members {
		if err := addClientGroupMemberByEmail(c, orgID, group.ID, email); err != nil {
			return fmt.Errorf("add member %q: %w", email, err)
		}
	}

	// Step 4: upsert nav_config if specified in YAML.
	if cfg.NavConfig != nil {
		navJSON, err := json.Marshal(cfg.NavConfig)
		if err != nil {
			return fmt.Errorf("marshal nav_config for %q: %w", cfg.Slug, err)
		}
		navJSONStr := string(navJSON)
		if c.dryRun {
			fmt.Printf("provision(dry-run): client group %q nav_config = %s\n", cfg.Slug, navJSON)
		} else {
			payload, _ := json.Marshal(map[string]any{"nav_config": navJSONStr})
			endpoint := fmt.Sprintf("/organizations/%d/client-groups/%d", orgID, group.ID)
			_, status, err := c.patch(endpoint, payload)
			if err != nil || status >= 300 {
				return fmt.Errorf("update nav_config for %q: status=%d err=%v", cfg.Slug, status, err)
			}
			fmt.Printf("provision: updated nav_config for client group %q\n", cfg.Slug)
		}
	}
	return nil
}

// resolvedGroup is the slimmed-down shape we read from
// GET /client-groups?slug=...
type resolvedGroup struct {
	ID            uint
	IdentifierSet map[string]bool
}

func resolveClientGroupBySlug(c *provisionClient, orgID uint, slug string) (*resolvedGroup, error) {
	endpoint := fmt.Sprintf("/organizations/%d/client-groups?slug=%s", orgID, slug)
	body, status, err := c.get(endpoint)
	if err != nil || status >= 300 {
		return nil, fmt.Errorf("status=%d err=%v body=%s", status, err, string(body))
	}
	var groups []struct {
		ID          uint `json:"id"`
		Identifiers []struct {
			IdentifierValue string `json:"identifier_value"`
		} `json:"identifiers"`
	}
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil, fmt.Errorf("decode list: %w", err)
	}
	if len(groups) == 0 {
		return nil, nil
	}
	set := make(map[string]bool, len(groups[0].Identifiers))
	for _, id := range groups[0].Identifiers {
		set[id.IdentifierValue] = true
	}
	return &resolvedGroup{ID: groups[0].ID, IdentifierSet: set}, nil
}

// pruneSurplusIdentifiers DELETEs any identifier in the live group
// that's not present in the YAML's want list. Logs each deletion to
// stdout so dry-run can preview.
func pruneSurplusIdentifiers(c *provisionClient, orgID, groupID uint, existing map[string]bool, want []string) error {
	wantSet := make(map[string]bool, len(want))
	for _, v := range want {
		wantSet[v] = true
	}
	for live := range existing {
		if wantSet[live] {
			continue
		}
		endpoint := fmt.Sprintf("/organizations/%d/client-groups/%d/identifiers/%s",
			orgID, groupID, urlPathEscape(live))
		_, status, err := c.write("DELETE", endpoint, nil)
		if err != nil || (status >= 300 && status != 404) {
			return fmt.Errorf("prune identifier %q: status=%d err=%v", live, status, err)
		}
		fmt.Printf("provision: removed surplus identifier %q from client group %d\n", live, groupID)
	}
	return nil
}

// urlPathEscape escapes a value for use in a URL path segment.
// We don't import net/url here to keep deps minimal — but we do need
// to encode spaces and reserved chars.
func urlPathEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '?' || c == '#' || c == '/' || c == '%' || c == '&' || c == '+' {
			out = append(out, '%')
			out = append(out, hexDigit(c>>4), hexDigit(c&0x0F))
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

func hexDigit(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'A' + (b - 10)
}

func reconcileIdentifiers(c *provisionClient, orgID, groupID uint, existing map[string]bool, want []string) error {
	for _, v := range want {
		if existing[v] {
			continue
		}
		payload, _ := json.Marshal(map[string]string{"identifier_value": v, "source": "manual"})
		endpoint := fmt.Sprintf("/organizations/%d/client-groups/%d/identifiers", orgID, groupID)
		_, status, err := c.post(endpoint, payload)
		if err != nil || (status >= 300 && status != 409) {
			return fmt.Errorf("add identifier %q: status=%d err=%v", v, status, err)
		}
		fmt.Printf("provision: added identifier %q to client group %d\n", v, groupID)
	}
	return nil
}

// addClientGroupMemberByEmail resolves the email to a user_id via the
// existing /users endpoint, then calls AddMember. Existing membership
// is treated as a no-op (idempotent re-runs of provision).
//
// Requires the provision tool's API key to have system-admin access to
// /users — that's the existing operational convention for cmd/provision.
func addClientGroupMemberByEmail(c *provisionClient, orgID, groupID uint, email string) error {
	userID, err := lookupUserIDByEmail(c, email)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return inviteClientGroupMember(c, orgID, groupID, email)
		}
		return err
	}

	// Check if the user is already a member of this group — skip rather
	// than 409 noise. Calls GET members and scans for the user id.
	if already, err := isMemberOfGroup(c, orgID, groupID, userID); err != nil {
		return fmt.Errorf("check existing membership: %w", err)
	} else if already {
		return nil
	}

	payload, _ := json.Marshal(map[string]any{"user_id": userID})
	endpoint := fmt.Sprintf("/organizations/%d/client-groups/%d/members", orgID, groupID)
	_, status, err := c.post(endpoint, payload)
	if err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	switch {
	case status == 201:
		fmt.Printf("provision: added %s to client group %d\n", email, groupID)
	case status == 409:
		return fmt.Errorf("add member %q: 409 (likely mutual-exclusion: user is in user_organizations of this org)", email)
	case status >= 300:
		return fmt.Errorf("add member: status=%d", status)
	}
	return nil
}

// inviteClientGroupMember sends a platform invitation to email so the user
// can register and land directly in the specified client group.
func inviteClientGroupMember(c *provisionClient, orgID, groupID uint, email string) error {
	cgID := groupID
	payload, _ := json.Marshal(map[string]any{
		"email":                  email,
		"target_type":            "client_group_member",
		"target_client_group_id": cgID,
		"skip_email":             c.noInviteEmail,
	})
	body, status, err := c.writeForOrg("POST", "/admin/invitations", payload, orgID)
	if err != nil || (status >= 300 && status != 409) {
		return fmt.Errorf("invite %s: status=%d err=%v body=%s", email, status, err, string(body))
	}
	if status == 409 {
		fmt.Printf("provision: invitation for %s already pending\n", email)
		return nil
	}
	fmt.Printf("provision: invited %s to client group %d\n", email, groupID)
	return nil
}

func lookupUserIDByEmail(c *provisionClient, email string) (uint, error) {
	body, status, err := c.get("/users")
	if err != nil || status >= 300 {
		return 0, fmt.Errorf("list users: status=%d err=%v", status, err)
	}
	var users []struct {
		ID    uint   `json:"id"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &users); err != nil {
		return 0, fmt.Errorf("decode users: %w", err)
	}
	for _, u := range users {
		if u.Email == email {
			return u.ID, nil
		}
	}
	return 0, fmt.Errorf("user %q not found", email)
}

func isMemberOfGroup(c *provisionClient, orgID, groupID, userID uint) (bool, error) {
	endpoint := fmt.Sprintf("/organizations/%d/client-groups/%d/members", orgID, groupID)
	body, status, err := c.get(endpoint)
	if err != nil || status >= 300 {
		return false, fmt.Errorf("status=%d err=%v body=%s", status, err, string(body))
	}
	var members []struct {
		UserID uint `json:"user_id"`
	}
	if err := json.Unmarshal(body, &members); err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}
	for _, m := range members {
		if m.UserID == userID {
			return true, nil
		}
	}
	return false, nil
}

func applyClientGroups(c *provisionClient, dir string, orgID uint, pruneIdentifiers bool) error {
	cgDir := filepath.Join(dir, "access", "client-groups")
	if !fileExists(cgDir) {
		return nil
	}
	entries, err := os.ReadDir(cgDir)
	if err != nil {
		return fmt.Errorf("client-groups/: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		cf := filepath.Join(cgDir, e.Name())
		var cfg clientGroupConfig
		mustReadYAML(cf, &cfg)
		if err := upsertClientGroupWithOpts(c, orgID, cfg, pruneIdentifiers); err != nil {
			return fmt.Errorf("client-group %s: %w", e.Name(), err)
		}
	}
	return nil
}
