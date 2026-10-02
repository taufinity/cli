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

// maybeStartRepairPrompt runs at bridge startup. When Claude Desktop has
// switched off Taufinity tools it starts `taufinity mcp repair --prompt` as a
// detached process: the bridge is a child of Claude Desktop, and the prompt
// has to outlive it because it can restart the app. All gating (snooze,
// acknowledged choices, one dialog per burst of bridges) lives in the prompt
// command, which re-reads the toggles after the delay.
func maybeStartRepairPrompt(stderr io.Writer) {
	if runtime.GOOS != "darwin" || os.Getenv("TAUFINITY_NO_REPAIR_PROMPT") != "" {
		return
	}
	cfgPath, err := claudeDesktopPath()
	if err != nil {
		return
	}
	off, err := switchedOffTools(cfgPath)
	if err != nil || off == 0 {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "mcp", "repair", "--prompt", "--wait", repairPromptDelay.String())
	detach(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "[taufinity mcp stdio] %d tools are switched off in Claude Desktop; could not start the repair prompt: %v\n", off, err)
		return
	}
	fmt.Fprintf(stderr, "[taufinity mcp stdio] %d tools are switched off in Claude Desktop; repair prompt started\n", off)
	_ = cmd.Process.Release()
}
