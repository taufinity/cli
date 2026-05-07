package commands

import "testing"

func TestMCPInstallCommand_RegisteredAlongsideLogin(t *testing.T) {
	for _, want := range []string{"install", "login", "uninstall", "print"} {
		cmd, _, err := rootCmd.Find([]string{"mcp", want})
		if err != nil {
			t.Fatalf("mcp %s not registered: %v", want, err)
		}
		if cmd.Name() != want {
			t.Fatalf("mcp %s: got command %q, want %q", want, cmd.Name(), want)
		}
	}
}
