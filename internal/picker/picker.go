package picker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/moby/term"
)

// ErrCancelled is returned when the user aborts an interactive selection.
// The CLI exits quietly (status 130) instead of printing an error.
var ErrCancelled = errors.New("selection cancelled")

// Item represents a selectable entry in the picker.
type Item struct {
	Label string // display text (e.g. "my-app (nginx:alpine) — Up 2 hours")
	Value string // actual target name
}

const maxTextPickerItems = 20

// Pick shows an interactive select list and returns the chosen Value. The
// context cancels the picker (Ctrl-C is handled internally; SIGTERM/SIGHUP
// arrive via ctx), which restores the terminal before returning.
func Pick(ctx context.Context, title string, items []Item) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("no items to select from")
	}

	// Without a terminal on stdio the full-screen picker cannot run; fall
	// back to the plain-text picker on /dev/tty so a shell with redirected
	// stdin/stdout can still select interactively.
	if !stdioIsTerminal() {
		return pickText(ctx, title, items)
	}

	opts := make([]huh.Option[string], len(items))
	for i, item := range items {
		opts[i] = huh.NewOption(item.Label, item.Value)
	}

	var selected string
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(title).
			Options(opts...).
			Filtering(true).
			Height(15).
			Value(&selected),
	))
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || ctx.Err() != nil {
			return "", ErrCancelled
		}
		return "", fmt.Errorf("interactive selection failed: %w", err)
	}

	return selected, nil
}

func stdioIsTerminal() bool {
	_, stdinTerm := term.GetFdInfo(os.Stdin)
	_, stdoutTerm := term.GetFdInfo(os.Stdout)
	return stdinTerm && stdoutTerm
}

// PickText is a lightweight terminal picker. It avoids Bubble Tea's full TUI,
// which is useful for very large Kubernetes namespaces or terminals where the
// richer picker is too heavy. Users can type a substring to filter, then select
// a number from the displayed matches.
func PickText(title string, items []Item) (string, error) {
	return pickText(context.Background(), title, items)
}

func pickText(ctx context.Context, title string, items []Item) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("no items to select from")
	}
	if len(items) == 1 {
		return items[0].Value, nil
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("multiple items matched but no TTY is available for selection")
	}
	defer func() { _ = tty.Close() }()

	return pickTextFromReadCloser(ctx, title, items, tty, tty)
}

// pickTextFromReadCloser interrupts a blocking terminal read when the command
// context is cancelled. signalContext turns SIGTERM/SIGHUP into cancellation,
// so merely checking ctx between reads would leave the process stuck forever.
func pickTextFromReadCloser(ctx context.Context, title string, items []Item, input io.ReadCloser, output io.Writer) (string, error) {
	stopInterrupt := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer stopInterrupt()

	selected, err := pickTextFromReader(title, items, input, output)
	if ctx.Err() != nil {
		return "", ErrCancelled
	}
	return selected, err
}

func pickTextFromReader(title string, items []Item, input io.Reader, output io.Writer) (string, error) {
	reader := bufio.NewReader(input)
	query := ""

	for {
		matches := filterItems(items, query)
		_, _ = fmt.Fprintf(output, "\n%s\n", title)
		if query != "" {
			_, _ = fmt.Fprintf(output, "Filter: %q (%d match(es))\n", query, len(matches))
		}

		if len(matches) == 0 {
			_, _ = fmt.Fprintln(output, "  No matches. Type another filter or 'q' to cancel.")
		} else {
			limit := len(matches)
			if limit > maxTextPickerItems {
				limit = maxTextPickerItems
			}
			for i := range limit {
				_, _ = fmt.Fprintf(output, "  %2d) %s\n", i+1, matches[i].Label)
			}
			if len(matches) > limit {
				_, _ = fmt.Fprintf(output, "  … %d more. Type a narrower filter.\n", len(matches)-limit)
			}
		}

		_, _ = fmt.Fprint(output, "Select number, type filter, or q to cancel: ")
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("reading selection: %w", err)
		}
		choice := strings.TrimSpace(line)
		if err == io.EOF && choice == "" {
			// Input closed without a selection; don't loop forever re-printing
			// the menu.
			return "", ErrCancelled
		}
		if strings.EqualFold(choice, "q") || strings.EqualFold(choice, "quit") {
			return "", ErrCancelled
		}
		if choice == "" {
			if len(matches) == 1 {
				return matches[0].Value, nil
			}
			query = ""
			continue
		}
		if n, err := strconv.Atoi(choice); err == nil {
			if n >= 1 && n <= len(matches) && n <= maxTextPickerItems {
				return matches[n-1].Value, nil
			}
			_, _ = fmt.Fprintln(output, "Invalid number.")
			continue
		}
		query = choice
	}
}

func filterItems(items []Item, query string) []Item {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return items
	}

	matches := make([]Item, 0, len(items))
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.Label), query) || strings.Contains(strings.ToLower(item.Value), query) {
			matches = append(matches, item)
		}
	}
	return matches
}
