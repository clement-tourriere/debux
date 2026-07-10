package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEBUX_CONFIG", path)
	// Reset the cached load so each test reads its own file.
	once = sync.Once{}
}

func TestResolveToolsExpandsSetsAndLiterals(t *testing.T) {
	writeConfig(t, `tools:
  python: [python3, py-spy, gdb]
  net: [socat, mtr]
`)

	got := ResolveTools([]string{"python", "strace", "net,python"})
	// Sets expand, literals pass through, duplicates collapse, order preserved.
	want := []string{"python3", "py-spy", "gdb", "strace", "socat", "mtr"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestResolveToolsCommaSeparatedLiterals(t *testing.T) {
	writeConfig(t, "")
	got := ResolveTools([]string{"py-spy,gdb", " delve "})
	want := []string{"py-spy", "gdb", "delve"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestGetDefaults(t *testing.T) {
	writeConfig(t, `image: my/img:dev
profile: restricted
pull-policy: Always
terminal: kitty
`)
	cfg := Get()
	if cfg.Image != "my/img:dev" || cfg.Profile != "restricted" || cfg.PullPolicy != "Always" || cfg.Terminal != "kitty" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestGetMissingFileIsEmpty(t *testing.T) {
	t.Setenv("DEBUX_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	once = sync.Once{}
	if cfg := Get(); cfg.Image != "" || len(cfg.Tools) != 0 {
		t.Fatalf("missing config should be empty, got %+v", cfg)
	}
}

// TestGetRejectsUnknownKeys locks the strict-decode behavior: a typo'd key
// ("profil:") must be reported instead of silently applying built-in defaults
// — for profile that could mean a more privileged session than configured.
func TestGetRejectsUnknownKeys(t *testing.T) {
	writeConfig(t, `profil: restricted
`)
	if cfg := Get(); cfg.Profile != "" {
		t.Fatalf("unknown key should not populate config, got %+v", cfg)
	}

	// The valid file still loads after the strict decoder change.
	writeConfig(t, `profile: restricted
`)
	if cfg := Get(); cfg.Profile != "restricted" {
		t.Fatalf("valid config did not load: %+v", cfg)
	}
}
