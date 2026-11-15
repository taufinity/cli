//go:build !windows

package commands

import (
	"errors"
	"syscall"
)

// pidAlive reports whether a process with this id exists.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
