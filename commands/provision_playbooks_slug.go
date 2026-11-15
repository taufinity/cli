package commands

import "strings"

// matchPlaybooks finds the live playbooks a YAML config refers to.
//
// A YAML slug is the provision key, so it matches on slug first. When nothing
// carries that slug yet, it falls back to the name and reports adopt=true: the
// playbook was created before Studio honoured a requested slug, and Studio stored
// one derived from the name. Updating it with the YAML slug in the payload
// re-keys it, instead of creating a second copy next to it. Without a YAML slug
// the name is the only key, as before.
//
// More than one match is returned as is; the caller refuses an ambiguous apply.
func matchPlaybooks(existing []provisionPlaybookListItem, cfg playbookConfig) (matches []provisionPlaybookListItem, adopt bool) {
	if cfg.Slug != "" {
		for _, p := range existing {
			if p.Slug == cfg.Slug {
				matches = append(matches, p)
			}
		}
		if len(matches) > 0 {
			return matches, false
		}
	}
	for _, p := range existing {
		if strings.EqualFold(p.Name, cfg.Name) {
			matches = append(matches, p)
		}
	}
	return matches, cfg.Slug != "" && len(matches) > 0
}

// addSlug puts the YAML slug in a create or update payload. Studio stores it as
// the playbook's key (and refuses an invalid or taken one); without a YAML slug
// nothing is sent, so create derives one from the name and update leaves it.
func addSlug(out map[string]interface{}, cfg playbookConfig) {
	if cfg.Slug != "" {
		out["slug"] = cfg.Slug
	}
}
