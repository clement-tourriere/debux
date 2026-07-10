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
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating history directory: %w", err)
	}

	// The read-modify-write below is not atomic across processes: two debux
	// sessions starting together would each read the same file and the last
	// rename would silently drop the other's entry.
	release := acquireAppendLock(path)
	defer release()

	entries, err := Load()
	if err != nil {
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

// acquireAppendLock takes a best-effort cross-process lock via an O_EXCL lock
// file, stealing locks older than a few seconds (a killed process). History
// is advisory, so on a wedged lock it gives up after a short wait instead of
// blocking the debug session; the caller then risks losing one entry, which
// beats not debugging.
func acquireAppendLock(path string) (release func()) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }
		}
		if !os.IsExist(err) {
			return func() {}
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 5*time.Second {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return func() {}
		}
		time.Sleep(20 * time.Millisecond)
	}
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
