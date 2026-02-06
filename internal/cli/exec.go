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

func newExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "exec [target]",
		Short:  "Debug a running container",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE:   runExec,
	}
}

func runExec(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var target *runtime.Target

	if len(args) == 0 {
		// No args: default to Docker, show picker
		target = &runtime.Target{Runtime: "docker"}
	} else {
		var err error
		target, err = runtime.ParseTarget(args[0])
		if err != nil {
			return fmt.Errorf("invalid target: %w", err)
		}
	}

	// If name is empty, show interactive picker for the runtime
	if target.Name == "" {
		name, err := pickTarget(ctx, cmd, target)
		if err != nil {
			return err
		}
		target.Name = name
	}

	profile, err := resolveProfile(cmd)
	if err != nil {
		return err
	}

	image := flagImage
	if image == "" {
		image = runtime.DefaultImage
	}

	opts := runtime.DebugOpts{
		Image:        image,
		Privileged:   flagPrivileged,
		User:         flagUser,
		AutoRemove:   flagRemove,
		ShareVolumes: !flagNoVolumes,
		PullPolicy:   flagPullPolicy,
		Fresh:        flagFresh,
		Profile:      profile,
	}

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

func pickTarget(ctx context.Context, cmd *cobra.Command, target *runtime.Target) (string, error) {
	switch target.Runtime {
	case "docker":
		return pickDockerContainer(ctx)
	case "kubernetes":
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		return pickK8sPod(ctx, kubeconfig, target.Namespace)
	default:
		return "", fmt.Errorf("interactive selection is not supported for runtime %q", target.Runtime)
	}
}

func pickDockerContainer(ctx context.Context) (string, error) {
	containers, err := runtime.DockerList(ctx)
	if err != nil {
		return "", err
	}
	if len(containers) == 0 {
		return "", fmt.Errorf("no running Docker containers found")
	}

	// Sort: active debux sessions first
	sort.SliceStable(containers, func(i, j int) bool {
		return containers[i].HasDebuxSession && !containers[j].HasDebuxSession
	})

	items := make([]picker.Item, len(containers))
	for i, c := range containers {
		label := fmt.Sprintf("%s (%s) — %s", c.Name, c.Image, c.Status)
		if c.HasDebuxSession {
			label = "● " + label
		}
		items[i] = picker.Item{
			Label: label,
			Value: c.Name,
		}
	}

	return picker.Pick("Select a container", items)
}

func pickK8sPod(ctx context.Context, kubeconfig, namespace string) (string, error) {
	pods, err := runtime.KubernetesList(ctx, kubeconfig, namespace)
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("no running pods found")
	}

	// Sort: active debux sessions first
	sort.SliceStable(pods, func(i, j int) bool {
		return pods[i].HasDebuxSession && !pods[j].HasDebuxSession
	})

	items := make([]picker.Item, len(pods))
	for i, p := range pods {
		label := fmt.Sprintf("%s/%s [%s]", p.Namespace, p.Name, strings.Join(p.Containers, ", "))
		if p.HasDebuxSession {
			label = "● " + label
		}
		items[i] = picker.Item{
			Label: label,
			Value: p.Name,
		}
	}

	return picker.Pick("Select a pod", items)
}
