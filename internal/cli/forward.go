package cli

import (
	"fmt"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newForwardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forward <target> [LOCAL_PORT:]REMOTE_PORT [[LOCAL_PORT:]REMOTE_PORT...]",
		Short: "Forward local ports to a container or pod",
		Long: `Forward local ports to a running target without restarting it.

Docker: debux runs a small socat relay on the target's network and publishes
the requested ports on 127.0.0.1 — useful for containers started without -p.
Kubernetes: debux uses the pod port-forward API (same as kubectl port-forward).`,
		Example: `  debux forward my-app 8080:80
  debux forward docker://my-app 5432
  debux forward compose://web 8080:3000
  debux forward k8s://prod/api-pod 8080:8080 9090:9090
  debux forward k8s://api-pod -n prod 8080:8080`,
		Args:          cobra.MinimumNArgs(2),
		RunE:          runForward,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	addKubernetesFlags(cmd)
	configureTargetCompletion(cmd)
	return cmd
}

func runForward(cmd *cobra.Command, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	target, err := runtime.ParseTarget(args[0])
	if err != nil {
		return fmt.Errorf("invalid target: %w", err)
	}
	mappings, err := runtime.ParsePortMappings(args[1:])
	if err != nil {
		return err
	}

	switch target.Runtime {
	case "docker":
		if kubernetesFlagsChanged(cmd) {
			return fmt.Errorf("--context, --kubeconfig, and --namespace are only supported for Kubernetes targets; use k8s://... or remove the flag")
		}
		return runtime.DockerForward(ctx, target, mappings, configuredPullPolicy(flagPullPolicy), resolveImage(""))
	case "kubernetes":
		applyKubeNamespaceFlagContainerShorthand(cmd, target)
		kubeContext, err := resolveKubeContext(cmd, target.Context)
		if err != nil {
			return err
		}
		kubeNamespace, err := resolveKubeNamespace(cmd, target.Namespace)
		if err != nil {
			return err
		}
		target.Namespace = kubeNamespace
		if target.Name == "" {
			name, err := pickK8sPod(ctx, completionFlagString(cmd, "kubeconfig"), kubeContext, target.Namespace)
			if err != nil {
				return err
			}
			target.Name = name
		}
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		return runtime.KubernetesForward(ctx, target, kubeconfig, kubeContext, mappings)
	default:
		return fmt.Errorf("forward is not supported for runtime %q", target.Runtime)
	}
}

func kubernetesFlagsChanged(cmd *cobra.Command) bool {
	return flagChanged(cmd, "context") || flagChanged(cmd, "kubeconfig") || flagChanged(cmd, "namespace")
}
