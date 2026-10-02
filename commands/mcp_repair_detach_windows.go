//go:build windows

package commands

import "os/exec"

// detach is a no-op on Windows: the repair prompt only runs on macOS.
func detach(*exec.Cmd) {}
