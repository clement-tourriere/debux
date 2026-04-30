package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/clement-tourriere/debux/internal/store"
	"github.com/spf13/cobra"
)

func newStoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "store",
		Short: "Inspect or clean debux Docker Nix store volumes",
		Long: `Inspect or clean the Docker volumes debux uses for persistent Nix data.

Docker debug sessions use image-specific Nix store volumes so installed tools can
survive across sessions without breaking rebuilt debug images.`,
		Example: `  debux store info
  debux store clean`,
	}

	cmd.AddCommand(newStoreCleanCmd())
	cmd.AddCommand(newStoreInfoCmd())

	return cmd
}

func newStoreCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Remove all debux store volumes",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			return store.Clean(ctx)
		},
	}
}

func newStoreInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show debux store volumes",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			return store.Info(ctx)
		},
	}
}
