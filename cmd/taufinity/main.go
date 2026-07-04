package main

import (
	"fmt"
	"os"
	"time"

	"github.com/taufinity/cli/commands"
	"github.com/taufinity/cli/internal/pixl"
	"github.com/taufinity/cli/internal/telemetry"
	"github.com/taufinity/cli/internal/terms"
)

func main() {
	telemetry.Init(commands.Version, commands.GitCommit)
	pixl.Init(commands.Version)
	defer telemetry.Flush()
	defer pixl.Flush(2 * time.Second)

	terms.ShowOnce(os.Stderr)

	if err := commands.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
