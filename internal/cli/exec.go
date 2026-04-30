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
	cmd := &cobra.Command{
		Use:     "exec [target]",
		Aliases: []string{"debug", "shell"},
		Short:   "Debug a running Docker container or Kubernetes pod",
		Long: `Start an interactive debux shell attached to a running target.

With no target, debux opens the Docker picker. Use docker:// or k8s:// to make
the runtime explicit and to open runtime-specific pickers.

Security: the default Kubernetes profile is root inside the pod. Use
--profile=restricted for a non-root, drop-capabilities session.`,
		Example: `  debux exec
  debux exec docker://my-app
  debux exec k8s://
  debux exec k8s://@eks-preprod-01/prod/api-pod/app
  debux exec k8s://prod/webapp-internal-api
  debux exec k8s://prod/api-pod/app --context eks-preprod-01
  debux exec k8s://prod/api-pod/app --profile=restricted
  debux exec k8s://prod/api-pod/app --fresh --pull-policy=Always
  debux exec k8s://prod/api-pod/app --copy`,
		Args: cobra.MaximumNArgs(1),
		RunE: runExec,
	}
	addExecFlags(cmd)
	return cmd
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

	if err := validateExecFlags(cmd, target.Runtime); err != nil {
		return err
	}

	kubeContext, err := resolveKubeContext(cmd, target.Context)
	if err != nil {
		return err
	}
	target.Context = kubeContext

	// If name is empty, show interactive picker for the runtime.
	if target.Name == "" {
		name, err := pickTarget(ctx, cmd, target)
		if err != nil {
			return err
		}
		target.Name = name
	} else if target.Runtime == "kubernetes" {
		name, err := resolveK8sPodName(ctx, cmd, target, kubeContext)
		if err != nil {
			return err
		}
		target.Name = name
	}

	profile := runtime.ProfileGeneral
	if target.Runtime == "kubernetes" {
		var err error
		profile, err = resolveProfile(cmd)
		if err != nil {
			return err
		}
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
		Copy:         flagCopy,
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
		opts.KubeContext = kubeContext
		return runtime.KubernetesExec(ctx, target, opts)
	default:
		return fmt.Errorf("unsupported runtime: %s", target.Runtime)
	}
}

func resolveK8sPodName(ctx context.Context, cmd *cobra.Command, target *runtime.Target, kubeContext string) (string, error) {
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")

	exists, err := runtime.KubernetesPodExists(ctx, kubeconfig, kubeContext, target.Namespace, target.Name)
	if err != nil {
		return "", err
	}
	if exists {
		return target.Name, nil
	}

	matches, err := runtime.KubernetesFindPods(ctx, kubeconfig, kubeContext, target.Namespace, target.Name)
	if err != nil {
		return "", fmt.Errorf("pod %q was not found and listing similar pods failed: %w", target.Name, err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("pod %q was not found and no running pods matched that substring", target.Name)
	}

	name, err := pickK8sPodFromList(fmt.Sprintf("Pod %q not found. Select a matching pod", target.Name), matches)
	if err != nil {
		return "", err
	}
	return name, nil
}

func resolveKubeContext(cmd *cobra.Command, targetContext string) (string, error) {
	if !flagChanged(cmd, "context") {
		return targetContext, nil
	}
	if targetContext != "" && targetContext != flagKubeContext {
		return "", fmt.Errorf("conflicting Kubernetes contexts: target uses %q but --context=%q", targetContext, flagKubeContext)
	}
	return flagKubeContext, nil
}

func validateExecFlags(cmd *cobra.Command, targetRuntime string) error {
	if targetRuntime == "kubernetes" {
		return nil
	}

	var invalid []string
	for _, name := range []string{"copy", "pull-policy", "kubeconfig", "context", "profile"} {
		if flagChanged(cmd, name) {
			invalid = append(invalid, "--"+name)
		}
	}
	if len(invalid) == 1 {
		return fmt.Errorf("%s is only supported for Kubernetes targets; use k8s://... or remove the flag", invalid[0])
	}
	if len(invalid) > 1 {
		return fmt.Errorf("%s are only supported for Kubernetes targets; use k8s://... or remove the flags", strings.Join(invalid, ", "))
	}
	return nil
}

func pickTarget(ctx context.Context, cmd *cobra.Command, target *runtime.Target) (string, error) {
	switch target.Runtime {
	case "docker":
		return pickDockerContainer(ctx)
	case "kubernetes":
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		return pickK8sPod(ctx, kubeconfig, target.Context, target.Namespace)
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

func pickK8sPod(ctx context.Context, kubeconfig, kubeContext, namespace string) (string, error) {
	pods, err := runtime.KubernetesList(ctx, kubeconfig, kubeContext, namespace)
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("no running pods found")
	}

	return pickK8sPodFromList("Select a pod", pods)
}

func pickK8sPodFromList(title string, pods []runtime.PodInfo) (string, error) {
	// Sort: active debux sessions first
	sort.SliceStable(pods, func(i, j int) bool {
		return pods[i].HasDebuxSession && !pods[j].HasDebuxSession
	})

	items := make([]picker.Item, len(pods))
	for i, p := range pods {
		items[i] = picker.Item{
			Label: formatK8sPodLabel(p, p.HasDebuxSession),
			Value: p.Name,
		}
	}

	return picker.Pick(title, items)
}

func formatK8sPodLabel(p runtime.PodInfo, active bool) string {
	label := fmt.Sprintf("%s/%s", p.Namespace, p.Name)
	if len(p.Containers) > 0 {
		label = fmt.Sprintf("%s [%s]", label, strings.Join(p.Containers, ", "))
	}
	if p.Context != "" {
		label = fmt.Sprintf("%s · ctx: %s", label, p.Context)
	}
	if active {
		label = "● " + label
	}
	return label
}
