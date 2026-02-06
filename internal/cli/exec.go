package cli

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/ctourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newExecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec <target>",
		Short: "Debug a running container",
		Long: `Debug a running container by launching a sidecar with shared namespaces.

Target formats:
  <container>                     Docker container (default runtime)
  docker://<container>            Docker container
  containerd://<container>        containerd container
  nerdctl://<container>           containerd container (alias)
  k8s://<pod>                     Kubernetes pod (default namespace)
  k8s://<namespace>/<pod>         Kubernetes pod (specific namespace)
  k8s://<ns>/<pod>/<container>    Kubernetes pod (specific container)`,
		Args: cobra.ExactArgs(1),
		RunE: runExec,
	}

	cmd.Flags().String("kubeconfig", "", "Override kubeconfig path")

	return cmd
}

func runExec(cmd *cobra.Command, args []string) error {
	target, err := runtime.ParseTarget(args[0])
	if err != nil {
		return fmt.Errorf("invalid target: %w", err)
	}

	image := flagImage
	if image == "" {
		image = runtime.DefaultImage
	}

	opts := runtime.DebugOpts{
		Image:      image,
		Privileged: flagPrivileged,
		User:       flagUser,
		AutoRemove: flagRemove,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	switch target.Runtime {
	case "docker":
		return runtime.DockerExec(ctx, target, opts)
	case "containerd":
		return runtime.ContainerdExec(ctx, target, opts)
	case "kubernetes":
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		opts.Kubeconfig = kubeconfig
		return runtime.KubernetesExec(ctx, target, opts)
	default:
		return fmt.Errorf("unsupported runtime: %s", target.Runtime)
	}
}
