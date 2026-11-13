package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/taufinity/cli/commands"
	"github.com/taufinity/cli/internal/cfaccess"
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
		// A typed exit code (taufinity review) is honoured. Code 1 without an
		// error means the command already printed its result; an error whose
		// text is already on stderr is not repeated.
		var ee *commands.ExitError
		if errors.As(err, &ee) {
			if ee.Err != nil && !ee.Printed {
				werr := ee.Err
				if be := cfaccess.AsBlocked(werr); be != nil {
					werr = be
				}
				fmt.Fprintln(os.Stderr, "Error:", werr)
			}
			os.Exit(ee.Code)
		}
		// A Cloudflare Access block carries its own instructions; print those,
		// not the transport wrapping ("request failed: Post \"https://...\": ...").
		if be := cfaccess.AsBlocked(err); be != nil {
			err = be
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
