//go:build !windows

package commands

import (
	"os/exec"
	"syscall"
)

// detach puts cmd in its own session, so Claude Desktop killing the bridge's
// process group on quit does not take the repair down with it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
