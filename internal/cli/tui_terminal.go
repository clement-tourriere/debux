package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/clement-tourriere/debux/internal/config"
	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func configuredTerminal() string {
	if t := strings.TrimSpace(os.Getenv("DEBUX_TERMINAL")); t != "" {
		return t
	}
	return strings.TrimSpace(config.Get().Terminal)
}

func openInTerminal(ctx context.Context, cmd *cobra.Command, launch tuiLaunch) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving debux executable: %w", err)
	}
	commandLine := terminalShellCommand(append([]string{exe}, buildExecArgs(cmd, launch)...))

	terminal := configuredTerminal()
	if terminal == "" {
		return fmt.Errorf("external launch is disabled by default; press enter to open in the current terminal, or set DEBUX_TERMINAL (or `terminal:` in the config file)")
	}
	if err := startConfiguredTerminal(ctx, terminal, commandLine); err != nil {
		return err
	}
	fmt.Printf("Opened %s with DEBUX_TERMINAL\n", launch.target)
	return nil
}

func startConfiguredTerminal(ctx context.Context, terminal, commandLine string) error {
	if strings.Contains(terminal, "{command}") {
		cmdline := strings.ReplaceAll(terminal, "{command}", terminalShellQuote(commandLine))
		proc := exec.CommandContext(ctx, "sh", "-lc", cmdline)
		return startTerminalProcess(proc)
	}
	return startTerminalExecutable(ctx, terminal, commandLine)
}

func startTerminalExecutable(ctx context.Context, terminal, commandLine string) error {
	fields := strings.Fields(terminal)
	if len(fields) == 0 {
		return fmt.Errorf("empty terminal command")
	}
	path, err := exec.LookPath(fields[0])
	if err != nil {
		return err
	}
	args := terminalArgs(filepath.Base(fields[0]), fields[1:], commandLine)
	proc := exec.CommandContext(ctx, path, args...)
	return startTerminalProcess(proc)
}

func startTerminalProcess(proc *exec.Cmd) error {
	if err := proc.Start(); err != nil {
		return err
	}
	go func() { _ = proc.Wait() }() // reap the launcher without blocking the TUI
	return nil
}

func terminalArgs(name string, prefix []string, commandLine string) []string {
	args := append([]string(nil), prefix...)
	switch name {
	case "wezterm":
		return append(args, "start", "--", "sh", "-lc", commandLine)
	case "kitty":
		return append(args, "sh", "-lc", commandLine)
	case "gnome-terminal":
		return append(args, "--", "sh", "-lc", commandLine)
	default:
		return append(args, "-e", "sh", "-lc", commandLine)
	}
}

func terminalShellCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = terminalShellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func terminalShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func buildExecArgs(cmd *cobra.Command, launch tuiLaunch) []string {
	if launch.session != nil {
		args := debugSessionAttachArgs(*launch.session)
		if kubeconfig, _ := cmd.Flags().GetString("kubeconfig"); kubeconfig != "" {
			args = append(args, "--kubeconfig", kubeconfig)
		}
		return args
	}
	args := []string{"exec", launch.target}
	isKubernetes := strings.HasPrefix(launch.target, "k8s://")
	if launch.image != "" {
		args = append(args, "--image", launch.image)
	}
	if launch.fresh {
		args = append(args, "--fresh")
	}
	if !launch.shareVolumes {
		args = append(args, "--no-volumes")
	}
	if launch.readOnlyVolumes {
		args = append(args, "--read-only-volumes")
	}
	if launch.user != "" {
		args = append(args, "--user", launch.user)
	}
	if launch.privileged {
		args = append(args, "--privileged")
	}
	if launch.pullPolicy != "" {
		args = append(args, "--pull-policy", launch.pullPolicy)
	}
	// Repeatable flags from the original command line must survive an
	// external-terminal launch, or it behaves differently from an
	// in-terminal (enter) launch that reuses the flag variables directly.
	for _, env := range flagStrings(cmd, "env") {
		args = append(args, "--env", env)
	}
	for _, cap := range flagStrings(cmd, "cap-add") {
		args = append(args, "--cap-add", cap)
	}
	for _, tool := range flagStrings(cmd, "tools") {
		args = append(args, "--tools", tool)
	}
	if isKubernetes {
		if launch.copy {
			args = append(args, "--copy")
			if flagBool(cmd, "keep") {
				args = append(args, "--keep")
			}
			if flagString(cmd, "ttl") != "" {
				args = append(args, "--ttl", flagString(cmd, "ttl"))
			}
		}
		if launch.profile != "" && !launch.privileged {
			args = append(args, "--profile", launch.profile)
		}
		if kubeconfig, _ := cmd.Flags().GetString("kubeconfig"); kubeconfig != "" {
			args = append(args, "--kubeconfig", kubeconfig)
		}
		if flagString(cmd, "context") != "" && !strings.Contains(launch.target, "k8s://@") {
			args = append(args, "--context", flagString(cmd, "context"))
		}
		if flagString(cmd, "namespace") != "" && strings.Count(strings.TrimPrefix(launch.target, "k8s://"), "/") == 0 {
			args = append(args, "--namespace", flagString(cmd, "namespace"))
		}
	}
	return args
}

func formatTargetURI(target *runtime.Target) string {
	switch target.Runtime {
	case "docker":
		if target.PreferPodman {
			return "podman://" + target.Name
		}
		return "docker://" + target.Name
	case "containerd":
		return "containerd://" + target.Name
	case "kubernetes":
		parts := make([]string, 0, 4)
		prefix := "k8s://"
		if target.Context != "" {
			prefix += "@" + url.PathEscape(target.Context)
		}
		if target.Namespace != "" {
			parts = append(parts, url.PathEscape(target.Namespace))
		}
		if target.Name != "" {
			parts = append(parts, url.PathEscape(target.Name))
		}
		// Without a namespace, k8s://<pod>/<container> would re-parse as
		// k8s://<namespace>/<pod>; drop the container rather than emit an
		// ambiguous URI.
		if target.Container != "" && (target.Namespace != "" || target.Name == "") {
			parts = append(parts, url.PathEscape(target.Container))
		}
		if len(parts) == 0 {
			return prefix
		}
		if target.Context != "" {
			return prefix + "/" + strings.Join(parts, "/")
		}
		return prefix + strings.Join(parts, "/")
	default:
		return target.Name
	}
}
