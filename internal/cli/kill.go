package cli

import (
	"context"
	"fmt"
	"sort"
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
pods that were kept with --keep or already expired via their TTL.`,
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
	configureTargetCompletion(cmd)

	return cmd
}

func runKill(cmd *cobra.Command, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	// Determine runtime from args or default to Docker. If Kubernetes-only flags
	// are present without a target, prefer Kubernetes for --all/interactive kill.
	rt := "docker"
	kubernetesFlagsSet := flagChanged(cmd, "context") || flagChanged(cmd, "kubeconfig") || flagChanged(cmd, "namespace")
	if kubernetesFlagsSet {
		rt = "kubernetes"
	}
	if len(args) > 0 {
		target, err := runtime.ParseTarget(args[0])
		if err != nil {
			return fmt.Errorf("invalid target: %w", err)
		}
		rt = target.Runtime
		if rt != "kubernetes" && kubernetesFlagsSet {
			return fmt.Errorf("--context, --kubeconfig, and --namespace are only supported for Kubernetes targets; use k8s://... or remove the flag")
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
			return killInteractive(ctx, cmd, rt, kubeContext, target.Namespace)
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

	// No target, no --all: show interactive picker
	return killInteractive(ctx, cmd, "", flagKubeContext, flagNamespace)
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

// killInteractive shows a picker over active debug sessions. rt scopes the
// search to one runtime ("docker"/"kubernetes"); empty tries Docker first,
// then Kubernetes (or Kubernetes first when kube flags were passed).
func killInteractive(ctx context.Context, cmd *cobra.Command, rt, kubeContext, namespace string) error {
	preferKubernetes := rt == "kubernetes" ||
		flagChanged(cmd, "context") || flagChanged(cmd, "kubeconfig") || flagChanged(cmd, "namespace")

	var dockerErr, k8sErr error

	if rt != "kubernetes" && !preferKubernetes {
		var containers []runtime.ContainerInfo
		containers, dockerErr = runtime.DockerList(ctx)
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
	}

	if rt == "" || rt == "kubernetes" {
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		var pods []runtime.PodInfo
		pods, k8sErr = runtime.KubernetesList(ctx, kubeconfig, kubeContext, namespace)
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
						Label: formatK8sPodLabel(p, true),
						Value: p.Name,
					}
				}

				name, err := picker.Pick("Select a debug session to kill", items)
				if err != nil {
					return err
				}
				target := &runtime.Target{
					Runtime:   "kubernetes",
					Context:   kubeContext,
					Namespace: namespace,
					Name:      name,
				}
				return runtime.KubernetesKill(ctx, target, kubeconfig, kubeContext)
			}
		}
	}

	// Distinguish "looked and found nothing" from "could not look at all".
	var problems []string
	if dockerErr != nil {
		problems = append(problems, fmt.Sprintf("Docker: %v", dockerErr))
	}
	if k8sErr != nil {
		problems = append(problems, fmt.Sprintf("Kubernetes: %v", k8sErr))
	}
	if len(problems) > 0 {
		return fmt.Errorf("no running debux sessions found, but some runtimes could not be checked:\n  %s", strings.Join(problems, "\n  "))
	}
	return fmt.Errorf("no running debux sessions found")
}
