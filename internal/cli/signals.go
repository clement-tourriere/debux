package cli

import (
	"context"
	"os/signal"
	"syscall"
)

// signalContext returns a context cancelled on SIGINT, SIGTERM, or SIGHUP so
// debug containers and copied pods are cleaned up when the terminal closes.
// Once the context is cancelled the handler is unregistered, so a second
// Ctrl-C terminates the process even if cleanup hangs.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}
