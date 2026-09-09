package config

import (
	"os"
	"path/filepath"
	"strings"
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
func TestValidationFailsClosed(t *testing.T) {
	for _, body := range []string{"profile: restricted\ntoolz: {}\n", "profile: restricted\n---\nprofile: general\n", "tools: [invalid]\n"} {
		writeConfig(t, body)
		if err := Validate(); err == nil || !strings.Contains(err.Error(), "DEBUX_CONFIG=/dev/null") {
			t.Fatalf("malformed config accepted: %v", err)
		}
	}
	writeConfig(t, "")
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPathHonorsXDG(t *testing.T) {
	t.Setenv("DEBUX_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := Path()
	if err != nil || path != filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "debux", "config.yaml") {
		t.Fatalf("XDG path: %q %v", path, err)
	}
}

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
