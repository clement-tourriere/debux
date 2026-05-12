package cli

import (
	"fmt"
	"os/exec"
	stdruntime "runtime"

	"github.com/spf13/cobra"
)

func newDocsCmd() *cobra.Command {
	var openBrowser bool
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Show the debux documentation URL",
		Long:  "Print the debux documentation URL, or open it in your browser with --open.",
		Example: `  debux docs
  debux docs --open`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !openBrowser {
				cmd.Println(docsURL)
				return nil
			}
			if err := openURL(docsURL); err != nil {
				return err
			}
			cmd.Printf("Opened %s\n", docsURL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&openBrowser, "open", false, "Open the documentation in your browser")
	return cmd
}

func openURL(url string) error {
	var command string
	var args []string

	switch stdruntime.GOOS {
	case "darwin":
		command = "open"
		args = []string{url}
	case "windows":
		command = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		command = "xdg-open"
		args = []string{url}
	}

	if err := exec.Command(command, args...).Start(); err != nil {
		return fmt.Errorf("opening docs: %w\n%s", err, docsURL)
	}
	return nil
}
