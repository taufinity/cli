package buildinfo

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name        string
		in          Inputs
		wantVersion string
		wantCommit  string
		wantDirty   bool
	}{
		{
			name:        "all unset returns dev/unknown",
			in:          Inputs{},
			wantVersion: "dev",
			wantCommit:  "unknown",
		},
		{
			name: "ldflag version wins over everything",
			in: Inputs{
				LDFlagVersion: "v1.2.3",
				LDFlagCommit:  "deadbee",
				BuildInfo: &debug.BuildInfo{
					Main: debug.Module{Version: "v9.9.9"},
					Settings: []debug.BuildSetting{
						{Key: "vcs.revision", Value: "cafebabecafebabecafebabe"},
					},
				},
			},
			wantVersion: "v1.2.3",
			wantCommit:  "deadbee",
		},
		{
			name: "ldflag dev is treated as unset",
			in: Inputs{
				LDFlagVersion: "dev",
				BuildInfo: &debug.BuildInfo{
					Main: debug.Module{Version: "v2.0.0"},
				},
			},
			wantVersion: "v2.0.0",
			wantCommit:  "unknown",
		},
		{
			name: "(devel) module version is skipped",
			in: Inputs{
				BuildInfo: &debug.BuildInfo{
					Main: debug.Module{Version: "(devel)"},
					Settings: []debug.BuildSetting{
						{Key: "vcs.revision", Value: "abc1234deadbeef"},
					},
				},
			},
			wantVersion: "abc1234",
			wantCommit:  "abc1234deadbeef",
		},
		{
			name: "pseudo-version renders as short sha + date",
			in: Inputs{
				BuildInfo: &debug.BuildInfo{
					Main: debug.Module{Version: "v0.0.0-20260512143000-abc1234deadbeef"},
				},
			},
			wantVersion: "abc1234 (2026-05-12)",
			wantCommit:  "unknown",
		},
		{
			name: "pseudo-version after a tag (vX.Y.Z-0.timestamp-sha)",
			in: Inputs{
				BuildInfo: &debug.BuildInfo{
					Main: debug.Module{Version: "v0.1.1-0.20260512133242-c68a2e314bbf"},
				},
			},
			wantVersion: "c68a2e3 (2026-05-12)",
		},
		{
			name: "pseudo-version with +dirty suffix",
			in: Inputs{
				BuildInfo: &debug.BuildInfo{
					Main: debug.Module{Version: "v0.1.1-0.20260512133242-c68a2e314bbf+dirty"},
					Settings: []debug.BuildSetting{
						{Key: "vcs.modified", Value: "true"},
					},
				},
			},
			wantVersion: "c68a2e3 (2026-05-12)+dirty",
			wantDirty:   true,
		},
		{
			name: "real tag passes through",
			in: Inputs{
				BuildInfo: &debug.BuildInfo{
					Main: debug.Module{Version: "v1.5.0"},
				},
			},
			wantVersion: "v1.5.0",
		},
		{
			name: "dirty tree appends suffix and reports Dirty",
			in: Inputs{
				LDFlagVersion: "v1.0.0",
				BuildInfo: &debug.BuildInfo{
					Settings: []debug.BuildSetting{
						{Key: "vcs.modified", Value: "true"},
					},
				},
			},
			wantVersion: "v1.0.0+dirty",
			wantCommit:  "unknown",
			wantDirty:   true,
		},
		{
			name: "vcs.revision only -> short sha",
			in: Inputs{
				BuildInfo: &debug.BuildInfo{
					Settings: []debug.BuildSetting{
						{Key: "vcs.revision", Value: "1234567890abcdef"},
					},
				},
			},
			wantVersion: "1234567",
			wantCommit:  "1234567890abcdef",
		},
		{
			name: "ldflag commit 'unknown' falls through to vcs.revision",
			in: Inputs{
				LDFlagCommit: "unknown",
				BuildInfo: &debug.BuildInfo{
					Settings: []debug.BuildSetting{
						{Key: "vcs.revision", Value: "feedfacefeedface"},
					},
				},
			},
			wantCommit: "feedfacefeedface",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.in)
			if tt.wantVersion != "" && got.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tt.wantVersion)
			}
			if tt.wantCommit != "" && got.Commit != tt.wantCommit {
				t.Errorf("Commit = %q, want %q", got.Commit, tt.wantCommit)
			}
			if got.Dirty != tt.wantDirty {
				t.Errorf("Dirty = %v, want %v", got.Dirty, tt.wantDirty)
			}
		})
	}
}

func TestResolveBuildTime(t *testing.T) {
	got := Resolve(Inputs{
		BuildInfo: &debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.time", Value: "2026-05-12T10:00:00Z"},
			},
		},
	})
	if got.BuildTime != "2026-05-12T10:00:00Z" {
		t.Errorf("BuildTime = %q, want vcs.time value", got.BuildTime)
	}

	got = Resolve(Inputs{LDFlagBuildTime: "2026-01-01T00:00:00Z"})
	if got.BuildTime != "2026-01-01T00:00:00Z" {
		t.Errorf("BuildTime = %q, want ldflag value", got.BuildTime)
	}
}

func TestIsPseudoVersion(t *testing.T) {
	if !isPseudoVersion("v0.0.0-20260512143000-abc1234") {
		t.Error("v0.0.0 pseudo not detected")
	}
	if isPseudoVersion("v1.2.3") {
		t.Error("real tag misdetected as pseudo")
	}
	if isPseudoVersion("v0.0.0") {
		t.Error("plain v0.0.0 misdetected (needs the dashes)")
	}
}

func TestFromBuildtime_UsesEmbeddedInfo(t *testing.T) {
	// Smoke test against the actual build of the test binary — we don't
	// assert exact values (they depend on `go test` flags) but we do
	// assert that the call never panics and returns *something*.
	got := FromBuildtime("", "", "")
	if got.Version == "" {
		t.Error("Version is empty")
	}
	if !strings.Contains(got.Version, "dev") && got.Commit == "unknown" && !got.Dirty {
		// Acceptable: test binary may have stripped VCS info. Not a failure.
		t.Logf("test binary built without VCS info: %+v", got)
	}
}
