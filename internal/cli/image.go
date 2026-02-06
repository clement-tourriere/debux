package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/ctourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newImageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "image <image-ref>",
		Short: "Debug a Docker image directly",
		Long: `Debug a Docker image by copying its filesystem into a debug container.

Works with ALL images including scratch and distroless — the target image
is never started. The image filesystem is available at /target.`,
		Args: cobra.ExactArgs(1),
		RunE: runImage,
	}
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
