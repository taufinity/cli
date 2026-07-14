package commands

import (
	"fmt"
	"sync"

	"github.com/taufinity/cli/internal/buildinfo"
)

// provisionUserAgent identifies which binary and which build performed a write.
//
// Two separate provision implementations existed and both sent the same hardcoded
// product token, so a server-side audit log could not attribute a write to either
// of them. Two clients with identical identity are, for forensics, one anonymous
// client — and when a destructive apply needs explaining, "provision did it" is not
// an answer.
//
// The shape is deliberately parseable, so an audit query or alert can filter on the
// product token and still read the build:
//
//	taufinity-cli/1.4.2 (provision; commit=a1b2c3d)
//	taufinity-cli/dev (provision; commit=a1b2c3d; dirty)
//
// A dirty build is worth surfacing: the configuration it applied came from a working
// tree that cannot be reconstructed from git.
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
