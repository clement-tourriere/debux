package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// TUIState holds the last-used options from the interactive TUI so repeat
// invocations can start with the user's preferred defaults. Flags always win.
type TUIState struct {
	Image      string `yaml:"image,omitempty"`
	User       string `yaml:"user,omitempty"`
	PullPolicy string `yaml:"pull-policy,omitempty"`
	Profile    string `yaml:"profile,omitempty"`
	Fresh      bool   `yaml:"fresh,omitempty"`
	Copy       bool   `yaml:"copy,omitempty"`
	Privileged bool   `yaml:"privileged,omitempty"`
	// NoVolumes mirrors the --no-volumes flag (the inverse of shareVolumes) so
	// its zero value (false) matches the default of sharing volumes. Storing
	// ShareVolumes instead would make a missing/zero state read as "off" and
	// silently disable volume sharing on first run.
	NoVolumes       bool `yaml:"no-volumes,omitempty"`
	ReadOnlyVolumes bool `yaml:"read-only-volumes,omitempty"`
}

// TUIStatePath returns the path to the TUI options state file, honoring
// $XDG_CACHE_HOME when set.
func TUIStatePath() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolving cache directory: %w", err)
		}
	}
	// Store TUI state next to other debux cached data so it is easy to locate
	// and clear without touching the user's config directory.
	return filepath.Join(base, "debux", "tui-options.yaml"), nil
}

// LoadTUIState returns the persisted TUI options. A missing file yields a zero
// TUIState; a malformed file is reported on stderr and ignored. Because every
// field's zero value equals its default (strings are guarded by the caller and
// NoVolumes is stored inverted), a zero TUIState safely means "keep defaults".
func LoadTUIState() TUIState {
	state, err := loadTUIState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: ignoring debux TUI state: %v\n", err)
	}
	return state
}

func loadTUIState() (TUIState, error) {
	path, err := TUIStatePath()
	if err != nil {
		return TUIState{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return TUIState{}, nil
		}
		return TUIState{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var state TUIState
	if err := yaml.Unmarshal(data, &state); err != nil {
		return TUIState{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return state, nil
}

// SaveTUIState persists the given TUI options for the next invocation.
func SaveTUIState(state TUIState) error {
	path, err := TUIStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling TUI state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
