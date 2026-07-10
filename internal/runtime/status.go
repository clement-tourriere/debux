package runtime

import (
	"fmt"
	"os"
)

// Session progress, notes, and warnings go to stderr: in one-shot mode
// (`debux target -- cmd`) stdout carries only the command's output so scripts
// and CI can capture it cleanly.
func statusf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

func statusln(args ...any) {
	fmt.Fprintln(os.Stderr, args...)
}
