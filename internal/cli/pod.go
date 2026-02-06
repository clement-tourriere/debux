package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/ctourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newPodCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pod",
		Short: "Create a standalone debug pod in Kubernetes",
		Long:  "Create a standalone debug pod with the NixOS debug image in a Kubernetes cluster.",
		RunE:  runPod,
	}

	cmd.Flags().StringP("namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().String("kubeconfig", "", "Override kubeconfig path")
	cmd.Flags().Bool("keep", false, "Keep the debug pod after exit (default: delete on exit)")
	cmd.Flags().Bool("host-network", false, "Use host network for the debug pod")

	return cmd
}

func runPod(cmd *cobra.Command, args []string) error {
	namespace, _ := cmd.Flags().GetString("namespace")
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
	keep, _ := cmd.Flags().GetBool("keep")
	hostNetwork, _ := cmd.Flags().GetBool("host-network")

	image := flagImage
	if image == "" {
		image = runtime.DefaultImage
	}

	opts := runtime.PodOpts{
		Image:       image,
		Namespace:   namespace,
		Kubeconfig:  kubeconfig,
		Keep:        keep,
		HostNetwork: hostNetwork,
		Privileged:  flagPrivileged,
		User:        flagUser,
		PullPolicy:  flagPullPolicy,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return runtime.KubernetesPod(ctx, opts)
}
