package commands

import (
	"fmt"
	"sync"

	"github.com/taufinity/cli/internal/buildinfo"
)

// provisionUserAgent identifies WHICH binary and WHICH build performed a write.
//
// Both this CLI and ai-site-gen's (deprecated) cmd/provision used to send the same
// hardcoded "taufinity-provision/1.0". So when a provision apply deleted a live
// playbook step on 2026-07-14 at 14:10:28, the audit log looked like this:
//
//	DELETE /api/playbooks/28/steps/329  taufinity-provision/1.0  204
//
// and could not answer the only question that mattered: which binary, on which
// build, run by whom. Two clients with identical identity are, for forensics, one
// anonymous client.
//
// The shape is deliberately parseable, so an audit query or an alert can filter on
// the product token and still read the build:
//
//	taufinity-cli/1.4.2 (provision; commit=a1b2c3d)
//	taufinity-cli/dev (provision; commit=a1b2c3d; dirty)
//
// A "dirty" build is worth surfacing loudly: it means the config that was applied
// came from a working tree nobody can reconstruct from git.
var provisionUserAgent = sync.OnceValue(func() string {
	info := buildinfo.FromBuildtime(Version, GitCommit, BuildTime)

	commit := info.Commit
	if commit == "" {
		commit = "unknown"
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}

	ua := fmt.Sprintf("taufinity-cli/%s (provision; commit=%s", info.Version, commit)
	if info.Dirty {
		ua += "; dirty"
	}
	return ua + ")"
})
