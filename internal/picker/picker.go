package picker

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
)

// Item represents a selectable entry in the picker.
type Item struct {
	Label string // display text (e.g. "my-app (nginx:alpine) — Up 2 hours")
	Value string // actual target name
}

const maxTextPickerItems = 20

// Pick shows an interactive select list and returns the chosen Value.
func Pick(title string, items []Item) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("no items to select from")
	}

	opts := make([]huh.Option[string], len(items))
	for i, item := range items {
		opts[i] = huh.NewOption(item.Label, item.Value)
	}

	var selected string
	err := huh.NewSelect[string]().
		Title(title).
		Options(opts...).
		Filtering(true).
		Height(15).
		Value(&selected).
		Run()
	if err != nil {
		return "", fmt.Errorf("selection cancelled: %w", err)
	}

	return selected, nil
}

// PickText is a lightweight terminal picker. It avoids Bubble Tea's full TUI,
// which is useful for very large Kubernetes namespaces or terminals where the
// richer picker is too heavy. Users can type a substring to filter, then select
// a number from the displayed matches.
func PickText(title string, items []Item) (string, error) {
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

	return pickTextFromReader(title, items, tty, tty)
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
			for i := 0; i < limit; i++ {
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
		if strings.EqualFold(choice, "q") || strings.EqualFold(choice, "quit") {
			return "", fmt.Errorf("selection cancelled")
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
