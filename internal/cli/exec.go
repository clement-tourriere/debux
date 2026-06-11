package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/config"
	"github.com/clement-tourriere/debux/internal/history"
	"github.com/clement-tourriere/debux/internal/picker"
	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newExecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "exec [target] [-- command...]",
		Aliases: []string{"debug", "shell"},
		Short:   "Debug a running Docker container or Kubernetes pod",
		Long: `Start an interactive debux shell attached to a running target, or run a one-shot
command after --.

With no target, debux opens the Docker picker. Use docker:// or k8s:// to make
the runtime explicit and to open runtime-specific pickers.

Security: the default Kubernetes profile is root inside the pod. Use
--profile=restricted for a non-root, drop-capabilities session.`,
		Example: `  debux exec
  debux exec docker://my-app
  debux exec k8s://
  debux exec k8s://@eks-preprod-01/prod/api-pod/app
  debux exec k8s://prod/webapp-internal-api
  debux exec k8s://api-pod/app --namespace prod
  debux exec k8s://prod/api-pod/app --context eks-preprod-01
  debux exec k8s://prod/api-pod/app --profile=restricted
  debux exec k8s://prod/api-pod/app --fresh --pull-policy=Always
  debux exec k8s://prod/api-pod/app --read-only-volumes
  debux exec k8s://prod/api-pod/app --copy
  debux exec k8s://prod/api-pod/app --copy --keep --ttl=48h
  debux exec docker://my-app -- curl -I localhost
  debux exec k8s://prod/api-pod/app -- ps aux`,
		Args: cobra.ArbitraryArgs,
		RunE: runExec,
	}
	addExecFlags(cmd)
	configureTargetCompletion(cmd)
	return cmd
}

func runExec(cmd *cobra.Command, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	var target *runtime.Target
	var command []string

	// Honor the documented `debux [target] [-- command...]` form: everything
	// after -- is the command, even when the target is omitted.
	targetArgs := args
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		targetArgs = args[:dash]
		command = args[dash:]
	} else if len(args) > 1 {
		targetArgs = args[:1]
		command = args[1:]
	}

	switch len(targetArgs) {
	case 0:
		// No target: default to Docker, show picker
		target = &runtime.Target{Runtime: "docker"}
	case 1:
		var err error
		target, err = runtime.ParseTarget(targetArgs[0])
		if err != nil {
			return fmt.Errorf("invalid target: %w", err)
		}
	default:
		return fmt.Errorf("expected at most one target before --, got %d arguments", len(targetArgs))
	}

	if err := validateExecFlags(cmd, target.Runtime); err != nil {
		return err
	}

	// Parse --ttl before any pickers or cluster roundtrips so a typo fails fast.
	var copyTTL time.Duration
	if flagCopy {
		ttl, err := parseCopyPodTTL(flagTTL)
		if err != nil {
			return err
		}
		copyTTL = ttl
	}

	applyKubeNamespaceFlagContainerShorthand(cmd, target)

	kubeContext, err := resolveKubeContext(cmd, target.Context)
	if err != nil {
		return err
	}
	target.Context = kubeContext

	kubeNamespace, err := resolveKubeNamespace(cmd, target.Namespace)
	if err != nil {
		return err
	}
	target.Namespace = kubeNamespace

	// If name is empty, show interactive picker for the runtime — unless a
	// compose service was given, which the Docker runtime resolves to a
	// concrete container itself.
	if target.Name == "" && target.ComposeService == "" {
		name, err := pickTarget(ctx, cmd, target)
		if err != nil {
			return err
		}
		target.Name = name
	} else if target.Name != "" && target.Runtime == "kubernetes" {
		name, err := resolveK8sPodName(ctx, cmd, target, kubeContext)
		if err != nil {
			return err
		}
		target.Name = name
	}

	// In copy mode the runtime resolves the container from the pod spec, so a
	// crash-looping pod (no running containers) is still a valid target.
	if target.Runtime == "kubernetes" && target.Container == "" && !flagCopy {
		containerName, err := resolveK8sContainerName(ctx, cmd, target, kubeContext)
		if err != nil {
			return err
		}
		target.Container = containerName
	}

	profile := runtime.ProfileGeneral
	if target.Runtime == "kubernetes" {
		var err error
		profile, err = resolveProfile(cmd)
		if err != nil {
			return err
		}
	}

	pullPolicy, err := resolvePullPolicy(configuredPullPolicy(flagPullPolicy))
	if err != nil {
		return err
	}

	if err := runtime.ValidateEnvVars(flagEnv); err != nil {
		return err
	}

	opts := runtime.DebugOpts{
		Image:           resolveImage(flagImage),
		Privileged:      flagPrivileged,
		User:            flagUser,
		ShareVolumes:    !flagNoVolumes,
		ReadOnlyVolumes: flagReadOnlyVolumes,
		PullPolicy:      pullPolicy,
		Fresh:           flagFresh,
		Copy:            flagCopy,
		Keep:            flagKeep,
		TTL:             copyTTL,
		Profile:         profile,
		Command:         command,
		Env:             flagEnv,
		CapAdd:          flagCapAdd,
		Tools:           config.ResolveTools(flagTools),
	}

	recordDebugHistory(target, opts)

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

// parseCopyPodTTL parses --ttl. "0" disables the deadline; anything else must
// be a Go duration of at least one second (activeDeadlineSeconds granularity).
func parseCopyPodTTL(value string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid --ttl %q: use a Go duration like 30m, 8h, 2h45m, or 0 to disable", value)
	}
	if d == 0 {
		return 0, nil
	}
	if d < time.Second {
		return 0, fmt.Errorf("invalid --ttl %q: must be at least 1s (or 0 to disable)", value)
	}
	return d, nil
}

func resolveK8sContainerName(ctx context.Context, cmd *cobra.Command, target *runtime.Target, kubeContext string) (string, error) {
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
	containers, err := runtime.KubernetesRunningContainers(ctx, kubeconfig, kubeContext, target.Namespace, target.Name)
	if err != nil {
		return "", err
	}
	if len(containers) == 1 {
		return containers[0], nil
	}
	items := make([]picker.Item, len(containers))
	for i, name := range containers {
		items[i] = picker.Item{Label: name, Value: name}
	}
	return picker.Pick(fmt.Sprintf("Select a container in %s", target.Name), items)
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
		if (flagChanged(cmd, "keep") || flagChanged(cmd, "ttl")) && !flagCopy {
			return fmt.Errorf("--keep and --ttl are only supported with --copy: ephemeral debug containers live inside the target pod and cannot outlive it")
		}
		return nil
	}

	var invalid []string
	for _, name := range []string{"copy", "keep", "ttl", "kubeconfig", "context", "namespace", "profile"} {
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

func recordDebugHistory(target *runtime.Target, opts runtime.DebugOpts) {
	if target == nil || target.Name == "" {
		return
	}
	_ = history.Append(history.NewEntry(target, formatTargetURI(target), opts, "cli"))
}
