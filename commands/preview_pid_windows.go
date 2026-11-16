//go:build windows

package commands

import "os"

// pidAlive reports whether a process with this id exists (FindProcess opens a
// handle on Windows and fails for a missing process).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	p.Release()
	return true
}
