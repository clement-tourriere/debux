package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/clement-tourriere/debux/internal/selfupdate"
	"github.com/clement-tourriere/debux/internal/version"
	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	var checkOnly bool
	var targetVersion string
	var installPath string
	var force bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check for or install the latest debux release",
		Long: `Check GitHub Releases for a newer debux binary and optionally install it.

By default, debux updates the currently running executable in place. If the
binary lives in a protected directory such as /usr/local/bin, rerun the command
with appropriate permissions or use the one-line installer with --bin-dir.`,
		Example: `  debux update --check
  debux update
  debux update --version v1.2.3
  debux update --install-path ~/.local/bin/debux`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			if installPath == "" {
				path, err := os.Executable()
				if err != nil {
					return fmt.Errorf("locating current executable: %w", err)
				}
				installPath = path
			}

			result, err := selfupdate.Run(ctx, selfupdate.Options{
				Repo:           selfupdate.DefaultRepo,
				CurrentVersion: version.Version,
				TargetVersion:  targetVersion,
				InstallPath:    installPath,
				CheckOnly:      checkOnly,
				Force:          force,
				Stdout:         cmd.OutOrStdout(),
			})
			if err != nil {
				return fmt.Errorf("updating debux: %w", err)
			}

			printUpdateResult(cmd, result, targetVersion)
			return nil
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false, "Only check for updates; do not install")
	cmd.Flags().StringVar(&targetVersion, "version", "", "Install a specific version (for example v1.2.3)")
	cmd.Flags().StringVar(&installPath, "install-path", "", "Path to replace (default: current debux executable)")
	cmd.Flags().BoolVar(&force, "force", false, "Reinstall even if the selected version is already installed")

	return cmd
}

func printUpdateResult(cmd *cobra.Command, result selfupdate.Result, targetVersion string) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Current: %s\n", result.Current)
	_, _ = fmt.Fprintf(out, "Latest:  %s\n", result.Latest)

	if result.CheckOnly {
		if result.UpToDate {
			_, _ = fmt.Fprintln(out, "debux is up to date.")
			return
		}
		if targetVersion != "" {
			_, _ = fmt.Fprintf(out, "Selected debux release is available. Run: debux update --version %s\n", targetVersion)
			return
		}
		_, _ = fmt.Fprintln(out, "A newer debux release is available. Run: debux update")
		return
	}

	if result.UpToDate {
		_, _ = fmt.Fprintln(out, "debux is already up to date.")
		return
	}
	if result.Updated {
		_, _ = fmt.Fprintf(out, "Updated %s to %s.\n", result.InstallPath, result.Latest)
		_, _ = fmt.Fprintln(out, "Run `hash -r` if your shell still finds an older debux.")
	}
}
