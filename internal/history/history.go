package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/clement-tourriere/debux/internal/runtime"
)

const maxEntries = 200

// Entry records a previously launched debug session.
type Entry struct {
	StartedAt       time.Time `json:"startedAt"`
	Target          string    `json:"target"`
	Runtime         string    `json:"runtime"`
	Context         string    `json:"context,omitempty"`
	Namespace       string    `json:"namespace,omitempty"`
	Name            string    `json:"name"`
	Container       string    `json:"container,omitempty"`
	Image           string    `json:"image,omitempty"`
	Profile         string    `json:"profile,omitempty"`
	Fresh           bool      `json:"fresh,omitempty"`
	Copy            bool      `json:"copy,omitempty"`
	ShareVolumes    bool      `json:"shareVolumes"`
	ReadOnlyVolumes bool      `json:"readOnlyVolumes,omitempty"`
	Command         []string  `json:"command,omitempty"`
	Launcher        string    `json:"launcher,omitempty"`
}

// Path returns the history file path.
func Path() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "debux", "history.json"), nil
}

// Load reads the session history. Missing files are treated as empty history.
func Load() ([]Entry, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading history: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing history: %w", err)
	}
	return entries, nil
}

// Append records a session, most-recent first, and caps the file size.
// A corrupt history file is moved aside and recording starts over — refusing
// to append forever because one write was torn would silently disable history.
func Append(entry Entry) error {
	entries, err := Load()
	if err != nil {
		path, pathErr := Path()
		if pathErr != nil {
			return err
		}
		_ = os.Rename(path, path+".corrupt")
		entries = nil
	}
	if entry.StartedAt.IsZero() {
		entry.StartedAt = time.Now()
	}
	entries = append([]Entry{entry}, entries...)
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
	}

	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating history directory: %w", err)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding history: %w", err)
	}
	data = append(data, '\n')

	// Write atomically (temp file + rename): concurrent debux sessions append
	// too, and an in-place truncate-and-write can tear the file mid-crash.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history-*")
	if err != nil {
		return fmt.Errorf("writing history: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing history: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing history: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing history: %w", err)
	}
	return nil
}

// NewEntry constructs a history entry from a resolved target and options.
func NewEntry(target *runtime.Target, targetString string, opts runtime.DebugOpts, launcher string) Entry {
	return Entry{
		StartedAt:       time.Now(),
		Target:          targetString,
		Runtime:         target.Runtime,
		Context:         target.Context,
		Namespace:       target.Namespace,
		Name:            target.Name,
		Container:       target.Container,
		Image:           opts.Image,
		Profile:         opts.Profile,
		Fresh:           opts.Fresh,
		Copy:            opts.Copy,
		ShareVolumes:    opts.ShareVolumes,
		ReadOnlyVolumes: opts.ReadOnlyVolumes,
		Command:         append([]string(nil), opts.Command...),
		Launcher:        launcher,
	}
}
