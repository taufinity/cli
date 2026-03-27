package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"test-go/cmd/taufinity/internal/config"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "View and edit CLI configuration",
	Long: `The config command lets you set, view, and manage taufinity CLI properties.

Properties are stored in ~/.config/taufinity/config.yaml`,
}

var configSetCmd = &cobra.Command{
	Use:   "set PROPERTY VALUE",
	Short: "Set a configuration property",
	Long: `Set a configuration property.

Available properties:
  site      Default site ID (e.g., voorpositiviteit_nl)
  api_url   API base URL (default: https://studio.taufinity.io)`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key, value := args[0], args[1]
		if err := config.Set(key, value); err != nil {
			return err
		}
		Print("Updated property [%s]\n", key)
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get PROPERTY",
	Short: "Get a configuration property value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		value, err := config.Get(args[0])
		if err != nil {
			return err
		}
		fmt.Println(value)
		return nil
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configuration properties",
	RunE: func(cmd *cobra.Command, args []string) error {
		props, err := config.List()
		if err != nil {
			return err
		}

		format := GetFormat()
		switch format {
		case "json":
			return printJSON(props)
		case "yaml":
			return printYAML(props)
		default:
			// Table format
			for key, value := range props {
				if value == "" {
					value = "(unset)"
				}
				fmt.Printf("%s = %s\n", key, value)
			}
		}
		return nil
	},
}

var configUnsetCmd = &cobra.Command{
	Use:   "unset PROPERTY",
	Short: "Unset a configuration property (revert to default)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := config.Unset(args[0]); err != nil {
			return err
		}
		Print("Unset property [%s] (will use default)\n", args[0])
		return nil
	},
}

var configResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset all configuration to defaults",
	Long: `Reset all configuration properties to their default values.

This removes the config file at ~/.config/taufinity/config.yaml.
Default values:
  api_url: https://studio.taufinity.io
  site:    (none - must be set)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := config.Reset(); err != nil {
			return err
		}
		Print("Configuration reset to defaults\n")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configUnsetCmd)
	configCmd.AddCommand(configResetCmd)
}
