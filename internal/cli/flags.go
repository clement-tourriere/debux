package cli

import "github.com/spf13/cobra"

// Flags belong to their command, not package globals shared by every root,
// subcommand, and TUI launch. Optional flags on helper commands return zero.
func flagString(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

func flagBool(cmd *cobra.Command, name string) bool {
	value, _ := cmd.Flags().GetBool(name)
	return value
}

func flagStrings(cmd *cobra.Command, name string) []string {
	value, _ := cmd.Flags().GetStringArray(name)
	return value
}
