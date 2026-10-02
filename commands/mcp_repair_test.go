package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDesktop records what repair asked of Claude Desktop. answers are
// returned in order, one per dialog.
type fakeDesktop struct {
	answers  []dialogAnswer
	messages []string
	settings int
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

// repairTestCatalog marks query_insights and list_articles read-only and
// delete_article not.
const repairTestCatalog = `{"tools":[
	{"name":"query_insights","annotations":{"readOnlyHint":true}},
	{"name":"list_articles","annotations":{"readOnlyHint":true}},
	{"name":"delete_article","annotations":{"readOnlyHint":false}}]}`

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
	// The bridge would have recorded this from Studio's tools/list.
	recordToolCatalog(json.RawMessage(repairTestCatalog))

	fake := &fakeDesktop{answers: answers}
	prevDesktop, prevSupported := claudeDesktop, desktopRepairSupported
	claudeDesktop, desktopRepairSupported = fake, true
	flagMCPRepairPrompt, flagMCPRepairWait = false, 0
	t.Cleanup(func() {
		claudeDesktop, desktopRepairSupported = prevDesktop, prevSupported
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

func TestMCPRepair_OpenSettings(t *testing.T) {
	fake, togglesPath := setupRepair(t, repairTestToggles, answerOK)

	out := runRepair(t)

	if fake.settings != 1 {
		t.Errorf("settings = %d, want the Connectors settings opened once", fake.settings)
	}
	if len(fake.messages) != 1 || !strings.Contains(fake.messages[0], `"studio"`) {
		t.Errorf("dialogs = %q, want one dialog naming the server", fake.messages)
	}
	if !strings.Contains(out, "To restore connections") {
		t.Errorf("output = %q, want the instructions in the terminal too", out)
	}
	if got, _ := os.ReadFile(togglesPath); string(got) != repairTestToggles {
		t.Errorf("repair wrote to Claude Desktop's toggles file: %s", got)
	}
}

func TestMCPRepair_LaterDoesNothing(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerLater)

	runRepair(t)

	if fake.settings != 0 || len(fake.messages) != 1 {
		t.Errorf("settings=%d dialogs=%d, want one dialog and nothing else", fake.settings, len(fake.messages))
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
	fake, togglesPath := setupRepair(t, repairTestToggles, answerOK)

	runRepair(t, "--prompt") // opens settings; the user restarts Desktop

	// After the restart the user left list_articles off on purpose.
	writeTogglesFile(t, togglesPath, `{"v":3,"owners":{"acct":["local:studio:list_articles"]}}`)
	runRepair(t, "--prompt") // within the acknowledgement window: remembered
	runRepair(t, "--prompt") // later start: nothing new, so no dialog

	if len(fake.messages) != 1 {
		t.Fatalf("dialogs = %d, want only the first run's", len(fake.messages))
	}

	// A tool switched off later is new, so the dialog comes back.
	writeTogglesFile(t, togglesPath, `{"v":3,"owners":{"acct":["local:studio:list_articles","local:studio:query_insights"]}}`)
	fake.answers = []dialogAnswer{answerLater}
	runRepair(t, "--prompt")
	if len(fake.messages) != 2 {
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
	fake, _ := setupRepair(t, repairTestToggles, answerOK)

	runRepair(t, "--prompt") // opens settings; the user restarts Desktop
	runRepair(t, "--prompt") // nothing was switched on: not a choice

	st := loadRepairState()
	if len(st.Acknowledged) != 0 {
		t.Errorf("acknowledged %v although nothing changed in Connectors", st.Acknowledged)
	}
	if left := time.Until(st.SnoozedUntil); left <= 0 || left > repairSnoozeUnanswered {
		t.Errorf("snoozed for %s, want a short pause of at most %s", left, repairSnoozeUnanswered)
	}
	if len(fake.messages) != 1 {
		t.Errorf("dialogs = %d, want no new dialog during the pause", len(fake.messages))
	}
}

func TestMCPRepair_ManualRunDefersToAnOpenDialog(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles, answerOK)
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

func TestMCPRepair_PromptIgnoresWriteToolsLeftOff(t *testing.T) {
	fake, _ := setupRepair(t, `{"v":3,"owners":{"acct":["local:studio:delete_article"]}}`)

	runRepair(t, "--prompt")

	if len(fake.messages) != 0 {
		t.Errorf("dialogs = %d, want none: a write tool left off is a choice", len(fake.messages))
	}
}

func TestMCPRepair_PromptStaysQuietWithoutCatalog(t *testing.T) {
	fake, _ := setupRepair(t, repairTestToggles)
	if err := os.Remove(toolCatalogPath()); err != nil {
		t.Fatal(err)
	}

	runRepair(t, "--prompt")

	if len(fake.messages) != 0 {
		t.Errorf("dialogs = %d, want none without a catalog to tell read from write tools", len(fake.messages))
	}
}

func TestMCPRepair_ManualWithoutCatalogShowsEverything(t *testing.T) {
	fake, _ := setupRepair(t, `{"v":3,"owners":{"acct":["local:studio:delete_article"]}}`, answerLater)
	if err := os.Remove(toolCatalogPath()); err != nil {
		t.Fatal(err)
	}

	runRepair(t)

	if len(fake.messages) != 1 {
		t.Errorf("dialogs = %d, want the dialog when asked by hand", len(fake.messages))
	}
}

func TestRecordToolCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	recordToolCatalog(json.RawMessage(`{"tools":[
		{"name":"query_insights","annotations":{"readOnlyHint":true}},
		{"name":"update_article","annotations":{"readOnlyHint":false}},
		{"name":"no_annotations"}]}`))
	got := loadReadOnlyTools()
	if len(got) != 1 || !got["query_insights"] {
		t.Fatalf("read-only = %v, want only query_insights", got)
	}

	// A partial page must not replace the full list.
	recordToolCatalog(json.RawMessage(`{"tools":[{"name":"x","annotations":{"readOnlyHint":true}}],"nextCursor":"2"}`))
	if got := loadReadOnlyTools(); !got["query_insights"] || got["x"] {
		t.Fatalf("read-only after a paged result = %v, want unchanged", got)
	}
}

func TestIsUserCancel(t *testing.T) {
	if !isUserCancel("0:182: execution error: User canceled. (-128)\n") {
		t.Error("cancel button not recognised")
	}
	if isUserCancel("execution error: Not authorized to send Apple events to System Events. (-1743)") {
		t.Error("a permission error must not count as Later")
	}
}

func TestRecordToolCatalog_ConcurrentWritersLeaveAValidCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			recordToolCatalog(json.RawMessage(`{"tools":[{"name":"query_insights","annotations":{"readOnlyHint":true}}]}`))
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if got := loadReadOnlyTools(); !got["query_insights"] {
		t.Fatalf("read-only = %v, want query_insights after concurrent writes", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(toolCatalogPath()), "mcp-tools.*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}
