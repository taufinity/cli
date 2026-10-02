package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDesktop records what repair asked of Claude Desktop, and checks that
// the toggles file is only changed while the app is closed.
type fakeDesktop struct {
	t            *testing.T
	togglesPath  string
	running      bool
	quitWorks    bool
	answer       bool
	asked        int
	quits, opens int
}

func (f *fakeDesktop) Running() bool { return f.running }

func (f *fakeDesktop) Quit() error {
	f.quits++
	if f.quitWorks {
		raw, _ := os.ReadFile(f.togglesPath)
		if !strings.Contains(string(raw), "local:studio:") {
			f.t.Error("toggles were changed before Claude Desktop quit")
		}
		f.running = false
	}
	return nil
}

func (f *fakeDesktop) Open() error {
	f.opens++
	raw, _ := os.ReadFile(f.togglesPath)
	if strings.Contains(string(raw), "local:studio:") {
		f.t.Error("Claude Desktop reopened before the toggles were cleared")
	}
	return nil
}

func (f *fakeDesktop) Ask(string) (bool, error) {
	f.asked++
	return f.answer, nil
}

const repairTestToggles = `{"v":3,"owners":{"acct":["local:studio:query_insights","local:studio:list_articles","local:other:foo"]}}`

// setupRepair writes a Claude Desktop config with one Taufinity bridge under a
// custom label, plus a toggles file, and installs a fake desktop.
func setupRepair(t *testing.T, toggles string) (*fakeDesktop, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "claude_desktop_config.json")
	t.Setenv("TAUFINITY_DESKTOP_CONFIG", cfgPath)
	cfg := `{"mcpServers":{
		"studio":{"command":"/usr/local/bin/taufinity","args":["--org","3","mcp","stdio"]},
		"other":{"command":"npx","args":["other-server"]}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	togglesPath := filepath.Join(dir, "mcp-user-tool-toggles.json")
	if err := os.WriteFile(togglesPath, []byte(toggles), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := &fakeDesktop{t: t, togglesPath: togglesPath, running: true, quitWorks: true}
	prevDesktop, prevSupported, prevTimeout := claudeDesktop, desktopRepairSupported, repairQuitTimeout
	claudeDesktop, desktopRepairSupported, repairQuitTimeout = fake, true, 300*time.Millisecond
	flagMCPRepairPrompt, flagMCPRepairNoRestart = false, false
	t.Cleanup(func() {
		claudeDesktop, desktopRepairSupported, repairQuitTimeout = prevDesktop, prevSupported, prevTimeout
		flagMCPRepairPrompt, flagMCPRepairNoRestart = false, false
	})
	return fake, togglesPath
}

func runRepair(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })
	rootCmd.SetArgs(append([]string{"mcp", "repair"}, args...))
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v (output %q)", err, out.String())
	}
	return out.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestMCPRepair_QuitsClearsAndReopens(t *testing.T) {
	fake, togglesPath := setupRepair(t, repairTestToggles)

	out := runRepair(t)

	if fake.quits != 1 || fake.opens != 1 {
		t.Errorf("quits=%d opens=%d, want 1 and 1", fake.quits, fake.opens)
	}
	got := readFile(t, togglesPath)
	if strings.Contains(got, "local:studio:") || !strings.Contains(got, "local:other:foo") {
		t.Errorf("toggles = %s, want only the other server's entry left", got)
	}
	if !strings.Contains(out, "Switched on 2 feature(s)") {
		t.Errorf("output = %q", out)
	}
}

func TestMCPRepair_LeavesFileAloneWhenDesktopWontQuit(t *testing.T) {
	fake, togglesPath := setupRepair(t, repairTestToggles)
	fake.quitWorks = false

	rootCmd.SetArgs([]string{"mcp", "repair"})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetErr(&bytes.Buffer{})
	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "did not quit") {
		t.Fatalf("err = %v, want a did-not-quit error", err)
	}
	if got := readFile(t, togglesPath); got != repairTestToggles {
		t.Errorf("toggles changed although Claude Desktop kept running: %s", got)
	}
	if fake.opens != 0 {
		t.Errorf("opens = %d, want 0", fake.opens)
	}
}

func TestMCPRepair_PromptLaterSnoozes(t *testing.T) {
	fake, togglesPath := setupRepair(t, repairTestToggles)
	fake.answer = false

	runRepair(t, "--prompt")
	runRepair(t, "--prompt")

	if fake.asked != 1 {
		t.Errorf("asked = %d, want 1 (second run is snoozed)", fake.asked)
	}
	if fake.quits != 0 {
		t.Errorf("quits = %d, want 0 after Later", fake.quits)
	}
	if got := readFile(t, togglesPath); got != repairTestToggles {
		t.Errorf("toggles changed after Later: %s", got)
	}
}

func TestMCPRepair_PromptRepairNow(t *testing.T) {
	fake, togglesPath := setupRepair(t, repairTestToggles)
	fake.answer = true

	runRepair(t, "--prompt")

	if fake.asked != 1 || fake.quits != 1 || fake.opens != 1 {
		t.Errorf("asked=%d quits=%d opens=%d, want 1/1/1", fake.asked, fake.quits, fake.opens)
	}
	if strings.Contains(readFile(t, togglesPath), "local:studio:") {
		t.Error("toggles not cleared")
	}
}

func TestMCPRepair_PromptSilentWhenNothingOff(t *testing.T) {
	fake, _ := setupRepair(t, `{"v":3,"owners":{"acct":["local:other:foo"]}}`)

	runRepair(t, "--prompt")

	if fake.asked != 0 || fake.quits != 0 {
		t.Errorf("asked=%d quits=%d, want no dialog and no restart", fake.asked, fake.quits)
	}
}

func TestMCPRepair_PromptDoesNotLoopAfterRecentRepair(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles)
	if err := saveRepairState(repairState{LastRepairAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	runRepair(t, "--prompt")

	if fake.asked != 0 {
		t.Errorf("asked = %d, want 0: Desktop restored the toggles right after a repair", fake.asked)
	}
}

func TestMCPRepair_PromptOnlyOneDialogAtATime(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles)
	release, ok := acquireRepairLock()
	if !ok {
		t.Fatal("could not take the lock")
	}
	defer release()

	runRepair(t, "--prompt")

	if fake.asked != 0 {
		t.Errorf("asked = %d, want 0 while another prompt holds the lock", fake.asked)
	}
}
