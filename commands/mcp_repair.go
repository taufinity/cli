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

	// repairAckWindow: when the user switched tools on within this time after
	// opening Connectors, the ones still off are what they chose to leave off.
	// Those are remembered and not asked about again. If nothing changed they
	// probably did not find the switch, so the dialog comes back later.
	repairAckWindow = time.Hour

	// repairLockStale: a lock older than this belongs to a dialog that died.
	// Above the longest a live prompt can hold it: one dialog of 15 minutes.
	repairLockStale = 20 * time.Minute

	// connectorsSettingsURL opens Claude Desktop on Customize → Connectors.
	// The older /settings/connectors only says connectors moved to Customize.
	// It lands on the Discover tab (so does ?directory=false); there is no
	// link to Yours, so the dialog text names that tab.
	connectorsSettingsURL = "claude://claude.ai/customize/connectors"
)

var (
	// desktopRepairSupported gates the dialog and the settings link, which use
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
under Customize → Connectors → Yours, and take effect after a restart.

Repair shows what is switched off and opens the Connectors settings for you.
Restarting Claude Desktop afterwards is up to you; the dialog says so.

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
// When a tool catalog is known it holds only read-only tools: write and admin
// tools left off are a choice, not a problem, and are not asked about.
type switchedOff struct {
	readOnlyOnly bool                // filtered on the recorded read-only tools
	servers      []string            // Taufinity servers with tools switched off
	tools        map[string][]string // server → switched-off tool names
	total        int
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

// keepReadOnly narrows tools to the read-only ones in readOnly. A nil
// readOnly means no catalog is known, and tools come back unchanged.
func keepReadOnly(tools []string, readOnly map[string]bool) []string {
	if readOnly == nil {
		return tools
	}
	kept := make([]string, 0, len(tools))
	for _, t := range tools {
		if readOnly[t] {
			kept = append(kept, t)
		}
	}
	return kept
}

func findSwitchedOff(cfgPath string) (switchedOff, error) {
	readOnly := loadReadOnlyTools()
	out := switchedOff{tools: map[string][]string{}, readOnlyOnly: readOnly != nil}
	servers, err := desktopconfig.BridgeServers(cfgPath)
	if err != nil {
		return out, err
	}
	for _, srv := range servers {
		tools, err := desktopconfig.SwitchedOffTools(cfgPath, srv)
		if err != nil {
			return out, err
		}
		tools = keepReadOnly(tools, readOnly)
		if len(tools) > 0 {
			out.servers = append(out.servers, srv)
			out.tools[srv] = tools
			out.total += len(tools)
		}
	}
	return out, nil
}

// repairMessage is the text of the first dialog, and of the terminal output.
func repairMessage(off switchedOff) string {
	quoted := make([]string, len(off.servers))
	for i, s := range off.servers {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("Some Taufinity connections are turned off\n"+
		"To restore connections open Customize → Connectors → Yours →\n"+
		"%s → allow the tools → restart Claude Desktop.",
		strings.Join(quoted, " and "))
}

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
			if off.readOnlyOnly {
				fmt.Fprintln(out, "Nothing to fix: every Taufinity tool that reads your data is switched on in Claude Desktop.")
			} else {
				fmt.Fprintln(out, "Nothing to fix: all features of your Taufinity connection are switched on in Claude Desktop.")
			}
			return nil
		}
		fmt.Fprintln(out, repairMessage(off))
		if !desktopRepairSupported {
			return nil
		}
		// The lock guards the dialogs and the state file alike, so a manual
		// run cannot interleave with a prompt the bridge started.
		release, ok := acquireRepairLock()
		if !ok {
			fmt.Fprintln(out, "A Taufinity dialog about this is already open; use that one.")
			return nil
		}
		defer release()
		_, err = guideRepair(out, off)
		return err
	}

	// --prompt runs unattended (bridge startup, daily agent). Several bridges
	// start in the same second, so only the first one may show a dialog. The
	// lock is held for the whole run: it also serialises the state file.
	release, ok := acquireRepairLock()
	if !ok {
		return nil
	}
	defer release()

	off, err := findSwitchedOff(cfgPath)
	// Without a recorded catalog there is no telling a switched-off read tool
	// from a write tool left off on purpose, so stay quiet rather than nag.
	if err != nil || off.total == 0 || !off.readOnlyOnly || !desktopRepairSupported {
		return err
	}

	state := loadRepairState()
	now := time.Now()
	if !state.SettingsOpenedAt.IsZero() && now.Sub(state.SettingsOpenedAt) < repairAckWindow {
		// The user was just in Connectors.
		opened := state.OffWhenOpened
		state.SettingsOpenedAt, state.OffWhenOpened = time.Time{}, 0
		if off.total < opened {
			// They switched some on; what is still off is their choice.
			state.Acknowledged = off.keys()
			telemetry.Report(telemetry.Event{
				EventType:    "mcp.tool_toggles_acknowledged",
				ErrorCode:    "left_off_after_settings",
				ErrorMessage: fmt.Sprintf("%d of %d tools left switched off after opening Connectors", off.total, opened),
			})
		} else {
			// Nothing changed: probably did not find the switch. Ask again later.
			state.SnoozedUntil = now.Add(repairSnoozeUnanswered)
			telemetry.Report(telemetry.Event{
				EventType:    "mcp.tool_toggles_unchanged",
				ErrorCode:    "unchanged_after_settings",
				ErrorMessage: fmt.Sprintf("%d tools still switched off after opening Connectors", off.total),
			})
		}
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

// guideRepair shows the switched-off dialog and, on "Open settings", opens
// Connectors. It returns the answer to the dialog.
func guideRepair(out io.Writer, off switchedOff) (dialogAnswer, error) {
	telemetry.Report(telemetry.Event{
		EventType:    "mcp.tool_toggles_switched_off",
		ErrorCode:    "repair_prompt_shown",
		ErrorMessage: fmt.Sprintf("%d tools switched off across %d server(s)", off.total, len(off.servers)),
	})
	answer, err := claudeDesktop.Ask(repairMessage(off), "Open settings")
	if err != nil {
		reportRepairFailure("dialog_failed", err)
		return answer, err
	}
	if answer != answerOK {
		return answer, nil
	}

	if err := claudeDesktop.OpenSettings(); err != nil {
		reportRepairFailure("open_settings_failed", err)
		fmt.Fprintf(out, "Could not open the settings (%v); open them yourself as described above.\n", err)
	}
	state := loadRepairState()
	state.SettingsOpenedAt, state.OffWhenOpened = time.Now(), off.total
	_ = saveRepairState(state)
	return answerOK, nil
}

// reportRepairFailure records why the guided repair could not complete, so a
// broken dialog or deep link shows up in telemetry instead of only on screen.
func reportRepairFailure(code string, err error) {
	telemetry.Report(telemetry.Event{
		EventType:    "mcp.repair_failed",
		ErrorCode:    code,
		ErrorMessage: err.Error(),
	})
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
	OffWhenOpened    int       `json:"off_when_opened,omitempty"`
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
	if err != nil {
		// "Later" is the cancel button: osascript then fails with "User
		// canceled. (-128)", which is an answer. Anything else (no display,
		// no permission) is a failure, and must not snooze the dialog.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && isUserCancel(string(exitErr.Stderr)) {
			return answerLater, nil
		}
		return answerLater, fmt.Errorf("show dialog: %w", err)
	}
	return parseDialogAnswer(string(raw), okLabel), nil
}

// isUserCancel reports whether osascript's stderr is AppleScript's
// "User canceled" error (-128), raised by the cancel button.
func isUserCancel(stderr string) bool {
	return strings.Contains(stderr, "(-128)")
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
