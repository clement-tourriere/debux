package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/clement-tourriere/debux/internal/cli"
	"github.com/clement-tourriere/debux/internal/picker"
	"github.com/clement-tourriere/debux/internal/runtime"
)

func main() {
	err := cli.Execute()
	if err == nil {
		return
	}

	// Propagate the remote command's exit status (like kubectl/docker exec)
	// without printing a spurious error.
	var exitErr *runtime.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.Code)
	}

	// Interrupted or user-cancelled runs exit quietly with the conventional
	// SIGINT status.
	if errors.Is(err, context.Canceled) || errors.Is(err, picker.ErrCancelled) {
		os.Exit(130)
	}

	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
