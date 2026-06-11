package picker

import (
	"errors"
	"strings"
	"testing"
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
