package cli

import (
	"github.com/clement-tourriere/debux/internal/config"
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
		Example: `  debux pod
  debux pod -n prod
  debux pod -n prod --host-network
  debux pod -n prod --keep
  debux pod -n prod --profile=netadmin`,
		Args: cobra.NoArgs,
		RunE: runPod,
	}

	addPodDebugFlags(cmd)
	cmd.Flags().StringP("namespace", "n", "", "Kubernetes namespace (default: current kube-context namespace)")
	registerNamespaceFlagCompletion(cmd)
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

	pullPolicy, err := resolvePullPolicy(configuredPullPolicy(flagString(cmd, "pull-policy")))
	if err != nil {
		return err
	}

	if err := runtime.ValidateEnvVars(flagStrings(cmd, "env")); err != nil {
		return err
	}

	opts := runtime.PodOpts{
		Image:       resolveImage(flagString(cmd, "image")),
		Namespace:   namespace,
		Kubeconfig:  kubeconfig,
		KubeContext: flagString(cmd, "context"),
		Keep:        keep,
		HostNetwork: hostNetwork,
		User:        flagString(cmd, "user"),
		PullPolicy:  pullPolicy,
		Profile:     profile,
		Env:         flagStrings(cmd, "env"),
		CapAdd:      flagStrings(cmd, "cap-add"),
		Tools:       config.ResolveTools(flagStrings(cmd, "tools")),
	}

	ctx, cancel := signalContext()
	defer cancel()

	return runtime.KubernetesPod(ctx, opts)
}
