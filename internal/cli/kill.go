package cli

import (
	"context"
	"fmt"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/clement-tourriere/debux/internal/picker"
	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

var flagKillAll bool

func newKillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kill [target]",
		Short: "Stop running debux debug sessions",
		Long: `Stop debux debug sessions.

Docker debug sessions are sidecar containers. Kubernetes ephemeral containers
cannot be removed from the pod spec, so debux terminates their process instead.`,
		Example: `  # Pick an active session to stop
  debux kill

  # Stop a Docker debug sidecar
  debux kill docker://my-app

  # Stop the debux ephemeral container on a pod
  debux kill k8s://prod/api-pod

  # Stop all sessions in a namespace
  debux kill k8s://prod/ --all`,
		Args:          cobra.MaximumNArgs(1),
		RunE:          runKill,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	addKubeconfigFlag(cmd)
	cmd.Flags().BoolVar(&flagKillAll, "all", false, "Kill all running debux sessions for the selected runtime")

	return cmd
}

func runKill(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Determine runtime from args or default to docker
	rt := "docker"
	if len(args) > 0 {
		target, err := runtime.ParseTarget(args[0])
		if err != nil {
			return fmt.Errorf("invalid target: %w", err)
		}
		rt = target.Runtime

		if flagKillAll {
			return killAll(ctx, cmd, rt, target.Namespace)
		}

		// Kill specific target
		switch rt {
		case "docker":
			return runtime.DockerKill(ctx, target.Name)
		case "kubernetes":
			kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
			return runtime.KubernetesKill(ctx, target, kubeconfig)
		default:
			return fmt.Errorf("kill is not supported for runtime %q", rt)
		}
	}

	if flagKillAll {
		return killAll(ctx, cmd, rt, "")
	}

	// No target, no --all: show interactive picker
	return killInteractive(ctx, cmd)
}

func killAll(ctx context.Context, cmd *cobra.Command, rt string, namespace string) error {
	switch rt {
	case "docker":
		return runtime.DockerKillAll(ctx)
	case "kubernetes":
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		return runtime.KubernetesKillAll(ctx, kubeconfig, namespace)
	default:
		return fmt.Errorf("kill --all is not supported for runtime %q", rt)
	}
}

func killInteractive(ctx context.Context, cmd *cobra.Command) error {
	// Try Docker first
	containers, dockerErr := runtime.DockerList(ctx)
	if dockerErr == nil {
		// Filter to only containers with active debux sessions
		var active []runtime.ContainerInfo
		for _, c := range containers {
			if c.HasDebuxSession {
				active = append(active, c)
			}
		}

		if len(active) > 0 {
			sort.SliceStable(active, func(i, j int) bool {
				return active[i].Name < active[j].Name
			})

			items := make([]picker.Item, len(active))
			for i, c := range active {
				items[i] = picker.Item{
					Label: fmt.Sprintf("● %s (%s) — %s", c.Name, c.Image, c.Status),
					Value: c.Name,
				}
			}

			name, err := picker.Pick("Select a debug session to kill", items)
			if err != nil {
				return err
			}
			return runtime.DockerKill(ctx, name)
		}
	}

	// Try K8s
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
	pods, k8sErr := runtime.KubernetesList(ctx, kubeconfig, "")
	if k8sErr == nil {
		var active []runtime.PodInfo
		for _, p := range pods {
			if p.HasDebuxSession {
				active = append(active, p)
			}
		}

		if len(active) > 0 {
			items := make([]picker.Item, len(active))
			for i, p := range active {
				items[i] = picker.Item{
					Label: fmt.Sprintf("● %s/%s [%s]", p.Namespace, p.Name, strings.Join(p.Containers, ", ")),
					Value: p.Name,
				}
			}

			name, err := picker.Pick("Select a debug session to kill", items)
			if err != nil {
				return err
			}
			target := &runtime.Target{
				Runtime: "kubernetes",
				Name:    name,
			}
			return runtime.KubernetesKill(ctx, target, kubeconfig)
		}
	}

	return fmt.Errorf("no running debux sessions found")
}
