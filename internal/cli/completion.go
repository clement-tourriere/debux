package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for debux.

Load the generated script from your shell profile, or write it to your shell's
completion directory.`,
		Example: `  debux completion zsh > ~/.zfunc/_debux
  debux completion bash > /etc/bash_completion.d/debux
  debux completion fish > ~/.config/fish/completions/debux.fish`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(cmd.OutOrStdout())
			case "zsh":
				return root.GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return root.GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return root.GenPowerShellCompletion(cmd.OutOrStdout())
			default:
				return fmt.Errorf("unsupported shell %q: expected bash, zsh, fish, or powershell", args[0])
			}
		},
	}
	return cmd
}
