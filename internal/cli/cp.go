package cli

import (
	"fmt"
	"strings"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newCpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cp <source> <destination>",
		Short: "Copy files between a container/pod and the local filesystem",
		Long: `Copy files between a target's filesystem and the local machine.

One side is a target path (<target>:<path>), the other a local path.
Kubernetes copies stream through the debux toolbox, so they work on
distroless and scratch images where kubectl cp fails (no tar in the target).
Docker copies use the engine API and also work on stopped containers.

When copying INTO a target, the destination is treated as a directory.`,
		Example: `  debux cp my-app:/var/log/app.log ./app.log
  debux cp docker://my-app:/etc/nginx ./nginx-conf
  debux cp k8s://prod/api-pod:/app/heap.prof ./
  debux cp ./debug-tool k8s://prod/api-pod:/tmp
  debux cp compose://web:/usr/share/nginx/html ./html`,
		Args:          cobra.ExactArgs(2),
		RunE:          runCp,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	addKubernetesFlags(cmd)
	cmd.Flags().String("profile", runtime.ProfileGeneral, "Kubernetes security profile (defaults to config)")
	cmd.Flags().String("image", "", "Kubernetes debug image (defaults to config)")
	cmd.Flags().String("user", "", "Kubernetes debug user uid[:gid]")
	cmd.Flags().String("pull-policy", "", "Kubernetes debug image pull policy")
	registerProfileFlagCompletion(cmd)
	registerImageFlagCompletion(cmd)
	registerPullPolicyFlagCompletion(cmd)
	return cmd
}

func runCp(cmd *cobra.Command, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	srcRef, srcPath, srcRemote, err := splitCpArg(args[0])
	if err != nil {
		return err
	}
	dstRef, dstPath, dstRemote, err := splitCpArg(args[1])
	if err != nil {
		return err
	}

	switch {
	case srcRemote && dstRemote:
		return fmt.Errorf("copying between two targets is not supported; copy to a local path first")
	case !srcRemote && !dstRemote:
		return fmt.Errorf("one side must be a target path (<target>:<path>); for local copies use cp")
	}

	ref := srcRef
	if dstRemote {
		ref = dstRef
	}
	target, err := runtime.ParseTarget(ref)
	if err != nil {
		return fmt.Errorf("invalid target: %w", err)
	}
	if target.Name == "" && target.ComposeService == "" {
		return fmt.Errorf("cp requires a concrete target name")
	}

	switch target.Runtime {
	case "docker":
		if kubernetesFlagsChanged(cmd) || flagChanged(cmd, "profile") || flagChanged(cmd, "image") || flagChanged(cmd, "user") || flagChanged(cmd, "pull-policy") {
			return fmt.Errorf("--context, --kubeconfig, and --namespace are only supported for Kubernetes targets; use k8s://... or remove the flag")
		}
		if srcRemote {
			return runtime.DockerCopyFrom(ctx, target, srcPath, dstPath)
		}
		return runtime.DockerCopyTo(ctx, target, srcPath, dstPath)
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
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")

		profile, err := resolveProfile(cmd)
		if err != nil {
			return err
		}
		pullPolicy, err := resolvePullPolicy(configuredPullPolicy(flagString(cmd, "pull-policy")))
		if err != nil {
			return err
		}
		opts := runtime.DebugOpts{
			Image:       resolveImage(flagString(cmd, "image")),
			Kubeconfig:  kubeconfig,
			KubeContext: kubeContext,
			PullPolicy:  pullPolicy,
			Profile:     profile,
			User:        flagString(cmd, "user"),
		}
		if srcRemote {
			return runtime.KubernetesCopyFrom(ctx, target, opts, srcPath, dstPath)
		}
		return runtime.KubernetesCopyTo(ctx, target, opts, srcPath, dstPath)
	default:
		return fmt.Errorf("cp is not supported for runtime %q", target.Runtime)
	}
}

// splitCpArg splits "<target>:<path>" while leaving URI schemes (k8s://...)
// intact. Plain arguments without a colon are local paths; prefix local paths
// containing colons with ./ to disambiguate.
func splitCpArg(arg string) (targetRef, path string, remote bool, err error) {
	if strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") || arg == "." || arg == ".." {
		return "", arg, false, nil
	}
	rest := arg
	schemeEnd := 0
	if idx := strings.Index(arg, "://"); idx != -1 {
		schemeEnd = idx + 3
		rest = arg[schemeEnd:]
	}
	colon := strings.Index(rest, ":")
	if colon == -1 {
		return "", arg, false, nil
	}
	targetRef = arg[:schemeEnd+colon]
	path = rest[colon+1:]
	if targetRef == "" || path == "" {
		return "", "", false, fmt.Errorf("invalid target path %q: expected <target>:<path>", arg)
	}
	return targetRef, path, true, nil
}
