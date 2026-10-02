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

	// repairLoopGuard: tools still switched off this soon after a repair mean
	// Claude Desktop restored them itself. Asking again would loop, so we
	// report it and stay quiet instead.
	repairLoopGuard = 15 * time.Minute

	// repairLockStale: a lock older than this belongs to a dialog that died.
	repairLockStale = 20 * time.Minute
)

var (
	// repairQuitTimeout caps how long we wait for Claude Desktop to exit
	// before giving up without touching its files. A var so tests can shorten it.
	repairQuitTimeout = 30 * time.Second

	// desktopRepairSupported gates the dialog and the restart, which use macOS
	// tooling. A var so tests run the same paths on Linux CI.
	desktopRepairSupported = runtime.GOOS == "darwin"

	flagMCPRepairPrompt    bool
	flagMCPRepairNoRestart bool
)

var mcpRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Re-enable Taufinity tools that Claude Desktop switched off",
	Long: `Repair finds the Taufinity servers in Claude Desktop's config and
re-enables every tool Claude Desktop has recorded as switched off for them.

Claude Desktop keeps that list across reinstalls and renames, and only reads
it at startup. Repair therefore quits Claude Desktop first, changes the file
while it is closed (so the app cannot write the old list back), and reopens it.

Flags:
  --prompt      Ask first in a dialog, and do nothing when there is nothing to
                repair. Used by the bridge at startup and by the daily check.
                "Later" keeps the dialog away for seven days.
  --no-restart  Change the file only; restart Claude Desktop yourself.`,
	RunE: runMCPRepair,
}

func init() {
	mcpCmd.AddCommand(mcpRepairCmd)
	mcpRepairCmd.Flags().BoolVar(&flagMCPRepairPrompt, "prompt", false, "Ask in a dialog first; silent when nothing needs repair")
	mcpRepairCmd.Flags().BoolVar(&flagMCPRepairNoRestart, "no-restart", false, "Do not quit and reopen Claude Desktop")
}

// desktopApp is the part of Claude Desktop repair talks to. Swapped in tests.
type desktopApp interface {
	Running() bool
	Quit() error
	Open() error
	// Ask shows message with a "Later" and a "Fix now" button and
	// reports whether "Fix now" was chosen.
	Ask(message string) (bool, error)
}

var claudeDesktop desktopApp = macClaudeDesktop{}

func runMCPRepair(cmd *cobra.Command, _ []string) error {
	cfgPath, err := claudeDesktopPath()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	restart := !flagMCPRepairNoRestart && desktopRepairSupported

	if !flagMCPRepairPrompt {
		n, err := repairDesktopTools(out, cfgPath, restart)
		if err != nil {
			return err
		}
		if n == 0 {
			fmt.Fprintln(out, "Nothing to fix: all features of your Taufinity connection are switched on in Claude Desktop.")
		}
		return nil
	}

	// --prompt runs unattended (bridge startup, daily agent). Several bridges
	// start in the same second, so only the first one may show a dialog.
	release, ok := acquireRepairLock()
	if !ok {
		return nil
	}
	defer release()

	off, err := switchedOffTools(cfgPath)
	if err != nil || off == 0 {
		return err
	}
	state := loadRepairState()
	now := time.Now()
	if now.Sub(state.LastRepairAt) < repairLoopGuard {
		telemetry.Report(telemetry.Event{
			EventType:    "mcp.tool_toggles_restored",
			ErrorCode:    "switched_off_after_repair",
			ErrorMessage: fmt.Sprintf("%d tools switched off again within %s of a repair", off, repairLoopGuard),
		})
		return nil
	}
	if now.Before(state.SnoozedUntil) || !desktopRepairSupported {
		return nil
	}

	yes, err := claudeDesktop.Ask(fmt.Sprintf(
		"Claude can't fully use your Taufinity connection,\n"+
			"because %d of its features are switched off.\n\n"+
			"Fix it now? Claude Desktop will close and reopen.", off))
	if err != nil {
		return err
	}
	if !yes {
		state.SnoozedUntil = now.Add(repairSnooze)
		return saveRepairState(state)
	}
	_, err = repairDesktopTools(out, cfgPath, restart)
	return err
}

// switchedOffTools sums the switched-off tools over every Taufinity bridge
// server in the Claude Desktop config.
func switchedOffTools(cfgPath string) (int, error) {
	servers, err := desktopconfig.BridgeServers(cfgPath)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, s := range servers {
		n, err := desktopconfig.CountToolToggles(cfgPath, s)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// repairDesktopTools quits Claude Desktop when restart is set, clears the
// switched-off tools of every Taufinity server, and reopens the app. If the
// app does not quit in time nothing is changed, because a running app would
// write its in-memory list back over ours.
func repairDesktopTools(out io.Writer, cfgPath string, restart bool) (int, error) {
	servers, err := desktopconfig.BridgeServers(cfgPath)
	if err != nil {
		return 0, err
	}
	off, err := switchedOffTools(cfgPath)
	if err != nil || off == 0 {
		return 0, err
	}

	wasRunning := restart && claudeDesktop.Running()
	if wasRunning {
		if err := claudeDesktop.Quit(); err != nil {
			return 0, fmt.Errorf("quit Claude Desktop: %w", err)
		}
		deadline := time.Now().Add(repairQuitTimeout)
		for claudeDesktop.Running() {
			if time.Now().After(deadline) {
				return 0, errors.New("Claude Desktop did not quit in time; nothing was changed")
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	total := 0
	for _, s := range servers {
		n, err := desktopconfig.ClearToolToggles(cfgPath, s)
		if err != nil {
			return total, err
		}
		total += n
	}
	state := loadRepairState()
	state.LastRepairAt = time.Now()
	_ = saveRepairState(state)
	telemetry.Report(telemetry.Event{
		EventType:    "mcp.tool_toggles_cleared",
		ErrorCode:    "tools_reenabled",
		ErrorMessage: fmt.Sprintf("re-enabled %d tools across %d server(s)", total, len(servers)),
	})
	fmt.Fprintf(out, "Switched on %d feature(s) of your Taufinity connection in Claude Desktop.\n", total)

	if wasRunning {
		if err := claudeDesktop.Open(); err != nil {
			return total, fmt.Errorf("reopen Claude Desktop: %w", err)
		}
	} else if !restart {
		fmt.Fprintln(out, "Quit and reopen Claude Desktop to load them.")
	}
	return total, nil
}

// repairState persists the dialog's snooze and the last repair, so the
// prompt neither nags nor loops across bridge restarts.
type repairState struct {
	SnoozedUntil time.Time `json:"snoozed_until"`
	LastRepairAt time.Time `json:"last_repair_at"`
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

func (macClaudeDesktop) Ask(message string) (bool, error) {
	script := fmt.Sprintf(`display dialog %q with title "Taufinity" `+
		`buttons {"Later", "Fix now"} default button "Fix now" cancel button "Later" `+
		`with icon caution giving up after 900`, message)
	raw, err := exec.Command("osascript", "-e", script).Output()
	// "Later" is the cancel button, so osascript exits non-zero for it; that
	// is an answer, not a failure.
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(string(raw), "button returned:Fix now"), nil
}
