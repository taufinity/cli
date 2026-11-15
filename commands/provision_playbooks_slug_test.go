package commands

import "testing"

// The finance playbook that started this: Studio stored the name-derived slug,
// the YAML says finance-actions.
var slugFixturePlaybooks = []provisionPlaybookListItem{
	{ID: 170, Name: "Finance: customer invoices and actions (financial manager)", Slug: "finance-customer-invoices-and-actions-financial-manager"},
	{ID: 169, Name: "Financial manager", Slug: "financial-manager"},
}

func TestMatchPlaybooks(t *testing.T) {
	t.Run("slug_match_wins", func(t *testing.T) {
		live := append([]provisionPlaybookListItem{{ID: 200, Name: "Other name", Slug: "finance-actions"}}, slugFixturePlaybooks...)
		m, adopt := matchPlaybooks(live, playbookConfig{Name: "Finance: customer invoices and actions (financial manager)", Slug: "finance-actions"})
		if len(m) != 1 || m[0].ID != 200 || adopt {
			t.Fatalf("got %v adopt=%v, want only id 200 by slug, no adopt", m, adopt)
		}
	})
	t.Run("no_slug_match_adopts_by_name", func(t *testing.T) {
		m, adopt := matchPlaybooks(slugFixturePlaybooks, playbookConfig{Name: "finance: customer invoices and actions (financial manager)", Slug: "finance-actions"})
		if len(m) != 1 || m[0].ID != 170 || !adopt {
			t.Fatalf("got %v adopt=%v, want id 170 adopted by name", m, adopt)
		}
	})
	t.Run("no_yaml_slug_matches_by_name_without_adopt", func(t *testing.T) {
		m, adopt := matchPlaybooks(slugFixturePlaybooks, playbookConfig{Name: "Financial manager"})
		if len(m) != 1 || m[0].ID != 169 || adopt {
			t.Fatalf("got %v adopt=%v, want id 169, no adopt", m, adopt)
		}
	})
	t.Run("nothing_matches_creates", func(t *testing.T) {
		m, adopt := matchPlaybooks(slugFixturePlaybooks, playbookConfig{Name: "Brand new", Slug: "brand-new"})
		if len(m) != 0 || adopt {
			t.Fatalf("got %v adopt=%v, want no match", m, adopt)
		}
	})
	t.Run("ambiguous_name_fallback_returns_all", func(t *testing.T) {
		live := []provisionPlaybookListItem{{ID: 1, Name: "Twin", Slug: "twin"}, {ID: 2, Name: "Twin", Slug: "twin-2"}}
		m, _ := matchPlaybooks(live, playbookConfig{Name: "Twin", Slug: "twin-yaml"})
		if len(m) != 2 {
			t.Fatalf("got %v, want both so the apply refuses as ambiguous", m)
		}
	})
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
