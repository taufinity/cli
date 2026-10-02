package commands

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// repairPromptDelay lets Claude Desktop finish starting before the prompt
// reads its toggles: Desktop rewrites that file after it launches the bridges.
const repairPromptDelay = 90 * time.Second

// maybeStartRepairPrompt runs at bridge startup and starts
// `taufinity mcp repair --prompt` as a detached process: the bridge is a
// child of Claude Desktop, and the prompt has to outlive it because it can
// restart the app.
//
// It deliberately does not look at the toggles itself. At this moment Claude
// Desktop has not rewritten them yet, so a stale "nothing switched off" would
// skip the prompt for good. The prompt reads them after the delay and decides
// (snooze, acknowledged choices, one dialog per burst of bridges); when
// nothing is switched off it exits silently.
func maybeStartRepairPrompt(stderr io.Writer) {
	if runtime.GOOS != "darwin" || os.Getenv("TAUFINITY_NO_REPAIR_PROMPT") != "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "mcp", "repair", "--prompt", "--wait", repairPromptDelay.String())
	detach(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "[taufinity mcp stdio] could not start the switched-off tools check: %v\n", err)
		return
	}
	_ = cmd.Process.Release()
}
