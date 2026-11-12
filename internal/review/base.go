// Package review collects a local change set for `taufinity review`: it reads
// the local git repository only (so the git host does not matter), drops what
// must never leave the machine, masks secrets, and decodes Studio's streamed
// answer. It holds no network code.
package review

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// git runs `git -C dir args...` and returns stdout. A failure carries git's
// stderr, trimmed, so callers can wrap it without losing the reason.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	// quotepath off keeps non-ASCII paths readable in diff headers; fsmonitor
	// off stops a repository's own config from running a program on our behalf.
	base := []string{"-C", dir, "-c", "core.quotepath=false", "-c", "core.fsmonitor=false"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out, nil
}

// revParse resolves ref to a commit hash, or "" when it does not exist.
func revParse(ctx context.Context, dir, ref string) string {
	out, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ResolveBase picks the ref to compare against and the merge-base of that ref
// with HEAD. Order: the explicit ref, the current branch's upstream,
// origin/main, main. The merge-base is what the review diffs against, so a
// branch that has fallen behind its base does not show the base's own commits
// as removals.
func ResolveBase(ctx context.Context, dir, explicit string) (ref, mergeBase string, err error) {
	if revParse(ctx, dir, "HEAD") == "" {
		return "", "", fmt.Errorf("the repository has no commits yet, so there is nothing to compare against")
	}

	var tried []string
	candidates := []string{}
	if explicit != "" {
		// A ref that starts with "-" would reach git as an option. The base can
		// come from a tool call, so it is never passed on.
		if strings.HasPrefix(explicit, "-") {
			return "", "", fmt.Errorf("base %q is not a ref", explicit)
		}
		if revParse(ctx, dir, explicit) == "" {
			return "", "", fmt.Errorf("base %q does not resolve to a commit", explicit)
		}
		candidates = append(candidates, explicit)
	} else {
		tried = append(tried, "the upstream of the current branch")
		if out, uerr := git(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); uerr == nil {
			if up := strings.TrimSpace(string(out)); up != "" && revParse(ctx, dir, up) != "" {
				candidates = append(candidates, up)
			}
		}
		for _, c := range []string{"origin/main", "main"} {
			tried = append(tried, c)
			if revParse(ctx, dir, c) != "" {
				candidates = append(candidates, c)
			}
		}
	}

	for _, c := range candidates {
		out, merr := git(ctx, dir, "merge-base", "HEAD", c)
		if merr != nil {
			continue // unrelated histories: try the next candidate
		}
		return c, strings.TrimSpace(string(out)), nil
	}
	if explicit != "" {
		return "", "", fmt.Errorf("base %q shares no history with HEAD", explicit)
	}
	return "", "", fmt.Errorf("no base to compare against; tried %s. Pass --base <ref>", strings.Join(tried, ", "))
}
