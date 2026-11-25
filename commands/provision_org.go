package commands

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// orgMembersConfig is the YAML representation of seeded organization
// memberships under studio/access/org-members.yaml.
//
// Why this exists: orgs created via the bootstrap admin API key (cmd/provision
// or curl) come up with zero rows in user_organizations because the
// CreateOrganization handler only auto-seats the creator when user != nil
// (session auth). Without at least one admin row, no one can manage members
// from the UI — see WVS Hoveniers (org id 18) circa 2026-05-21.
//
// This step reconciles a YAML-declared admin/member list against the live org
// membership. Idempotent: existing memberships are left untouched, missing
// ones are added. It does NOT prune surplus memberships — removing a real
// admin should be a deliberate human action, not a YAML omission.
//
// Who is "added": a system-wide admin API key still adds the membership
// directly (201). Any other caller (an organization's own key or a user) gets
// an invitation instead (202 {"status":"invitation_sent"}): the person joins
// only when they accept, and the answer is the same whether or not they have
// an account. Addresses with a pending invitation for this organization are
// skipped, so a re-run does not mail them again or revoke the link they have.
//
// Example (studio/access/org-members.yaml):
//
//	members:
//	  - email: robin@us2.nl
//	    role: admin
//	  - email: thomas@example.com
//	    role: member
type orgMembersConfig struct {
	Members []orgMemberEntry `yaml:"members"`
}

type orgMemberEntry struct {
	Email string `yaml:"email"`
	Role  string `yaml:"role"`
}

// validRoles lists the org membership roles accepted by
// POST /api/organizations/{id}/members. Mirrors the dropdown in
// web/src/pages/OrganizationDetail.tsx.
var validOrgMemberRoles = map[string]bool{
	"admin":  true,
	"member": true,
	"viewer": true,
}

// upsertOrgMembers walks cfg.Members and ensures each one has a
// user_organizations row for orgID. Skips entries where the user already
// exists at any role (does not downgrade or upgrade — drift on role is
// surfaced as a warning, the operator can fix it via the UI). Logs a warning
// and continues if the user doesn't exist in the system yet (typical for a
// first-bootstrap before the human has logged in via SSO).
func upsertOrgMembers(c *provisionClient, orgID uint, cfg orgMembersConfig) error {
	if len(cfg.Members) == 0 {
		return nil
	}

	// Validate the YAML up front so a typo doesn't waste an API roundtrip
	// or — worse — get caught only halfway through provisioning.
	normalized := make([]orgMemberEntry, 0, len(cfg.Members))
	for _, m := range cfg.Members {
		email := strings.TrimSpace(m.Email)
		role := strings.TrimSpace(m.Role)
		if email == "" {
			return fmt.Errorf("org-members: entry missing email")
		}
		if role == "" {
			role = "admin"
		}
		if !validOrgMemberRoles[role] {
			return fmt.Errorf("org-members: invalid role %q for %s (want admin|member|viewer)", role, email)
		}
		normalized = append(normalized, orgMemberEntry{Email: email, Role: role})
	}

	// Snapshot the current member set once. The endpoint returns the full
	// list with role per user, so we can both detect "already a member" and
	// surface role drift in one pass.
	existing, err := listOrgMembers(c, orgID)
	if err != nil {
		return fmt.Errorf("list org members: %w", err)
	}
	// Best effort: without the list a re-run re-invites (one extra mail and
	// a fresh link), which is worse than skipping but not wrong.
	pending, err := listPendingOrgInvites(c, orgID)
	if err != nil {
		c.Warn("org-members: could not list pending invitations for org %d (%v); members who have not accepted yet will be invited again", orgID, err)
		pending = map[string]bool{}
	}

	for _, m := range normalized {
		email := m.Email
		role := m.Role

		if cur, ok := existing[strings.ToLower(email)]; ok {
			if cur.Role != role {
				c.Warn("org-members: %s is already a %s of org %d (YAML wants %s) — fix via UI if intentional",
					email, cur.Role, orgID, role)
			} else {
				fmt.Printf("provision: org-members %s already %s of org %d\n", email, role, orgID)
			}
			continue
		}
		if pending[strings.ToLower(email)] {
			fmt.Printf("provision: org-members %s already invited to org %d, waiting for them to accept\n", email, orgID)
			continue
		}

		payload, _ := json.Marshal(map[string]string{
			"email": email,
			"role":  role,
		})
		endpoint := fmt.Sprintf("/organizations/%d/members", orgID)
		body, status, err := c.post(endpoint, payload)
		switch {
		case err != nil:
			return fmt.Errorf("add org member %s: %w", email, err)
		case status == 201:
			fmt.Printf("provision: added %s as %s of org %d\n", email, role, orgID)
		case status == 202:
			// Not a system-admin key: Studio sent an invitation. Same answer
			// whether or not the address has an account, by design.
			fmt.Printf("provision: invited %s as %s of org %d (they join once they accept)\n", email, role, orgID)
		case status == 404:
			// User doesn't exist yet — typical pre-SSO-bootstrap. Warn and
			// continue so the rest of the provision run completes; rerun
			// after the human has logged in once to create the User row.
			c.Warn("org-members: user %s not found (has not logged in yet) — skipping",
				email)
		case status == 400 && strings.Contains(string(body), "already in this organization"):
			// Race with concurrent UI edit: listOrgMembers was stale.
			// Treat as success.
			fmt.Printf("provision: %s already in org %d (race-skip)\n", email, orgID)
		default:
			return provisionAPIErr(fmt.Sprintf("add org member %s", email), status, body, nil)
		}
	}

	return nil
}

// listOrgMembers returns a map of lowercased-email → existing membership.
// Uses the existing GET /api/organizations/{id}/members endpoint.
func listOrgMembers(c *provisionClient, orgID uint) (map[string]orgMemberRow, error) {
	endpoint := fmt.Sprintf("/organizations/%d/members", orgID)
	body, status, err := c.get(endpoint)
	if err != nil || status != 200 {
		return nil, fmt.Errorf("status=%d err=%v body=%s", status, err, string(body))
	}
	var rows []orgMemberRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make(map[string]orgMemberRow, len(rows))
	for _, r := range rows {
		if r.User != nil && r.User.Email != "" {
			out[strings.ToLower(r.User.Email)] = r
		}
	}
	return out, nil
}

// listPendingOrgInvites returns the lower-cased emails with a pending
// org_member invitation in orgID (GET /api/admin/invitations?status=pending,
// paged at the server's cap of 200).
func listPendingOrgInvites(c *provisionClient, orgID uint) (map[string]bool, error) {
	const page = 200
	out := map[string]bool{}
	for offset := 0; ; offset += page {
		body, status, err := c.getForOrg(fmt.Sprintf("/admin/invitations?status=pending&limit=%d&offset=%d", page, offset), orgID)
		if err != nil || status != 200 {
			return nil, fmt.Errorf("status=%d err=%v", status, err)
		}
		var resp struct {
			Invitations []struct {
				Email      string `json:"email"`
				TargetType string `json:"target_type"`
			} `json:"invitations"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		for _, inv := range resp.Invitations {
			if inv.TargetType == "org_member" {
				out[strings.ToLower(inv.Email)] = true
			}
		}
		if len(resp.Invitations) < page {
			return out, nil
		}
	}
}

type orgMemberRow struct {
	UserID uint   `json:"user_id"`
	Role   string `json:"role"`
	User   *struct {
		Email string `json:"email"`
	} `json:"user"`
}

func applyOrgMembers(c *provisionClient, dir string, orgID uint) error {
	mf := filepath.Join(dir, "access", "org-members.yaml")
	if !fileExists(mf) {
		return nil
	}
	var cfg orgMembersConfig
	mustReadYAML(mf, &cfg)
	return upsertOrgMembers(c, orgID, cfg)
}
