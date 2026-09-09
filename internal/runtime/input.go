package runtime

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/moby/term"
	"github.com/muesli/cancelreader"
)

// startSessionInput owns stdin until stop returns. Closing the remote stream
// alone cannot interrupt a blocked terminal read; that used to steal the next
// TUI's input. Stop both directions before handing stdin back to the caller.
func startSessionInput(input *os.File, output io.Writer, closeOutput func(), closeWrite func() error) (func(), error) {
	var source io.Reader = input
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	_, tty := term.GetFdInfo(input)
	if info.Mode().IsRegular() || (info.Mode()&os.ModeCharDevice != 0 && !tty) {
		// epoll cannot monitor regular files or /dev/null. Their reads do not
		// wait for terminal input, so cancelreader's plain-reader fallback is safe.
		source = struct{ io.Reader }{input}
	}
	reader, err := cancelreader.NewReader(source)
	if err != nil {
		return nil, fmt.Errorf("preparing cancellable stdin: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(output, reader)
		_ = closeWrite()
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			reader.Cancel()
			closeOutput() // also unblock a write stalled by an unresponsive peer
			<-done
			_ = reader.Close()
		})
	}, nil
}
