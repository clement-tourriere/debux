package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"
)

type kubernetesExecutorFunc func(context.Context, remotecommand.StreamOptions) error

func (f kubernetesExecutorFunc) Stream(opts remotecommand.StreamOptions) error {
	return f(context.Background(), opts)
}

func (f kubernetesExecutorFunc) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	return f(ctx, opts)
}

func kubernetesTestStdin(t *testing.T) *os.File {
	t.Helper()
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = input
	t.Cleanup(func() {
		os.Stdin = old
		_ = input.Close()
		_ = writer.Close()
	})
	return writer
}

func TestKubernetesStreamShutdownDeliversEOFToStdinCopier(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "normal exit"},
		{name: "cancelled", err: context.Canceled},
		{name: "transport error", err: io.ErrClosedPipe},
		{name: "command failure", err: k8sexec.CodeExitError{Err: errors.New("exit 42"), Code: 42}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			kubernetesTestStdin(t)
			copied := make(chan error, 1)
			executor := kubernetesExecutorFunc(func(_ context.Context, opts remotecommand.StreamOptions) error {
				// Like client-go's copyStdin, this goroutine can outlive StreamWithContext.
				go func() {
					_, err := io.Copy(io.Discard, opts.Stdin)
					copied <- err
				}()
				return tt.err
			})
			err := streamKubernetesSession(t.Context(), executor, remotecommand.StreamOptions{})
			if tt.name == "command failure" {
				var exit *ExitError
				if !errors.As(err, &exit) || exit.Code != 42 {
					t.Errorf("command exit status lost: %v", err)
				}
			} else if !errors.Is(err, tt.err) {
				t.Errorf("stream error = %v, want %v", err, tt.err)
			}
			select {
			case err := <-copied:
				if err != nil {
					t.Fatalf("stdin copier should see clean EOF, not a shutdown error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stdin copier did not stop after the session ended")
			}
		})
	}
}

func TestKubernetesStreamShutdownUnblocksPendingInputWrite(t *testing.T) {
	writer := kubernetesTestStdin(t)
	if _, err := writer.Write([]byte("pending input")); err != nil {
		t.Fatal(err)
	}
	var clientStdin io.Reader
	executor := kubernetesExecutorFunc(func(_ context.Context, opts remotecommand.StreamOptions) error {
		clientStdin = opts.Stdin
		// Consume just one byte, leaving the pump's pipe write unfinished when
		// the remote command exits. Closing only the writer must still stop it.
		buf := make([]byte, 1)
		if _, err := io.ReadFull(opts.Stdin, buf); err != nil {
			return err
		}
		if buf[0] != 'p' {
			return fmt.Errorf("stdin data lost: %q", buf)
		}
		return nil
	})
	if err := streamKubernetesSession(t.Context(), executor, remotecommand.StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, clientStdin); err != nil {
		t.Fatalf("late client-go stdin copy should see EOF after pending write is stopped: %v", err)
	}
}
