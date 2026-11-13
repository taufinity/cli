package review

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
)

// RepoName is owner/name taken from the origin remote URL (ssh or https, any
// host, subgroups kept), or the folder name when there is no usable origin.
func RepoName(ctx context.Context, dir string) string {
	if out, err := git(ctx, dir, "remote", "get-url", "origin"); err == nil {
		if name := repoFromURL(strings.TrimSpace(string(out))); name != "" {
			return name
		}
	}
	top := dir
	if out, err := git(ctx, dir, "rev-parse", "--show-toplevel"); err == nil {
		top = strings.TrimSpace(string(out))
	}
	if abs, err := filepath.Abs(top); err == nil {
		top = abs
	}
	return filepath.Base(top)
}

// repoFromURL extracts the path of a git remote URL without host, user,
// credentials or the .git suffix.
func repoFromURL(raw string) string {
	var p string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		p = u.Path
	} else if i := strings.Index(raw, ":"); i >= 0 {
		p = raw[i+1:] // scp-like: git@host:owner/name.git
	} else {
		return ""
	}
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	return p
}
