package main

import (
	"fmt"
	"os"

	"github.com/taufinity/cli/commands"
	"github.com/taufinity/cli/internal/telemetry"
)

func main() {
	telemetry.Init(commands.Version, commands.GitCommit)
	defer telemetry.Flush()

	if err := commands.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
