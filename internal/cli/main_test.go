package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates the cli test binary from the developer's (or CI image's)
// real debux config. Helpers such as resolveProfile fall back to config.Get(),
// which memoizes its first read via an unexported sync.Once this package cannot
// reset. Pointing DEBUX_CONFIG at a non-existent file before any test runs
// guarantees that first read resolves to an empty config, so tests do not
// depend on whatever ~/.config/debux/config.yaml happens to exist on the host.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "debux-cli-test")
	if err != nil {
		// Fall back to a path that cannot exist rather than the host config —
		// the whole point is to never read a real config file.
		dir = filepath.Join(os.TempDir(), "debux-cli-test-nonexistent")
	}
	// A path inside a dir we never populate => empty config.
	if err := os.Setenv("DEBUX_CONFIG", filepath.Join(dir, "config.yaml")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
