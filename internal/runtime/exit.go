package runtime

import "fmt"

// ExitError carries a remote command's exit status so the CLI can terminate
// with the same code instead of collapsing everything to exit 1.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.Code)
}
