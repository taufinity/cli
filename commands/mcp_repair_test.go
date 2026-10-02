package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDesktop records what repair asked of Claude Desktop. answers are
// returned in order, one per dialog.
type fakeDesktop struct {
	running   bool
	quitWorks bool
	answers   []dialogAnswer
	messages  []string
	quits     int
	opens     int
	settings  int
	// failedStarts is how many Open calls do not actually start the app.
	failedStarts int
}

func (f *fakeDesktop) Running() bool { return f.running }
func (f *fakeDesktop) Quit() error {
	f.quits++
	if f.quitWorks {
		f.running = false
	}
	return nil
}
func (f *fakeDesktop) Open() error {
	f.opens++
	if f.failedStarts > 0 {
		f.failedStarts-- // macOS ignored the launch
		return nil
	}
	f.running = true
	return nil
}
func (f *fakeDesktop) OpenSettings() error { f.settings++; return nil }
func (f *fakeDesktop) Ask(message, _ string) (dialogAnswer, error) {
	f.messages = append(f.messages, message)
	if len(f.answers) == 0 {
		return answerLater, nil
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
}

const repairTestToggles = `{"v":3,"owners":{"acct":["local:studio:query_insights","local:studio:list_articles","local:other:foo"]}}`

// setupRepair writes a Claude Desktop config with one Taufinity bridge under a
// custom label plus a toggles file, and installs a fake desktop.
func setupRepair(t *testing.T, toggles string, answers ...dialogAnswer) (*fakeDesktop, string) {
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
	writeTogglesFile(t, togglesPath, toggles)

	fake := &fakeDesktop{running: true, quitWorks: true, answers: answers}
	prevDesktop, prevSupported, prevTimeout, prevGrace := claudeDesktop, desktopRepairSupported, repairQuitTimeout, repairReopenGrace
	claudeDesktop, desktopRepairSupported, repairQuitTimeout, repairReopenGrace = fake, true, 300*time.Millisecond, 0
	flagMCPRepairPrompt, flagMCPRepairWait = false, 0
	t.Cleanup(func() {
		claudeDesktop, desktopRepairSupported, repairQuitTimeout, repairReopenGrace = prevDesktop, prevSupported, prevTimeout, prevGrace
		flagMCPRepairPrompt, flagMCPRepairWait = false, 0
	})
	return fake, togglesPath
}

func writeTogglesFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runRepair(t *testing.T, args ...string) string {
	t.Helper()
	flagMCPRepairPrompt = false
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

func TestRepairMessage_IsTheAgreedText(t *testing.T) {
	got := repairMessage(switchedOff{servers: []string{"voorpositiviteit"}, total: 217})
	want := "Some Taufinity connections are turned off\n" +
		"To restore connections open Customize → Connectors → Yours →\n" +
		"\"voorpositiviteit\" → allow the tools → restart Claude Desktop."
	if got != want {
		t.Fatalf("message =\n%s\nwant\n%s", got, want)
	}
}

func TestMCPRepair_OpenSettingsThenRestart(t *testing.T) {
	fake, togglesPath := setupRepair(t, repairTestToggles, answerOK, answerOK)

	out := runRepair(t)

	if fake.settings != 1 || fake.quits != 1 || fake.opens != 1 {
		t.Errorf("settings=%d quits=%d opens=%d, want 1/1/1", fake.settings, fake.quits, fake.opens)
	}
	if len(fake.messages) != 2 || !strings.Contains(fake.messages[0], `"studio"`) || fake.messages[1] != restartMessage {
		t.Errorf("dialogs = %q", fake.messages)
	}
	if !strings.Contains(out, "To restore connections") {
		t.Errorf("output = %q, want the instructions in the terminal too", out)
	}
	if got, _ := os.ReadFile(togglesPath); string(got) != repairTestToggles {
		t.Errorf("repair wrote to Claude Desktop's toggles file: %s", got)
	}
}

func TestMCPRepair_OpenSettingsWithoutRestart(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerOK, answerLater)

	runRepair(t)

	if fake.settings != 1 || fake.quits != 0 || fake.opens != 0 {
		t.Errorf("settings=%d quits=%d opens=%d, want settings only", fake.settings, fake.quits, fake.opens)
	}
}

func TestMCPRepair_LaterDoesNothing(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerLater)

	runRepair(t)

	if fake.settings != 0 || fake.quits != 0 || len(fake.messages) != 1 {
		t.Errorf("settings=%d quits=%d dialogs=%d, want one dialog and nothing else", fake.settings, fake.quits, len(fake.messages))
	}
}

func TestMCPRepair_RestartGivesUpWhenDesktopWontQuit(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerOK, answerOK)
	fake.quitWorks = false

	rootCmd.SetArgs([]string{"mcp", "repair"})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetErr(&bytes.Buffer{})
	if err := rootCmd.Execute(); err == nil || !strings.Contains(err.Error(), "did not quit") {
		t.Fatalf("err = %v, want a did-not-quit error", err)
	}
	if fake.opens != 0 {
		t.Errorf("opens = %d, want 0 while the old instance still runs", fake.opens)
	}
}

func TestMCPRepair_NothingSwitchedOff(t *testing.T) {
	fake, _ := setupRepair(t, `{"v":3,"owners":{"acct":["local:other:foo"]}}`)

	out := runRepair(t)

	if len(fake.messages) != 0 || !strings.Contains(out, "Nothing to fix") {
		t.Errorf("dialogs=%d output=%q", len(fake.messages), out)
	}
}

func TestMCPRepair_PromptLaterSnoozesAWeek(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerLater)

	runRepair(t, "--prompt")
	runRepair(t, "--prompt")

	if len(fake.messages) != 1 {
		t.Errorf("dialogs = %d, want 1 (second run is snoozed)", len(fake.messages))
	}
	if left := time.Until(loadRepairState().SnoozedUntil); left < repairSnooze-time.Minute {
		t.Errorf("snoozed for %s, want about %s", left, repairSnooze)
	}
}

func TestMCPRepair_PromptUnansweredSnoozesShort(t *testing.T) {
	setupRepair(t, repairTestToggles, answerUnanswered)

	runRepair(t, "--prompt")

	if left := time.Until(loadRepairState().SnoozedUntil); left <= 0 || left > repairSnoozeUnanswered {
		t.Errorf("snoozed for %s, want at most %s", left, repairSnoozeUnanswered)
	}
}

func TestMCPRepair_PromptRespectsToolsLeftOffOnPurpose(t *testing.T) {
	fake, togglesPath := setupRepair(t, repairTestToggles, answerOK, answerOK)

	runRepair(t, "--prompt") // opens settings, restarts

	// After the restart the user left list_articles off on purpose.
	writeTogglesFile(t, togglesPath, `{"v":3,"owners":{"acct":["local:studio:list_articles"]}}`)
	runRepair(t, "--prompt") // within the acknowledgement window: remembered
	runRepair(t, "--prompt") // later start: nothing new, so no dialog

	if len(fake.messages) != 2 {
		t.Fatalf("dialogs = %d, want only the first run's two", len(fake.messages))
	}

	// A tool switched off later is new, so the dialog comes back.
	writeTogglesFile(t, togglesPath, `{"v":3,"owners":{"acct":["local:studio:list_articles","local:studio:query_insights"]}}`)
	fake.answers = []dialogAnswer{answerLater}
	runRepair(t, "--prompt")
	if len(fake.messages) != 3 {
		t.Errorf("dialogs = %d, want the dialog back for a newly switched-off tool", len(fake.messages))
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

	if len(fake.messages) != 0 {
		t.Errorf("dialogs = %d, want 0 while another prompt holds the lock", len(fake.messages))
	}
}

func TestParseDialogAnswer(t *testing.T) {
	cases := map[string]dialogAnswer{
		"button returned:Open settings, gave up:false\n":          answerOK,
		"button returned:, gave up:true\n":                        answerUnanswered,
		"button returned:Later, gave up:false\n":                  answerLater,
		"button returned:Open settings and more, gave up:false\n": answerLater,
		"": answerLater,
	}
	for in, want := range cases {
		if got := parseDialogAnswer(in, "Open settings"); got != want {
			t.Errorf("parseDialogAnswer(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestMCPRepair_PromptUnchangedAfterSettingsAsksAgainLater(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerOK, answerOK)

	runRepair(t, "--prompt") // opens settings, restarts
	runRepair(t, "--prompt") // nothing was switched on: not a choice

	st := loadRepairState()
	if len(st.Acknowledged) != 0 {
		t.Errorf("acknowledged %v although nothing changed in Connectors", st.Acknowledged)
	}
	if left := time.Until(st.SnoozedUntil); left <= 0 || left > repairSnoozeUnanswered {
		t.Errorf("snoozed for %s, want a short pause of at most %s", left, repairSnoozeUnanswered)
	}
	if len(fake.messages) != 2 {
		t.Errorf("dialogs = %d, want no new dialog during the pause", len(fake.messages))
	}
}

func TestMCPRepair_ManualRunDefersToAnOpenDialog(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerOK, answerOK)
	release, ok := acquireRepairLock()
	if !ok {
		t.Fatal("could not take the lock")
	}
	defer release()

	out := runRepair(t)

	if len(fake.messages) != 0 || fake.settings != 0 {
		t.Errorf("dialogs=%d settings=%d, want none while another dialog is open", len(fake.messages), fake.settings)
	}
	if !strings.Contains(out, "already open") || !strings.Contains(out, "To restore connections") {
		t.Errorf("output = %q, want the instructions plus a note about the open dialog", out)
	}
}

func TestMCPRepair_RestartRetriesWhenTheFirstLaunchIsIgnored(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerOK, answerOK)
	fake.failedStarts = 1

	runRepair(t)

	if fake.opens != 2 || !fake.running {
		t.Errorf("opens=%d running=%v, want a second launch that starts the app", fake.opens, fake.running)
	}
}

func TestMCPRepair_RestartReportsWhenDesktopNeverComesBack(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerOK, answerOK)
	fake.failedStarts = 2

	rootCmd.SetArgs([]string{"mcp", "repair"})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetErr(&bytes.Buffer{})
	if err := rootCmd.Execute(); err == nil || !strings.Contains(err.Error(), "did not start again") {
		t.Fatalf("err = %v, want a did-not-start error", err)
	}
}
