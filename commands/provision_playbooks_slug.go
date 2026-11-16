package commands

import (
	"fmt"
	"regexp"
	"strings"
)

// matchPlaybooks finds the live playbook a YAML config refers to, among the
// organisation's own playbooks (the list also returns shared playbooks of other
// organisations, which provision must never update).
//
// A YAML slug is the provision key, so it matches on slug first. When nothing
// carries that slug, it falls back to the name, skipping playbooks whose slug
// another YAML in this apply declares (claimed), because those belong to that
// file. One name match is adopted (adopt=true: updated with the YAML slug, which
// re-keys it) only when its slug is the one Studio generates from its name: that
// is a playbook created before Studio honoured a requested slug. A name match
// with any other slug is refused rather than guessed at, since adopting it could
// re-key a playbook someone made by hand, and creating next to it duplicates the
// name. Without a YAML slug the name is the only key, as before.
//
// More than one match is returned as is; the caller refuses an ambiguous apply.
func matchPlaybooks(existing []provisionPlaybookListItem, cfg playbookConfig, orgID uint, claimed map[string]bool) (matches []provisionPlaybookListItem, adopt bool, err error) {
	var own []provisionPlaybookListItem
	for _, p := range existing {
		if p.OrganizationID == 0 || p.OrganizationID == orgID {
			own = append(own, p)
		}
	}
	if cfg.Slug != "" {
		for _, p := range own {
			if p.Slug == cfg.Slug {
				matches = append(matches, p)
			}
		}
		if len(matches) > 0 {
			return matches, false, nil
		}
	}
	for _, p := range own {
		if strings.EqualFold(p.Name, cfg.Name) && (cfg.Slug == "" || !claimed[p.Slug]) {
			matches = append(matches, p)
		}
	}
	if cfg.Slug == "" || len(matches) != 1 {
		return matches, false, nil
	}
	if !slugGeneratedFromName(matches[0]) {
		return nil, false, fmt.Errorf(
			"no playbook has slug %q, but playbook id=%d has the same name with slug %q, which was not generated from its name; "+
				"set `slug: %s` in the YAML to update that playbook, or rename one of them",
			cfg.Slug, matches[0].ID, matches[0].Slug, matches[0].Slug)
	}
	return matches, true, nil
}

// slugNonAlnum and studioNameSlug mirror database.Slugify in Studio
// (pkg/database/slug.go), which derives a playbook's slug from its name when
// none is requested.
var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func studioNameSlug(name string) string {
	s := strings.Trim(slugNonAlnum.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		return "item"
	}
	if len(s) > 80 {
		s = strings.TrimRight(s[:80], "-")
	}
	return s
}

// generatedSuffix is the "-" plus 8 hex characters Studio's EnsureUniqueSlug
// appends when the name-derived slug is already taken.
var generatedSuffix = regexp.MustCompile(`^-[0-9a-f]{8}$`)

// slugGeneratedFromName reports whether a live playbook's slug is the one Studio
// would have generated from its current name.
func slugGeneratedFromName(p provisionPlaybookListItem) bool {
	base := studioNameSlug(p.Name)
	return p.Slug == base || (strings.HasPrefix(p.Slug, base) && generatedSuffix.MatchString(p.Slug[len(base):]))
}

// declaredPlaybookSlugs reads every playbook YAML an apply of dir will process
// and returns the slugs they declare, so matchPlaybooks never adopts a playbook
// that another file owns.
func declaredPlaybookSlugs(files []string) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		var cfg playbookConfig
		mustReadYAML(f, &cfg)
		if cfg.Slug != "" {
			out[cfg.Slug] = true
		}
	}
	return out
}

// addSlug puts the YAML slug in a create or update payload. Studio stores it as
// the playbook's key (and refuses an invalid or taken one); without a YAML slug
// nothing is sent, so create derives one from the name and update leaves it.
func addSlug(out map[string]interface{}, cfg playbookConfig) {
	if cfg.Slug != "" {
		out["slug"] = cfg.Slug
	}
}
