package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/clement-tourriere/debux/internal/config"
	"github.com/clement-tourriere/debux/internal/picker"
	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node [node-name]",
		Short: "Debug a Kubernetes node with a host-namespace toolbox pod",
		Long: `Debug a Kubernetes node with a debux toolbox pod pinned to it.

The pod shares the node's PID, network, and IPC namespaces and mounts the
node's root filesystem at /host ($DEBUX_TARGET_ROOT). Node binaries like
crictl, journalctl, and systemctl run through the chroot fallback with their
original host paths.

Without a node name, debux opens a node picker. Tainted and cordoned nodes
are tolerated — debugging them is usually the point.`,
		Example: `  debux node
  debux node worker-1
  debux node worker-1 --profile=sysadmin
  debux node worker-1 -n kube-system --keep`,
		Args: cobra.MaximumNArgs(1),
		RunE: runNode,
	}

	addPodDebugFlags(cmd)
	cmd.Flags().StringP("namespace", "n", "", "Namespace for the debug pod (default: current kube-context namespace)")
	registerNamespaceFlagCompletion(cmd)
	cmd.Flags().Bool("keep", false, "Keep the debug pod after exit")

	return cmd
}

func runNode(cmd *cobra.Command, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	profile, err := resolveProfile(cmd)
	if err != nil {
		return err
	}
	namespace, _ := cmd.Flags().GetString("namespace")
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
	keep, _ := cmd.Flags().GetBool("keep")

	pullPolicy, err := resolvePullPolicy(configuredPullPolicy(flagPullPolicy))
	if err != nil {
		return err
	}
	if err := runtime.ValidateEnvVars(flagEnv); err != nil {
		return err
	}

	nodeName := ""
	if len(args) == 1 {
		nodeName = args[0]
	}
	if nodeName == "" {
		nodeName, err = pickKubernetesNode(ctx, kubeconfig, flagKubeContext)
		if err != nil {
			return err
		}
	}

	opts := runtime.PodOpts{
		Image:       resolveImage(flagImage),
		Namespace:   namespace,
		Kubeconfig:  kubeconfig,
		KubeContext: flagKubeContext,
		Keep:        keep,
		Privileged:  flagPrivileged,
		User:        flagUser,
		PullPolicy:  pullPolicy,
		Profile:     profile,
		Env:         flagEnv,
		CapAdd:      flagCapAdd,
		Tools:       config.ResolveTools(flagTools),
	}

	return runtime.KubernetesNode(ctx, nodeName, opts)
}

func pickKubernetesNode(ctx context.Context, kubeconfig, kubeContext string) (string, error) {
	nodes, err := runtime.KubernetesNodes(ctx, kubeconfig, kubeContext)
	if err != nil {
		return "", err
	}
	if len(nodes) == 0 {
		return "", fmt.Errorf("no nodes found in the cluster")
	}
	items := make([]picker.Item, len(nodes))
	for i, node := range nodes {
		label := node.Name
		var details []string
		if node.Status != "" {
			details = append(details, node.Status)
		}
		if len(node.Roles) > 0 {
			details = append(details, strings.Join(node.Roles, ","))
		}
		if node.Version != "" {
			details = append(details, node.Version)
		}
		if len(details) > 0 {
			label = fmt.Sprintf("%s (%s)", node.Name, strings.Join(details, " · "))
		}
		items[i] = picker.Item{Label: label, Value: node.Name}
	}
	return picker.Pick("Select a node to debug", items)
}
