package picker

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPickTextEOFCancelsInsteadOfLooping(t *testing.T) {
	items := []Item{{Label: "a", Value: "a"}, {Label: "b", Value: "b"}}
	var out strings.Builder

	// Closed input with multiple matches used to loop forever re-printing the
	// menu; it must cancel.
	_, err := pickTextFromReader("pick", items, strings.NewReader(""), &out)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled on EOF, got %v", err)
	}
}

func TestPickTextQuitReturnsCancelled(t *testing.T) {
	items := []Item{{Label: "a", Value: "a"}, {Label: "b", Value: "b"}}
	var out strings.Builder
	_, err := pickTextFromReader("pick", items, strings.NewReader("q\n"), &out)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled on quit, got %v", err)
	}
}

func TestPickTextContextCancellationInterruptsRead(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		var out strings.Builder
		_, err := pickTextFromReadCloser(ctx, "pick", []Item{{Label: "a", Value: "a"}, {Label: "b", Value: "b"}}, input, &out)
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("expected ErrCancelled after context cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not interrupt the picker read")
	}
}
