package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newPodCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pod",
		Short: "Create a standalone Kubernetes debug pod",
		Long: `Create a standalone debux toolbox pod in a Kubernetes cluster.

This is useful for cluster-level network or DNS debugging when you do not need
to attach to a specific application pod.`,
		Example: `  debux pod -n prod
  debux pod -n prod --host-network
  debux pod -n prod --keep
  debux pod -n prod --profile=netadmin`,
		RunE: runPod,
	}

	addPodDebugFlags(cmd)
	cmd.Flags().StringP("namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().Bool("keep", false, "Keep the debug pod after exit")
	cmd.Flags().Bool("host-network", false, "Use host network for the debug pod")

	return cmd
}

func runPod(cmd *cobra.Command, args []string) error {
	profile, err := resolveProfile(cmd)
	if err != nil {
		return err
	}

	namespace, _ := cmd.Flags().GetString("namespace")
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
	keep, _ := cmd.Flags().GetBool("keep")
	hostNetwork, _ := cmd.Flags().GetBool("host-network")

	pullPolicy, err := resolvePullPolicy(flagPullPolicy)
	if err != nil {
		return err
	}

	image := flagImage
	if image == "" {
		image = runtime.DefaultImage
	}

	opts := runtime.PodOpts{
		Image:       image,
		Namespace:   namespace,
		Kubeconfig:  kubeconfig,
		KubeContext: flagKubeContext,
		Keep:        keep,
		HostNetwork: hostNetwork,
		Privileged:  flagPrivileged,
		User:        flagUser,
		PullPolicy:  pullPolicy,
		Profile:     profile,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return runtime.KubernetesPod(ctx, opts)
}
