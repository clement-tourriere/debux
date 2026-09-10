package entrypoint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func shellFixture(t *testing.T) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh is required for behavioral shell tests (installed in CI)")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	wrappers := filepath.Join(dir, "wrappers")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	config := strings.ReplaceAll(ShellConfig, "/tmp/debux-target-bin", wrappers)
	config = strings.ReplaceAll(config, "/tmp/.zshrc", filepath.Join(dir, "other.zshrc"))
	config = strings.ReplaceAll(config, "/nix/var/debux-profile/bin:/usr/local/bin:", bin+":")
	rc := filepath.Join(dir, "zshrc")
	if err := os.WriteFile(rc, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, bin, rc
}

func runShell(t *testing.T, dir, rc, script string, extra ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "zsh", "-df", "-c", `source "$1"; `+script, "test", rc)
	// Do not inherit development credentials, shell hooks, or target variables.
	cmd.Env = append([]string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "TERM=dumb", "DEBUX_TARGET_ROOT=/dev/null", "DEBUX_TARGET_ENVIRON=/dev/null", "DEBUX_TARGET_CWD_LINK=/dev/null", "DEBUX_CONTEXT=safe"}, extra...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("zsh: %v\n%s", err, output)
	}
	return string(output)
}

func TestShellIsolatesTargetEnvironmentAndExecutesSafeWrappers(t *testing.T) {
	dir, bin, rc := shellFixture(t)
	root := filepath.Join(dir, "target")
	targetBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(targetBin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"debux-test-tool", "bad;injection"} {
		if err := os.WriteFile(filepath.Join(targetBin, name), []byte("not executed directly"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	environ := filepath.Join(dir, "environ")
	if err := os.WriteFile(environ, []byte("PATH=/bin\x00PYTHONHOME=/hostile\x00GCONV_PATH=/hostile\x00DEBUX_CONTEXT=untrusted\x00BAD-KEY=value\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "chroot-args")
	mock := fmt.Sprintf("#!/bin/sh\n[ \"$PYTHONHOME\" = /hostile ] || exit 51\n[ \"$GCONV_PATH\" = /hostile ] || exit 52\n[ \"$TERM\" = xterm ] || exit 53\nprintf '%%s\\n' \"$@\" > %q\n", record)
	if err := os.WriteFile(filepath.Join(bin, "chroot"), []byte(mock), 0o700); err != nil {
		t.Fatal(err)
	}
	runShell(t, dir, rc, `[[ -z ${PYTHONHOME:-} && -z ${GCONV_PATH:-} && $DEBUX_CONTEXT == safe && $TERM == xterm ]] || exit 41; command debux-test-tool "hello world"`, "DEBUX_TARGET_ROOT="+root, "DEBUX_TARGET_ENVIRON="+environ)
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if want := "--skip-chdir\n" + root + "\n/bin/debux-test-tool\nhello world\n"; string(got) != want {
		t.Fatalf("unsafe/wrong chroot arguments: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "wrappers", "bad;injection")); !os.IsNotExist(err) {
		t.Fatal("unsafe target name became executable wrapper")
	}
}

func TestShellWrapperDiscoveryHandlesSidecarPathEntries(t *testing.T) {
	for _, kind := range []string{"empty", "directory", "dangling-symlink", "regular", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir, bin, rc := shellFixture(t)
			root := filepath.Join(dir, "target")
			targetBin := filepath.Join(root, "bin")
			if err := os.MkdirAll(targetBin, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"debux-target-tool", "debux-sidecar-tool"} {
				if err := os.WriteFile(filepath.Join(targetBin, name), nil, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			sidecarTool := filepath.Join(bin, "debux-sidecar-tool")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(sidecarTool, 0o700)
			case "dangling-symlink":
				err = os.Symlink(filepath.Join(dir, "missing"), sidecarTool)
			case "regular":
				err = os.WriteFile(sidecarTool, nil, 0o700)
			case "symlink":
				err = os.Symlink(filepath.Join(targetBin, "debux-sidecar-tool"), sidecarTool)
			}
			if err != nil {
				t.Fatal(err)
			}
			environ := filepath.Join(dir, "environ")
			if err := os.WriteFile(environ, []byte("PATH=/bin\x00"), 0o600); err != nil {
				t.Fatal(err)
			}

			// An empty tool directory is normal before the first dctl install.
			// Do not depend on the developer's PATH containing one to catch NOMATCH.
			output := runShell(t, dir, rc, `[[ $(command -v debux-target-tool) == "$HOME/wrappers/debux-target-tool" ]] || exit 41`,
				"PATH="+bin+":/usr/bin:/bin", "DEBUX_TARGET_ROOT="+root, "DEBUX_TARGET_ENVIRON="+environ)
			if output != "" {
				t.Fatalf("wrapper discovery should be quiet, got: %s", output)
			}
			_, err = os.Stat(filepath.Join(dir, "wrappers", "debux-sidecar-tool"))
			if kind == "regular" || kind == "symlink" {
				if !os.IsNotExist(err) {
					t.Fatalf("sidecar command must not be shadowed by a target wrapper: %v", err)
				}
			} else if err != nil {
				t.Fatalf("non-command PATH entry must not prevent a target wrapper: %v", err)
			}
		})
	}
}

func TestFailedToolInstallationRetriesInsteadOfMarkingSuccess(t *testing.T) {
	dir, bin, rc := shellFixture(t)
	scripts := map[string]string{
		"sha256sum": "#!/bin/sh\ncat >/dev/null\nprintf 'fixed-digest  -\\n'\n",
		"dctl":      "#!/bin/sh\necho attempt >> \"$HOME/attempts\"\ntest -f \"$HOME/permit-success\"\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runShell(t, dir, rc, "true", "DEBUX_TOOLS=gdb")
	runShell(t, dir, rc, "true", "DEBUX_TOOLS=gdb")
	if err := os.WriteFile(filepath.Join(dir, "permit-success"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runShell(t, dir, rc, "true", "DEBUX_TOOLS=gdb")
	runShell(t, dir, rc, "true", "DEBUX_TOOLS=gdb")
	attempts, err := os.ReadFile(filepath.Join(dir, "attempts"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(attempts), "attempt") != 3 {
		t.Fatalf("failed install not retried or successful install not cached: %s", attempts)
	}
}

func TestImageScriptPreservesSavedTargetEnvironment(t *testing.T) {
	start := strings.Index(ImageScript, "export DEBUX_TARGET_ROOT=")
	end := strings.Index(ImageScript, "# Ensure persistent data directory")
	if start < 0 || end <= start {
		t.Fatal("missing image target initialization")
	}
	for _, value := range []string{"", "/tmp/saved-target-environ"} {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", ImageScript[start:end]+`printf '%s' "$DEBUX_TARGET_ENVIRON"`)
		cmd.Env = []string{"DEBUX_TARGET_ENVIRON=" + value}
		output, err := cmd.Output()
		want := value
		if want == "" {
			want = "/dev/null"
		}
		if err != nil || string(output) != want {
			t.Fatalf("image initialization discarded saved environment: %q %v", output, err)
		}
	}
}

func TestCanonicalShellConfigIsUsedEverywhere(t *testing.T) {
	for name, script := range map[string]string{"injected": Script, "image": ImageScript, "bootstrap": ShellBootstrapScript()} {
		if strings.Contains(script, "__DEBUX_ZSHRC__") || heredocContent(script, "ZSHRC_EOF") != strings.TrimSuffix(ShellConfig, "\n") {
			t.Fatalf("%s shell config drifted", name)
		}
	}
	// Match the actual image build's awk substitution without building an image.
	cmd := exec.CommandContext(t.Context(), "awk", `{if ($0 == "__DEBUX_ZSHRC__") {while ((getline line < "zshrc") > 0) print line} else print}`, "script.sh")
	rendered, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(rendered) != Script {
		t.Fatal("Dockerfile rendering differs from Go embedding")
	}
}
