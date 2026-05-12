// Package buildinfo resolves the running binary's version and commit metadata
// using a layered fallback: ldflag-injected values > module pseudo-version >
// VCS settings embedded by Go 1.18+ > literal "dev".
package buildinfo

import (
	"regexp"
	"runtime/debug"
	"strings"
)

// pseudoVersionRe captures the 14-digit timestamp and the short hash that
// follows it in a Go pseudo-version. The hash is the segment after the
// timestamp and before any "+dirty" suffix; we deliberately stop at "+" so we
// never include build metadata in the SHA.
var pseudoVersionRe = regexp.MustCompile(`(\d{14})-([0-9a-f]+)`)

// Info is the resolved metadata for the running binary.
type Info struct {
	// Version is a human-readable version string. Possible shapes:
	//   "v1.2.3"           — release tag (ldflag or @v1.2.3 install)
	//   "abc1234"          — short SHA when no tag is available
	//   "abc1234 (2026-05-12)" — pseudo-version rendered from go install ...@latest
	//   "abc1234+dirty"    — built from a tree with uncommitted changes
	//   "dev"              — no metadata at all (go build -trimpath -buildvcs=false, etc.)
	Version string

	// Commit is the full or short SHA, or "unknown" if unavailable.
	Commit string

	// BuildTime is an ISO-8601 timestamp from ldflags or vcs.time, or "unknown".
	BuildTime string

	// Dirty is true when the binary was built from a working tree with
	// uncommitted edits. Callers should disable any "is my binary up to date"
	// comparison when Dirty is true — vcs.revision in that case is the parent
	// commit, not what's actually running.
	Dirty bool
}

// Inputs override the build-time globals. They exist so tests can construct
// arbitrary scenarios without touching package state.
type Inputs struct {
	LDFlagVersion   string
	LDFlagCommit    string
	LDFlagBuildTime string
	BuildInfo       *debug.BuildInfo
}

// Resolve applies the fallback chain to the given inputs.
func Resolve(in Inputs) Info {
	out := Info{
		Version:   "dev",
		Commit:    "unknown",
		BuildTime: "unknown",
	}

	var vcsRevision, vcsTime string
	if in.BuildInfo != nil {
		for _, s := range in.BuildInfo.Settings {
			switch s.Key {
			case "vcs.revision":
				vcsRevision = s.Value
			case "vcs.time":
				vcsTime = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					out.Dirty = true
				}
			}
		}
	}

	// Commit: ldflag > vcs.revision > "unknown".
	switch {
	case in.LDFlagCommit != "" && in.LDFlagCommit != "unknown":
		out.Commit = in.LDFlagCommit
	case vcsRevision != "":
		out.Commit = vcsRevision
	}

	// BuildTime: ldflag > vcs.time > "unknown".
	switch {
	case in.LDFlagBuildTime != "" && in.LDFlagBuildTime != "unknown":
		out.BuildTime = in.LDFlagBuildTime
	case vcsTime != "":
		out.BuildTime = vcsTime
	}

	// Version: ldflag > Main.Version (skip "(devel)") > short SHA (with optional pseudo-version date) > "dev".
	switch {
	case in.LDFlagVersion != "" && in.LDFlagVersion != "dev":
		out.Version = in.LDFlagVersion
	case in.BuildInfo != nil && in.BuildInfo.Main.Version != "" && in.BuildInfo.Main.Version != "(devel)":
		out.Version = renderModuleVersion(in.BuildInfo.Main.Version, vcsRevision, vcsTime)
	case vcsRevision != "":
		out.Version = short(vcsRevision)
	}

	if out.Dirty {
		out.Version += "+dirty"
	}

	return out
}

// renderModuleVersion makes pseudo-versions human-readable.
//
// Go produces three pseudo-version shapes depending on whether the build
// commit's ancestry contains a tag:
//
//	v0.0.0-YYYYMMDDHHMMSS-shorthash                (no tag in ancestry)
//	vX.Y.Z-0.YYYYMMDDHHMMSS-shorthash              (tag in ancestry, ancestor is the tag commit)
//	vX.Y.(Z+1)-0.YYYYMMDDHHMMSS-shorthash          (tag in ancestry, ancestor is N commits before)
//
// Dirty trees additionally get a "+dirty" suffix on the SHA segment. We
// strip "+dirty" before parsing (Resolve owns the Dirty flag) and find the
// timestamp by looking for a 14-digit numeric token rather than relying on
// positional indexing, which broke on the second shape above.
//
// Real release tags ("v1.2.3") pass through unchanged.
func renderModuleVersion(modVersion, vcsRevision, vcsTime string) string {
	if !isPseudoVersion(modVersion) {
		// Strip a trailing "+dirty" if present — Dirty is handled in Resolve.
		return trimDirty(modVersion)
	}

	// Match the timestamp + sha tail. The "0." prefix in patch-level
	// pseudo-versions (vX.Y.Z-0.timestamp-sha) glues the digits to the
	// preceding segment after a split, so we extract directly from the raw
	// string instead.
	match := pseudoVersionRe.FindStringSubmatch(modVersion)

	sha := ""
	dateRaw := ""
	if len(match) == 3 {
		dateRaw = match[1]
		sha = match[2]
	}
	if sha == "" {
		sha = vcsRevision
	}
	sha = short(sha)

	date := ""
	if len(dateRaw) >= 8 {
		date = dateRaw[:4] + "-" + dateRaw[4:6] + "-" + dateRaw[6:8]
	} else if vcsTime != "" && len(vcsTime) >= 10 {
		date = vcsTime[:10]
	}

	if date == "" {
		return sha
	}
	return sha + " (" + date + ")"
}

func trimDirty(v string) string {
	return strings.TrimSuffix(v, "+dirty")
}

// isPseudoVersion is a cheap heuristic — the canonical form is documented at
// https://golang.org/ref/mod#pseudo-versions. We don't need to be exact;
// false negatives just leave the string untouched.
func isPseudoVersion(v string) bool {
	if !strings.HasPrefix(v, "v0.0.0-") && !strings.Contains(v, "-0.") {
		return false
	}
	return strings.Count(v, "-") >= 2
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// readBuildInfo is a package var so tests can stub it.
var readBuildInfo = debug.ReadBuildInfo

// FromBuildtime resolves Info using the supplied ldflag values and the running
// binary's embedded debug.BuildInfo.
func FromBuildtime(ldflagVersion, ldflagCommit, ldflagBuildTime string) Info {
	var bi *debug.BuildInfo
	if info, ok := readBuildInfo(); ok {
		bi = info
	}
	return Resolve(Inputs{
		LDFlagVersion:   ldflagVersion,
		LDFlagCommit:    ldflagCommit,
		LDFlagBuildTime: ldflagBuildTime,
		BuildInfo:       bi,
	})
}
