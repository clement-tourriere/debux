package entrypoint

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDctlHelpWithoutBackendOrWritableStore(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required for dctl behavioral tests")
	}
	script, err := filepath.Abs("../../images/debug/dctl")
	if err != nil {
		t.Fatal(err)
	}
	type testCase struct {
		args []string
		want string
		code int
	}
	cases := []testCase{
		{nil, "Usage:", 0},
		{[]string{"--help"}, "Usage:", 0},
		{[]string{"-h"}, "Usage:", 0},
		{[]string{"help"}, "Usage:", 0},
		{[]string{"help", "unknown"}, "Unknown dctl command", 2},
		{[]string{"unknown", "--help"}, "Unknown dctl command", 2},
		{[]string{"help", "install", "extra"}, "Usage: dctl help", 2},
	}
	for _, command := range []string{"install", "remove", "search", "list", "update"} {
		for _, args := range [][]string{{command, "--help"}, {command, "-h"}, {"help", command}} {
			cases = append(cases, testCase{args, "Usage: dctl " + command, 0})
		}
	}
	cases = append(cases, testCase{[]string{"install", "ggshield", "--help"}, "Usage: dctl install", 0})
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "must-not-be-created")
			cmd := exec.CommandContext(t.Context(), bash, append([]string{script}, tc.args...)...)
			// No mise, package manager, ambient shell hooks, or credentials.
			cmd.Env = []string{"HOME=" + home, "PATH=" + filepath.Join(dir, "no-binaries")}
			output, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if code != tc.code || !strings.Contains(string(output), tc.want) {
				t.Fatalf("exit %d, want %d containing %q: %s", code, tc.code, tc.want, output)
			}
			if strings.Contains(strings.ToLower(string(output)), "nix") || strings.Contains(string(output), "Invalid tool") {
				t.Fatalf("obsolete/incorrect help: %s", output)
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("help touched HOME: %v", err)
			}
		})
	}
}
