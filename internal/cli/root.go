package cli

import (
	"github.com/spf13/cobra"
)

var (
	flagImage      string
	flagPrivileged bool
	flagUser       string
	flagRemove     bool
)

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debux",
		Short: "Universal container debugging tool",
		Long:  "Debug any container — even distroless/scratch — with a rich NixOS-powered shell.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&flagImage, "image", "", "Override debug image (default: ghcr.io/ctourriere/debux:latest)")
	cmd.PersistentFlags().BoolVar(&flagPrivileged, "privileged", false, "Run debug container in privileged mode")
	cmd.PersistentFlags().StringVar(&flagUser, "user", "", "Run as specific user (uid:gid)")
	cmd.PersistentFlags().BoolVar(&flagRemove, "rm", true, "Auto-remove debug container on exit")

	cmd.AddCommand(newExecCmd())
	cmd.AddCommand(newPodCmd())
	cmd.AddCommand(newStoreCmd())

	return cmd
}

func Execute() error {
	return NewRootCmd().Execute()
}
