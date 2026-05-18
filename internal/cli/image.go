package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image <image-ref>",
		Short: "Debug a Docker image without starting it",
		Long: `Debug a Docker image by copying its filesystem into a debug container.

This works with scratch and distroless images because the target image is never
started. Its filesystem is copied into the debug container and exposed at /target.`,
		Example: `  debux image gcr.io/distroless/static-debian12
  debux image my-app:broken
  debux image my-app:broken --rm=false`,
		Args: cobra.ExactArgs(1),
		RunE: runImage,
	}
	addImageFlags(cmd)
	configureImageArgCompletion(cmd)
	return cmd
}

func runImage(cmd *cobra.Command, args []string) error {
	imageRef := args[0]

	debugImage := flagImage
	if debugImage == "" {
		debugImage = runtime.DefaultImage
	}

	opts := runtime.ImageOpts{
		DebugImage: debugImage,
		Privileged: flagPrivileged,
		User:       flagUser,
		AutoRemove: flagRemove,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return runtime.DockerImage(ctx, imageRef, opts)
}
