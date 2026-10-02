package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/taufinity/cli/internal/config"
	"github.com/taufinity/cli/internal/desktopconfig"
	"github.com/taufinity/cli/internal/telemetry"
)

const (
	// repairSnooze is how long "Later" keeps the dialog away.
	repairSnooze = 7 * 24 * time.Hour

	// repairSnoozeUnanswered is the shorter pause after the dialog timed out
	// unanswered. Nobody said "Later"; they were probably away from the Mac.
	repairSnoozeUnanswered = 4 * time.Hour

	// repairAckWindow: tools still switched off this soon after the user
	// opened Connectors are the ones they chose to leave off. They are
	// remembered and not asked about again.
	repairAckWindow = time.Hour

	// repairLockStale: a lock older than this belongs to a dialog that died.
	repairLockStale = 20 * time.Minute

	// connectorsSettingsURL opens Claude Desktop on its Connectors settings.
	connectorsSettingsURL = "claude://claude.ai/settings/connectors"
)

var (
	// repairQuitTimeout caps how long we wait for Claude Desktop to exit
	// before reopening it. A var so tests can shorten it.
	repairQuitTimeout = 30 * time.Second

	// desktopRepairSupported gates the dialogs and the restart, which use
	// macOS tooling. A var so tests run the same paths on Linux CI.
	desktopRepairSupported = runtime.GOOS == "darwin"

	flagMCPRepairPrompt bool
	flagMCPRepairWait   time.Duration
)

var mcpRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Help switch on Taufinity tools that Claude Desktop has switched off",
	Long: `Repair checks whether Claude Desktop has switched off tools of your
Taufinity connection. Those can only be switched on in Claude Desktop itself,
under Settings → Connectors, and take effect after a restart.

Repair shows what is switched off, opens the Connectors settings for you, and
then offers to restart Claude Desktop.

Flags:
  --prompt  Run unattended: silent when nothing is switched off, one dialog at
            a time, "Later" keeps it away for seven days (four hours when the
            dialog went unanswered). Tools still off within an hour of opening
            the settings count as a deliberate choice and are not asked about
            again. Used by the bridge at startup and by the daily check.`,
	RunE: runMCPRepair,
}

func init() {
	mcpCmd.AddCommand(mcpRepairCmd)
	mcpRepairCmd.Flags().BoolVar(&flagMCPRepairPrompt, "prompt", false, "Run unattended: silent when nothing is switched off, snoozable")
	mcpRepairCmd.Flags().DurationVar(&flagMCPRepairWait, "wait", 0, "Wait this long before checking")
	_ = mcpRepairCmd.Flags().MarkHidden("wait")
}

// desktopApp is the part of Claude Desktop repair talks to. Swapped in tests.
type desktopApp interface {
	Running() bool
	Quit() error
	Open() error
	OpenSettings() error
	// Ask shows message with a "Later" button and an okLabel button.
	Ask(message, okLabel string) (dialogAnswer, error)
}

// dialogAnswer is what came back from a dialog.
type dialogAnswer int

const (
	answerLater dialogAnswer = iota
	answerOK
	answerUnanswered // the dialog timed out
)

var claudeDesktop desktopApp = macClaudeDesktop{}

// switchedOff is what Claude Desktop has switched off, per Taufinity server.
type switchedOff struct {
	servers []string            // Taufinity servers with tools switched off
	tools   map[string][]string // server → switched-off tool names
	total   int
}

// keys returns "server:tool" for every switched-off tool.
func (s switchedOff) keys() []string {
	var keys []string
	for _, srv := range s.servers {
		for _, t := range s.tools[srv] {
			keys = append(keys, srv+":"+t)
		}
	}
	return keys
}

func findSwitchedOff(cfgPath string) (switchedOff, error) {
	out := switchedOff{tools: map[string][]string{}}
	servers, err := desktopconfig.BridgeServers(cfgPath)
	if err != nil {
		return out, err
	}
	for _, srv := range servers {
		tools, err := desktopconfig.SwitchedOffTools(cfgPath, srv)
		if err != nil {
			return out, err
		}
		if len(tools) > 0 {
			out.servers = append(out.servers, srv)
			out.tools[srv] = tools
			out.total += len(tools)
		}
	}
	return out, nil
}

// switchedOffTools is the total, for callers that only need to know whether
// anything is switched off.
func switchedOffTools(cfgPath string) (int, error) {
	off, err := findSwitchedOff(cfgPath)
	return off.total, err
}

// repairMessage is the text of the first dialog, and of the terminal output.
func repairMessage(off switchedOff) string {
	quoted := make([]string, len(off.servers))
	for i, s := range off.servers {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("Claude can't fully use your Taufinity connection,\n"+
		"because %d of its features are switched off.\n"+
		"To fix it: open Claude Desktop Settings → Connectors →\n"+
		"%s, allow the tools, then restart Claude Desktop.",
		off.total, strings.Join(quoted, " and "))
}

const restartMessage = "After allowing the tools, restart Claude Desktop to load them.\n\nRestart Claude Desktop now?"

func runMCPRepair(cmd *cobra.Command, _ []string) error {
	cfgPath, err := claudeDesktopPath()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if flagMCPRepairWait > 0 {
		time.Sleep(flagMCPRepairWait)
	}

	if !flagMCPRepairPrompt {
		off, err := findSwitchedOff(cfgPath)
		if err != nil {
			return err
		}
		if off.total == 0 {
			fmt.Fprintln(out, "Nothing to fix: all features of your Taufinity connection are switched on in Claude Desktop.")
			return nil
		}
		fmt.Fprintln(out, repairMessage(off))
		if !desktopRepairSupported {
			return nil
		}
		_, err = guideRepair(out, off)
		return err
	}

	// --prompt runs unattended (bridge startup, daily agent). Several bridges
	// start in the same second, so only the first one may show a dialog.
	release, ok := acquireRepairLock()
	if !ok {
		return nil
	}
	defer release()

	off, err := findSwitchedOff(cfgPath)
	if err != nil || off.total == 0 || !desktopRepairSupported {
		return err
	}

	state := loadRepairState()
	now := time.Now()
	if !state.SettingsOpenedAt.IsZero() && now.Sub(state.SettingsOpenedAt) < repairAckWindow {
		// The user was just in Connectors. What is still off is their choice.
		state.Acknowledged = off.keys()
		state.SettingsOpenedAt = time.Time{}
		telemetry.Report(telemetry.Event{
			EventType:    "mcp.tool_toggles_acknowledged",
			ErrorCode:    "left_off_after_settings",
			ErrorMessage: fmt.Sprintf("%d tools left switched off after opening Connectors", off.total),
		})
		return saveRepairState(state)
	}
	if !hasUnacknowledged(off.keys(), state.Acknowledged) || now.Before(state.SnoozedUntil) {
		return nil
	}

	answer, err := guideRepair(out, off)
	if err != nil {
		return err
	}
	state = loadRepairState() // guideRepair may have recorded SettingsOpenedAt
	switch answer {
	case answerLater:
		state.SnoozedUntil = now.Add(repairSnooze)
	case answerUnanswered:
		state.SnoozedUntil = now.Add(repairSnoozeUnanswered)
	default:
		return nil
	}
	return saveRepairState(state)
}

// guideRepair shows the switched-off dialog. On "Open settings" it opens
// Connectors and then offers a restart, so the change takes effect. It
// returns the answer to the first dialog.
func guideRepair(out io.Writer, off switchedOff) (dialogAnswer, error) {
	telemetry.Report(telemetry.Event{
		EventType:    "mcp.tool_toggles_switched_off",
		ErrorCode:    "repair_prompt_shown",
		ErrorMessage: fmt.Sprintf("%d tools switched off across %d server(s)", off.total, len(off.servers)),
	})
	answer, err := claudeDesktop.Ask(repairMessage(off), "Open settings")
	if err != nil || answer != answerOK {
		return answer, err
	}

	if err := claudeDesktop.OpenSettings(); err != nil {
		fmt.Fprintf(out, "Could not open the settings (%v); open them yourself as described above.\n", err)
	}
	state := loadRepairState()
	state.SettingsOpenedAt = time.Now()
	_ = saveRepairState(state)

	restart, err := claudeDesktop.Ask(restartMessage, "Restart")
	if err != nil || restart != answerOK {
		return answerOK, err
	}
	return answerOK, restartClaudeDesktop(out)
}

// restartClaudeDesktop quits Claude Desktop, waits for it to exit, and opens
// it again. If it does not quit in time it is left as it is.
func restartClaudeDesktop(out io.Writer) error {
	if claudeDesktop.Running() {
		fmt.Fprintln(out, "Restarting Claude Desktop.")
		if err := claudeDesktop.Quit(); err != nil {
			return fmt.Errorf("quit Claude Desktop: %w", err)
		}
		deadline := time.Now().Add(repairQuitTimeout)
		for claudeDesktop.Running() {
			if time.Now().After(deadline) {
				return errors.New("Claude Desktop did not quit in time; restart it yourself")
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	if err := claudeDesktop.Open(); err != nil {
		return fmt.Errorf("open Claude Desktop: %w", err)
	}
	return nil
}

func hasUnacknowledged(keys, acknowledged []string) bool {
	ack := make(map[string]bool, len(acknowledged))
	for _, k := range acknowledged {
		ack[k] = true
	}
	for _, k := range keys {
		if !ack[k] {
			return true
		}
	}
	return false
}

// repairState persists the dialog's snooze and what the user chose to leave
// off, so the prompt neither nags nor loops across bridge restarts.
type repairState struct {
	SnoozedUntil     time.Time `json:"snoozed_until"`
	SettingsOpenedAt time.Time `json:"settings_opened_at"`
	Acknowledged     []string  `json:"acknowledged,omitempty"`
}

func repairStatePath() string { return filepath.Join(config.Dir(), "mcp-repair.json") }

func loadRepairState() repairState {
	var s repairState
	if raw, err := os.ReadFile(repairStatePath()); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func saveRepairState(s repairState) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(repairStatePath(), raw, 0o600)
}

// acquireRepairLock takes a lock file with O_EXCL, which works on every OS
// the CLI ships for. A lock older than repairLockStale is taken over.
func acquireRepairLock() (release func(), ok bool) {
	path := filepath.Join(config.Dir(), "mcp-repair.lock")
	_ = os.MkdirAll(config.Dir(), 0o700)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, true
		}
		fi, statErr := os.Stat(path)
		if statErr != nil || time.Since(fi.ModTime()) < repairLockStale {
			return nil, false
		}
		os.Remove(path)
	}
	return nil, false
}

// macClaudeDesktop drives the Claude Desktop app through macOS tooling.
type macClaudeDesktop struct{}

func (macClaudeDesktop) Running() bool {
	return exec.Command("pgrep", "-x", "Claude").Run() == nil
}

func (macClaudeDesktop) Quit() error {
	return exec.Command("osascript", "-e", `tell application "Claude" to quit`).Run()
}

func (macClaudeDesktop) Open() error {
	return exec.Command("open", "-a", "Claude").Run()
}

func (macClaudeDesktop) OpenSettings() error {
	return exec.Command("open", connectorsSettingsURL).Run()
}

// dialogScript takes the message and the button label as arguments, so no
// text ever has to be escaped into AppleScript source.
const dialogScript = `on run argv
	display dialog (item 1 of argv) with title "Taufinity" buttons {"Later", (item 2 of argv)} default button (item 2 of argv) cancel button "Later" with icon caution giving up after 900
end run`

func (macClaudeDesktop) Ask(message, okLabel string) (dialogAnswer, error) {
	raw, err := exec.Command("osascript", "-e", dialogScript, message, okLabel).Output()
	// "Later" is the cancel button, so osascript exits non-zero for it; that
	// is an answer, not a failure.
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return answerLater, nil
		}
		return answerLater, err
	}
	return parseDialogAnswer(string(raw), okLabel), nil
}

// parseDialogAnswer reads osascript's record output, e.g.
// "button returned:Open settings, gave up:false" or "button returned:, gave up:true".
func parseDialogAnswer(out, okLabel string) dialogAnswer {
	switch {
	case strings.Contains(out, "gave up:true"):
		return answerUnanswered
	case strings.Contains(out, "button returned:"+okLabel+","),
		strings.TrimSpace(out) == "button returned:"+okLabel:
		return answerOK
	default:
		return answerLater
	}
}
