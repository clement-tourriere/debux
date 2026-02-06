package cli

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/clement-tourriere/debux/internal/store"
	"github.com/spf13/cobra"
)

func newStoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "store",
		Short: "Manage the persistent Nix store",
	}

	cmd.AddCommand(newStoreCleanCmd())
	cmd.AddCommand(newStoreInfoCmd())

	return cmd
}

func newStoreCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Remove the persistent store volume",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			if err := store.Clean(ctx); err != nil {
				return err
			}
			fmt.Println("Store volumes removed.")
			return nil
		},
	}
}

func newStoreInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show store size and installed packages",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			return store.Info(ctx)
		},
	}
}
