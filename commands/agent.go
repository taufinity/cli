package commands

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/telemetry"
)

var agentCmd = &cobra.Command{
	Use:    "agent",
	Short:  "Internal commands used by the background update-check agent",
	Hidden: true,
	Annotations: map[string]string{
		"suppress-update-warning": "true",
	},
}

var agentReportErrorCmd = &cobra.Command{
	Use:   "report-error",
	Short: "Report an error from the background update-check agent to Studio telemetry",
	RunE:  runAgentReportError,
	Annotations: map[string]string{
		"suppress-update-warning": "true",
	},
}

var (
	agentErrorExitCode int
	agentErrorMessage  string
)

func init() {
	rootCmd.AddCommand(agentCmd)
	agentCmd.AddCommand(agentReportErrorCmd)
	agentReportErrorCmd.Flags().IntVar(&agentErrorExitCode, "exit-code", 0, "exit code from the failed update check")
	agentReportErrorCmd.Flags().StringVar(&agentErrorMessage, "message", "", "error message from the failed update check")
}

func runAgentReportError(cmd *cobra.Command, _ []string) error {
	if !telemetry.Enabled() {
		return nil
	}
	err := telemetry.ReportSync(telemetry.Event{
		EventType:    "agent.update_check_error",
		ErrorCode:    fmt.Sprintf("exit_%d", agentErrorExitCode),
		ErrorMessage: agentErrorMessage,
	}, 5*time.Second)
	if err != nil {
		return fmt.Errorf("telemetry report failed: %w", err)
	}
	return nil
}
