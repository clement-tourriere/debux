package config

import (
	"path/filepath"
	"testing"
)

func TestSaveAndLoadTUIState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	state := TUIState{
		Image:           "img:v1",
		User:            "1000",
		PullPolicy:      "IfNotPresent",
		Profile:         "restricted",
		Fresh:           true,
		Copy:            true,
		Privileged:      false,
		NoVolumes:       true,
		ReadOnlyVolumes: true,
	}
	if err := SaveTUIState(state); err != nil {
		t.Fatalf("SaveTUIState: %v", err)
	}

	got := LoadTUIState()
	if got != state {
		t.Errorf("loaded state = %+v, want %+v", got, state)
	}
}

func TestLoadTUIStateMissingFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	got := LoadTUIState()
	if got != (TUIState{}) {
		t.Errorf("expected zero state, got %+v", got)
	}
}

func TestTUIStatePathUsesCacheDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	path, err := TUIStatePath()
	if err != nil {
		t.Fatalf("TUIStatePath: %v", err)
	}
	want := filepath.Join(dir, "debux", "tui-options.yaml")
	if path != want {
		t.Errorf("TUIStatePath() = %q, want %q", path, want)
	}
}
