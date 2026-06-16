package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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
cannot be removed from the pod spec, so debux terminates their process instead.
Kubernetes copy pods (--copy) are deleted outright; --all also sweeps copy
pods that were kept with --keep or already expired via their TTL.

With no target, debux opens the same active-session picker used by attach/list,
including recent Kubernetes contexts and namespaces from debux history. Use
--all-namespaces to broaden the Kubernetes picker when RBAC allows it.`,
		Example: `  # Pick an active session to stop
  debux kill

  # Stop a Docker debug sidecar
  debux kill docker://my-app

  # Stop the debux ephemeral container on a pod
  debux kill k8s://prod/api-pod
  debux kill k8s://api-pod -n prod
  debux kill k8s://@eks-preprod-01/prod/api-pod

  # Delete a kept --copy debug pod
  debux kill k8s://prod/debux-copy-abc12

  # Pick across every namespace in a context
  debux kill --context eks-preprod-01 --all-namespaces

  # Stop all sessions in a namespace (includes kept/expired copy pods)
  debux kill k8s://prod/ --all
  debux kill --all --namespace prod
  debux kill k8s://@eks-preprod-01/prod/ --all`,
		Args:          cobra.MaximumNArgs(1),
		RunE:          runKill,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	addKubernetesFlags(cmd)
	cmd.Flags().BoolVar(&flagKillAll, "all", false, "Kill all running debux sessions for the selected runtime")
	cmd.Flags().BoolVarP(&flagAllNamespaces, "all-namespaces", "A", false, "Kubernetes: include sessions across all namespaces in the picker")
	configureTargetCompletion(cmd)

	return cmd
}

func runKill(cmd *cobra.Command, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	// Determine runtime from args or default to Docker. If Kubernetes-only flags
	// are present without a target, prefer Kubernetes for --all/interactive kill.
	rt := "docker"
	kubernetesFlagsSet := flagChanged(cmd, "context") || flagChanged(cmd, "kubeconfig") || flagChanged(cmd, "namespace") || flagAllNamespaces
	if kubernetesFlagsSet {
		rt = "kubernetes"
	}
	if flagAllNamespaces && flagNamespace != "" {
		return fmt.Errorf("--all-namespaces cannot be combined with namespace %q", flagNamespace)
	}
	if flagAllNamespaces && flagKillAll {
		return fmt.Errorf("--all-namespaces is only supported for the interactive kill picker; use --namespace with --all to sweep one namespace")
	}
	if len(args) > 0 {
		target, err := runtime.ParseTarget(args[0])
		if err != nil {
			return fmt.Errorf("invalid target: %w", err)
		}
		rt = target.Runtime
		if rt != "kubernetes" && kubernetesFlagsSet {
			return fmt.Errorf("--context, --kubeconfig, --namespace, and --all-namespaces are only supported for Kubernetes targets; use k8s://... or remove the flag")
		}
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

		if flagAllNamespaces && target.Namespace != "" {
			return fmt.Errorf("--all-namespaces cannot be combined with namespace %q", target.Namespace)
		}

		if flagKillAll {
			// --all kills every session in scope; combining it with a specific
			// target name would silently ignore the name and kill far more
			// than the user asked for.
			if target.Name != "" {
				return fmt.Errorf("--all kills every session in scope and cannot be combined with target %q; drop the target name (e.g. k8s://<namespace>/ --all) or remove --all", target.Name)
			}
			return killAll(ctx, cmd, rt, kubeContext, target.Namespace)
		}

		// An empty target name (docker:// or k8s://ns/) opens the session
		// picker scoped to that runtime and namespace.
		if target.Name == "" {
			return killInteractive(ctx, cmd, rt, kubeContext, target.Namespace, flagAllNamespaces)
		}

		// Kill specific target
		switch rt {
		case "docker":
			return runtime.DockerKill(ctx, target.Name)
		case "kubernetes":
			kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
			return runtime.KubernetesKill(ctx, target, kubeconfig, kubeContext)
		default:
			return fmt.Errorf("kill is not supported for runtime %q", rt)
		}
	}

	if flagKillAll {
		return killAll(ctx, cmd, rt, flagKubeContext, flagNamespace)
	}

	// No target, no --all: show interactive picker across Docker and the
	// remembered Kubernetes scopes unless Kubernetes flags made the scope exact.
	pickerRuntime := ""
	if kubernetesFlagsSet {
		pickerRuntime = "kubernetes"
	}
	return killInteractive(ctx, cmd, pickerRuntime, flagKubeContext, flagNamespace, flagAllNamespaces)
}

func killAll(ctx context.Context, cmd *cobra.Command, rt string, kubeContext string, namespace string) error {
	switch rt {
	case "docker":
		return runtime.DockerKillAll(ctx)
	case "kubernetes":
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		return runtime.KubernetesKillAll(ctx, kubeconfig, kubeContext, namespace)
	default:
		return fmt.Errorf("kill --all is not supported for runtime %q", rt)
	}
}

// killInteractive shows a picker over exact active debux sessions. rt scopes
// the search to one runtime ("docker"/"kubernetes"); empty includes both.
func killInteractive(ctx context.Context, cmd *cobra.Command, rt, kubeContext, namespace string, allNamespaces bool) error {
	sessions, problems := collectDebugSessions(ctx, cmd, rt, kubeContext, namespace, allNamespaces)
	if len(sessions) == 0 {
		if len(problems) > 0 {
			return fmt.Errorf("no running debux sessions found, but some runtimes could not be checked:\n  %s", strings.Join(errorStrings(problems), "\n  "))
		}
		return fmt.Errorf("no running debux sessions found")
	}

	items := make([]picker.Item, len(sessions))
	for i, session := range sessions {
		items[i] = picker.Item{Label: formatDebugSessionLabel(session), Value: fmt.Sprintf("%d", i)}
	}

	chosen, err := picker.Pick("Select a debug session to kill", items)
	if err != nil {
		return err
	}
	idx, err := strconv.Atoi(chosen)
	if err != nil || idx < 0 || idx >= len(sessions) {
		return fmt.Errorf("invalid session selection %q", chosen)
	}

	return killDebugSession(ctx, cmd, sessions[idx])
}

func killDebugSession(ctx context.Context, cmd *cobra.Command, session runtime.DebugSessionInfo) error {
	target, err := runtime.ParseTarget(session.Target)
	if err != nil {
		return fmt.Errorf("invalid session target %q: %w", session.Target, err)
	}

	switch target.Runtime {
	case "docker":
		return runtime.DockerKill(ctx, target.Name)
	case "kubernetes":
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		kubeContext := target.Context
		if kubeContext == "" {
			kubeContext = session.Context
		}
		return runtime.KubernetesKill(ctx, target, kubeconfig, kubeContext)
	default:
		return fmt.Errorf("kill is not supported for runtime %q", target.Runtime)
	}
}
