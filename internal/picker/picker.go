package picker

import (
	"fmt"

	"github.com/charmbracelet/huh"
)

// Item represents a selectable entry in the picker.
type Item struct {
	Label string // display text (e.g. "my-app (nginx:alpine) — Up 2 hours")
	Value string // actual target name
}

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
		Value(&selected).
		Run()
	if err != nil {
		return "", fmt.Errorf("selection cancelled: %w", err)
	}

	return selected, nil
}
