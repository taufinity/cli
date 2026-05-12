package commands

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/buildinfo"
)

// Build-time variables (set via ldflags). Kept as package-level vars so the
// Makefile's -X linker flags still work; buildinfo.FromBuildtime falls back
// to debug.BuildInfo when they're left at their defaults.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Long:  `Print the version, git commit hash, and build time.`,
	Run:   runVersion,
	Annotations: map[string]string{
		// `version` is invoked by the update smoke test and by users
		// doing a quick sanity check; the staleness warning would clutter
		// both. The update command surfaces version info on its own.
		"suppress-update-warning": "true",
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

func runVersion(cmd *cobra.Command, args []string) {
	info := buildinfo.FromBuildtime(Version, GitCommit, BuildTime)
	fmt.Printf("taufinity %s\n", info.Version)
	fmt.Printf("  commit:  %s\n", info.Commit)
	fmt.Printf("  built:   %s\n", info.BuildTime)
	fmt.Printf("  go:      %s\n", runtime.Version())
	fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}
