package commands

import "testing"

// The finance playbook that started this: Studio stored the name-derived slug,
// the YAML says finance-actions.
var slugFixturePlaybooks = []provisionPlaybookListItem{
	{ID: 170, Name: "Finance: customer invoices and actions (financial manager)", Slug: "finance-customer-invoices-and-actions-financial-manager", OrganizationID: 12},
	{ID: 169, Name: "Financial manager", Slug: "financial-manager", OrganizationID: 12},
}

func TestMatchPlaybooks(t *testing.T) {
	const org = uint(12)
	finance := playbookConfig{Name: "Finance: customer invoices and actions (financial manager)", Slug: "finance-actions"}

	t.Run("slug_match_wins", func(t *testing.T) {
		live := append([]provisionPlaybookListItem{{ID: 200, Name: "Other name", Slug: "finance-actions", OrganizationID: org}}, slugFixturePlaybooks...)
		m, adopt, err := matchPlaybooks(live, finance, org, nil)
		if err != nil || len(m) != 1 || m[0].ID != 200 || adopt {
			t.Fatalf("got %v adopt=%v err=%v, want only id 200 by slug, no adopt", m, adopt, err)
		}
	})
	t.Run("adopts_slug_generated_from_name", func(t *testing.T) {
		m, adopt, err := matchPlaybooks(slugFixturePlaybooks, finance, org, map[string]bool{"finance-actions": true})
		if err != nil || len(m) != 1 || m[0].ID != 170 || !adopt {
			t.Fatalf("got %v adopt=%v err=%v, want id 170 adopted", m, adopt, err)
		}
	})
	t.Run("adopts_generated_slug_with_dedupe_suffix", func(t *testing.T) {
		live := []provisionPlaybookListItem{{ID: 7, Name: "Weekly Digest", Slug: "weekly-digest-3fa9c1d2"}}
		m, adopt, err := matchPlaybooks(live, playbookConfig{Name: "Weekly Digest", Slug: "digest"}, org, nil)
		if err != nil || len(m) != 1 || !adopt {
			t.Fatalf("got %v adopt=%v err=%v, want id 7 adopted", m, adopt, err)
		}
	})
	t.Run("hand_made_slug_refused", func(t *testing.T) {
		live := []provisionPlaybookListItem{{ID: 8, Name: "Weekly Digest", Slug: "my-own-digest"}}
		m, adopt, err := matchPlaybooks(live, playbookConfig{Name: "Weekly Digest", Slug: "digest"}, org, nil)
		if err == nil || adopt || m != nil {
			t.Fatalf("got %v adopt=%v err=%v, want a refusal naming id 8", m, adopt, err)
		}
	})
	t.Run("playbook_claimed_by_other_yaml_is_not_adopted", func(t *testing.T) {
		// Two YAML files, same name, different slugs: the second must create its
		// own playbook, never take over the first one's.
		live := []provisionPlaybookListItem{{ID: 1, Name: "Twin", Slug: "twin-a", OrganizationID: org}}
		m, adopt, err := matchPlaybooks(live, playbookConfig{Name: "Twin", Slug: "twin-b"}, org, map[string]bool{"twin-a": true, "twin-b": true})
		if err != nil || len(m) != 0 || adopt {
			t.Fatalf("got %v adopt=%v err=%v, want no match so twin-b is created", m, adopt, err)
		}
	})
	t.Run("other_org_shared_playbook_ignored", func(t *testing.T) {
		live := []provisionPlaybookListItem{
			{ID: 90, Name: finance.Name, Slug: "finance-actions", OrganizationID: 3},
			{ID: 91, Name: finance.Name, Slug: "finance-customer-invoices-and-actions-financial-manager", OrganizationID: 3},
		}
		m, adopt, err := matchPlaybooks(live, finance, org, nil)
		if err != nil || len(m) != 0 || adopt {
			t.Fatalf("got %v adopt=%v err=%v, want no match: both belong to organisation 3", m, adopt, err)
		}
	})
	t.Run("no_yaml_slug_matches_by_name_without_adopt", func(t *testing.T) {
		m, adopt, err := matchPlaybooks(slugFixturePlaybooks, playbookConfig{Name: "Financial manager"}, org, nil)
		if err != nil || len(m) != 1 || m[0].ID != 169 || adopt {
			t.Fatalf("got %v adopt=%v err=%v, want id 169, no adopt", m, adopt, err)
		}
	})
	t.Run("nothing_matches_creates", func(t *testing.T) {
		m, adopt, err := matchPlaybooks(slugFixturePlaybooks, playbookConfig{Name: "Brand new", Slug: "brand-new"}, org, nil)
		if err != nil || len(m) != 0 || adopt {
			t.Fatalf("got %v adopt=%v err=%v, want no match", m, adopt, err)
		}
	})
	t.Run("ambiguous_name_fallback_returns_all", func(t *testing.T) {
		live := []provisionPlaybookListItem{{ID: 1, Name: "Twin", Slug: "twin"}, {ID: 2, Name: "Twin", Slug: "twin-2"}}
		m, _, err := matchPlaybooks(live, playbookConfig{Name: "Twin", Slug: "twin-yaml"}, org, nil)
		if err != nil || len(m) != 2 {
			t.Fatalf("got %v err=%v, want both so the apply refuses as ambiguous", m, err)
		}
	})
}

// TestSlugChangeIsHighDrift: re-keying a playbook must be applied on purpose,
// with --allow-drift, never as a side effect of a routine apply.
func TestSlugChangeIsHighDrift(t *testing.T) {
	d := playbookDiff{Fields: diffPlaybookFields(
		playbookUpdatePayload(playbookConfig{Name: "Finance", Slug: "finance-actions"}, nil),
		playbookDetailRemote{Name: "Finance", Slug: "finance"})}
	if sev, _ := d.severity(); sev != driftHigh {
		t.Fatalf("severity = %v, want high", sev)
	}
}

func TestPlaybookPayload_Slug(t *testing.T) {
	t.Run("create_and_update_send_yaml_slug", func(t *testing.T) {
		cfg := playbookConfig{Name: "Finance", Slug: "finance-actions"}
		if got := playbookCreatePayload(cfg)["slug"]; got != "finance-actions" {
			t.Errorf("create payload slug = %v, want finance-actions", got)
		}
		if got := playbookUpdatePayload(cfg, nil)["slug"]; got != "finance-actions" {
			t.Errorf("update payload slug = %v, want finance-actions", got)
		}
	})
	t.Run("absent_slug_sends_nothing", func(t *testing.T) {
		cfg := playbookConfig{Name: "Finance"}
		if _, ok := playbookCreatePayload(cfg)["slug"]; ok {
			t.Error("create payload has a slug the YAML does not declare")
		}
		if _, ok := playbookUpdatePayload(cfg, nil)["slug"]; ok {
			t.Error("update payload has a slug the YAML does not declare; it would clear or overwrite the stored one")
		}
	})
}

// TestDiffPlaybookFields_Slug: an adoption must show up in `provision diff`
// before anything is applied, and a matching slug must not read as drift.
func TestDiffPlaybookFields_Slug(t *testing.T) {
	cfg := playbookConfig{Name: "Finance", Slug: "finance-actions"}
	t.Run("adoption_shows_slug_change", func(t *testing.T) {
		got := diffPlaybookFields(playbookUpdatePayload(cfg, nil), playbookDetailRemote{Name: "Finance", Slug: "finance-customer-invoices"})
		found := false
		for _, fc := range got {
			if fc.Path == "slug" {
				found = true
			}
		}
		if !found {
			t.Fatalf("diff %v has no slug change", got)
		}
	})
	t.Run("same_slug_no_change", func(t *testing.T) {
		for _, fc := range diffPlaybookFields(playbookUpdatePayload(cfg, nil), playbookDetailRemote{Name: "Finance", Slug: "finance-actions"}) {
			if fc.Path == "slug" {
				t.Fatalf("unchanged slug reported as drift: %v", fc)
			}
		}
	})
}
