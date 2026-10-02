package commands

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

// maybeStartRepairPrompt runs at bridge startup. When Claude Desktop has
// switched off Taufinity tools it starts `taufinity mcp repair --prompt` as a
// detached process: the bridge is a child of Claude Desktop, and the repair
// has to outlive it because it quits and reopens the app. All gating (snooze,
// loop guard, one dialog per burst of bridges) lives in the prompt command.
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
	cmd := exec.Command(exe, "mcp", "repair", "--prompt")
	detach(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "[taufinity mcp stdio] %d tools are switched off in Claude Desktop; could not start the repair prompt: %v\n", off, err)
		return
	}
	fmt.Fprintf(stderr, "[taufinity mcp stdio] %d tools are switched off in Claude Desktop; repair prompt started\n", off)
	_ = cmd.Process.Release()
}
