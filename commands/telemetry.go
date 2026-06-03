package commands

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/telemetry"
)

var telemetryCmd = &cobra.Command{
	Use:   "telemetry",
	Short: "Telemetry diagnostics",
}

var telemetryTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Send a test event to verify the telemetry pipeline",
	Long: `Sends a telemetry.test event to Studio synchronously and confirms receipt.

If it succeeds, check Studio → Admin → CLI Health to see the event.
If telemetry is not configured (e.g. a local dev build), a message is printed
and the command exits 0 — no event is sent.`,
	RunE: runTelemetryTest,
	Annotations: map[string]string{
		"suppress-update-warning": "true",
	},
}

func init() {
	rootCmd.AddCommand(telemetryCmd)
	telemetryCmd.AddCommand(telemetryTestCmd)
}

func runTelemetryTest(cmd *cobra.Command, _ []string) error {
	if !telemetry.Enabled() {
		fmt.Fprintln(cmd.OutOrStdout(), "Telemetry is not configured in this build.")
		fmt.Fprintln(cmd.OutOrStdout(), "Only official release builds (compiled with TelemetryKey) have telemetry enabled.")
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Sending telemetry.test event to %s…\n", telemetry.StudioURL)

	err := telemetry.ReportSync(telemetry.Event{
		EventType:    "telemetry.test",
		ErrorCode:    "manual_test",
		ErrorMessage: "manual test via taufinity telemetry test",
	}, 5*time.Second)
	if err != nil {
		return fmt.Errorf("telemetry test failed: %w\n\nCheck Studio → Admin → CLI Health for partial events", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), "✓ Event sent. Check Studio → Admin → CLI Health to confirm.")
	return nil
}
