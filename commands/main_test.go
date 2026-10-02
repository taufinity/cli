package commands

import (
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory for the whole package, so no
// test can write into the developer's real ~/.config/taufinity. The bridge,
// for one, records a tool catalog there whenever a test forwards tools/list.
// Tests that need their own HOME still set it with t.Setenv.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "taufinity-commands-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
