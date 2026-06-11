package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/clement-tourriere/debux/internal/config"
	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/clement-tourriere/debux/internal/version"
	"github.com/spf13/cobra"
)

const docsURL = "https://clement-tourriere.github.io/debux/"

// defaultCopyPodTTL bounds the life of --copy debug pods so a CLI that dies
// without cleaning up (power loss, SIGKILL) cannot leak a pod forever.
const defaultCopyPodTTL = "24h"

var (
	flagImage           string
	flagPrivileged      bool
	flagUser            string
	flagRemove          bool
	flagNoVolumes       bool
	flagReadOnlyVolumes bool
	flagPullPolicy      string
	flagKubeContext     string
	flagNamespace       string
	flagFresh           bool
	flagCopy            bool
	flagKeep            bool
	flagTTL             string
	flagProfile         string
	flagEnv             []string
	flagCapAdd          []string
	flagTools           []string
)

const rootLong = `debux starts a rich Nix-powered debug container next to your target.

It is built for production-style images that do not contain a useful shell:
distroless, scratch, Alpine, minimal images, and locked-down Kubernetes pods.

With Docker, debux creates a reusable debug sidecar. With Kubernetes, debux uses
an ephemeral container by default, or a temporary copied pod with --copy.

Target formats:
  <container>                     Docker container (default runtime)
  docker://<container>            Docker container
  docker://                       Docker picker
  k8s://                          Kubernetes pod picker
  k8s://<pod>                     Pod in the current kube-context namespace
  k8s://<namespace>/<pod>         Pod in an explicit namespace (or use -n/--namespace)
  k8s://<pod>/<ctr> -n <ns>       Specific container using namespace flag
  k8s://<namespace>/<pod>/<ctr>   Specific container in a pod
  k8s://@<context>                Pod picker in a specific kube context
  k8s://@<context>/<pod>          Pod in that context's namespace
  k8s://@<context>/<ns>/<pod>     Pod in a specific kube context
  k8s://@<context>/<ns>/<pod>/<ctr> Specific container in a specific context

If a Kubernetes pod name is not found exactly, debux treats it as a substring
and opens the searchable picker with matching running pods.

Security: the default Kubernetes profile runs a root debug container with
practical debugging capabilities inside the pod. Use --profile=restricted for a
non-root, drop-capabilities session.`

const rootExample = `  # Pick a Docker container interactively
  debux
  debux docker://

  # Debug a Docker container by name or ID
  debux my-app
  debux docker://my-app

  # Pick a Kubernetes pod in the current kube-context namespace
  debux k8s://

  # Pick a Kubernetes pod in another context
  debux k8s://@eks-preprod-01

  # Debug a Kubernetes pod or a specific container
  debux k8s://api-pod
  debux k8s://api-pod --namespace prod
  debux k8s://prod/api-pod
  debux k8s://prod/api-pod/app
  debux k8s://@eks-preprod-01/prod/api-pod/app

  # Partial pod names open the searchable matching-pod picker
  debux k8s://prod/webapp-internal-api

  # Restricted/non-root Kubernetes debug shell
  debux k8s://prod/api-pod/app --profile=restricted

  # If ephemeral containers are blocked by RBAC or policy
  debux k8s://prod/api-pod/app --copy

  # Long-lived copy session that survives rollouts of the source Deployment;
  # the pod stays after exit and self-destructs after 48h
  debux k8s://prod/api-pod/app --copy --keep --ttl=48h

  # Pull the latest debug image and force a fresh session
  debux k8s://prod/api-pod/app --fresh --pull-policy=Always

  # Mount target volumes read-only to reduce accidental writes
  debux k8s://prod/api-pod/app --read-only-volumes

  # Run a one-shot command inside the debug toolbox
  debux docker://my-app -- curl -I localhost
  debux k8s://prod/api-pod/app -- ps aux`

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "debux [target] [-- command...]",
		Short:         "Debug any Docker or Kubernetes container",
		Long:          rootLong,
		Example:       rootExample,
		Version:       version.Details(),
		Args:          cobra.ArbitraryArgs,
		RunE:          runExec,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.Flags().SortFlags = false
	cmd.InheritedFlags().SortFlags = false
	cmd.CompletionOptions.DisableDefaultCmd = true

	addExecFlags(cmd)
	configureTargetCompletion(cmd)

	cmd.AddCommand(newExecCmd())
	cmd.AddCommand(newTUICmd())
	cmd.AddCommand(newImageCmd())
	cmd.AddCommand(newPodCmd())
	cmd.AddCommand(newNodeCmd())
	cmd.AddCommand(newForwardCmd())
	cmd.AddCommand(newCpCmd())
	cmd.AddCommand(newKillCmd())
	cmd.AddCommand(newStoreCmd())
	cmd.AddCommand(newCompletionCmd(cmd))
	cmd.AddCommand(newCompletionCacheCmd())
	cmd.AddCommand(newDocsCmd())
	cmd.AddCommand(newDoctorCmd())
	cmd.AddCommand(newUpdateCmd())
	cmd.AddCommand(newVersionCmd())

	return cmd
}

func addExecFlags(cmd *cobra.Command) {
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&flagImage, "image", "", fmt.Sprintf("Debug image (default %s)", runtime.DefaultImage))
	cmd.Flags().BoolVar(&flagFresh, "fresh", false, "Create a fresh debug container instead of reusing an existing debux session")
	cmd.Flags().BoolVar(&flagNoVolumes, "no-volumes", false, "Do not directly mount target volumes (not a security boundary if /proc/1/root is accessible)")
	cmd.Flags().BoolVar(&flagReadOnlyVolumes, "read-only-volumes", false, "Mount target volumes read-only in the debug container")
	cmd.Flags().StringVar(&flagUser, "user", "", "Run debug container as uid[:gid]")
	cmd.Flags().BoolVar(&flagPrivileged, "privileged", false, "Run privileged (Docker); Kubernetes alias for --profile=sysadmin")
	cmd.Flags().BoolVar(&flagCopy, "copy", false, "Kubernetes: use a temporary copied pod instead of an ephemeral container")
	cmd.Flags().BoolVar(&flagKeep, "keep", false, "Kubernetes: with --copy, keep the copy pod after the session ends (reattach by targeting it, delete with debux kill)")
	cmd.Flags().StringVar(&flagTTL, "ttl", defaultCopyPodTTL, "Kubernetes: with --copy, kubelet-enforced deadline after which the copy pod is stopped (Go duration; 0 disables)")
	cmd.Flags().StringVar(&flagPullPolicy, "pull-policy", "", "Image pull policy for the debug image (Always, IfNotPresent, Never)")
	cmd.Flags().StringVar(&flagProfile, "profile", runtime.ProfileGeneral,
		fmt.Sprintf("Kubernetes: security profile (%s)", strings.Join(runtime.ValidProfiles, ", ")))
	cmd.Flags().StringArrayVar(&flagEnv, "env", nil, "Extra KEY=VALUE environment for the debug container (repeatable)")
	cmd.Flags().StringArrayVar(&flagCapAdd, "cap-add", nil, "Extra Linux capability for the debug container (repeatable)")
	cmd.Flags().StringArrayVar(&flagTools, "tools", nil, "Tool set name from the config file, or nixpkgs packages, auto-installed at session start (repeatable)")
	cmd.Flags().String("kubeconfig", "", "Kubernetes: kubeconfig path")
	cmd.Flags().StringVar(&flagKubeContext, "context", "", "Kubernetes: kube context name")
	cmd.Flags().StringVarP(&flagNamespace, "namespace", "n", "", "Kubernetes: namespace")
	registerExecFlagCompletions(cmd)
}

func addImageFlags(cmd *cobra.Command) {
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&flagImage, "image", "", fmt.Sprintf("Debug image (default %s)", runtime.DefaultImage))
	cmd.Flags().BoolVar(&flagRemove, "rm", true, "Remove the debug container after exit")
	cmd.Flags().BoolVar(&flagPrivileged, "privileged", false, "Run debug container privileged")
	cmd.Flags().StringVar(&flagUser, "user", "", "Run debug container as uid[:gid]")
	registerImageFlagCompletion(cmd)
}

func addPodDebugFlags(cmd *cobra.Command) {
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&flagImage, "image", "", fmt.Sprintf("Debug image (default %s)", runtime.DefaultImage))
	cmd.Flags().StringVar(&flagUser, "user", "", "Run debug container as uid[:gid]")
	cmd.Flags().BoolVar(&flagPrivileged, "privileged", false, "Alias for --profile=sysadmin")
	cmd.Flags().StringVar(&flagPullPolicy, "pull-policy", "", "Image pull policy (Always, IfNotPresent, Never)")
	cmd.Flags().StringVar(&flagProfile, "profile", runtime.ProfileGeneral,
		fmt.Sprintf("Security profile (%s)", strings.Join(runtime.ValidProfiles, ", ")))
	cmd.Flags().StringArrayVar(&flagEnv, "env", nil, "Extra KEY=VALUE environment for the debug container (repeatable)")
	cmd.Flags().StringArrayVar(&flagCapAdd, "cap-add", nil, "Extra Linux capability for the debug container (repeatable)")
	cmd.Flags().StringArrayVar(&flagTools, "tools", nil, "Tool set name from the config file, or nixpkgs packages, auto-installed at session start (repeatable)")
	cmd.Flags().String("kubeconfig", "", "Kubeconfig path")
	cmd.Flags().StringVar(&flagKubeContext, "context", "", "Kube context name")
	registerImageFlagCompletion(cmd)
	registerKubeContextFlagCompletion(cmd)
	registerPullPolicyFlagCompletion(cmd)
	registerProfileFlagCompletion(cmd)
}

func addKubernetesFlags(cmd *cobra.Command) {
	cmd.Flags().SortFlags = false
	cmd.Flags().String("kubeconfig", "", "Kubeconfig path")
	cmd.Flags().StringVar(&flagKubeContext, "context", "", "Kube context name")
	cmd.Flags().StringVarP(&flagNamespace, "namespace", "n", "", "Kubernetes namespace")
	registerKubernetesFlagCompletions(cmd)
}

func flagChanged(cmd *cobra.Command, name string) bool {
	flag := cmd.Flags().Lookup(name)
	return flag != nil && flag.Changed
}

func resolveKubeNamespace(cmd *cobra.Command, targetNamespace string) (string, error) {
	if !flagChanged(cmd, "namespace") {
		return targetNamespace, nil
	}
	if targetNamespace != "" && targetNamespace != flagNamespace {
		return "", fmt.Errorf("conflicting Kubernetes namespaces: target uses %q but --namespace=%q", targetNamespace, flagNamespace)
	}
	return flagNamespace, nil
}

func applyKubeNamespaceFlagContainerShorthand(cmd *cobra.Command, target *runtime.Target) {
	if target == nil || target.Runtime != "kubernetes" || !flagChanged(cmd, "namespace") || flagNamespace == "" {
		return
	}
	if target.Namespace == "" || target.Name == "" || target.Container != "" || target.Namespace == flagNamespace {
		return
	}

	// With --namespace, treat two URI segments as pod/container. This keeps
	// k8s://namespace/pod unambiguous when no namespace flag is supplied while
	// supporting the documented k8s://pod/container --namespace namespace form.
	target.Container = target.Name
	target.Name = target.Namespace
	target.Namespace = ""
}

// resolveProfile resolves the security profile from --profile and --privileged flags.
func resolveProfile(cmd *cobra.Command) (string, error) {
	privilegedSet := flagChanged(cmd, "privileged") && flagPrivileged
	profileSet := flagChanged(cmd, "profile")

	if privilegedSet && profileSet && flagProfile != runtime.ProfileSysadmin {
		return "", fmt.Errorf("conflicting flags: --privileged and --profile=%s (use --profile=sysadmin or remove --privileged)", flagProfile)
	}

	if privilegedSet {
		fmt.Fprintln(os.Stderr, "Warning: --privileged is deprecated for Kubernetes, use --profile=sysadmin instead")
		return runtime.ProfileSysadmin, nil
	}

	if profileSet {
		if err := validateProfile(flagProfile); err != nil {
			return "", err
		}
		return flagProfile, nil
	}

	// Fall back to the config file's default profile before the built-in one.
	if cfgProfile := strings.TrimSpace(config.Get().Profile); cfgProfile != "" {
		if err := validateProfile(cfgProfile); err != nil {
			return "", fmt.Errorf("config file: %w", err)
		}
		return cfgProfile, nil
	}

	return runtime.ProfileGeneral, nil
}

func validateProfile(profile string) error {
	for _, p := range runtime.ValidProfiles {
		if profile == p {
			return nil
		}
	}
	return fmt.Errorf("invalid profile %q: must be one of %s", profile, strings.Join(runtime.ValidProfiles, ", "))
}

// resolveImage applies the image precedence: --image flag, config file,
// built-in default.
func resolveImage(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if cfg := strings.TrimSpace(config.Get().Image); cfg != "" {
		return cfg
	}
	return runtime.DefaultImage
}

// configuredPullPolicy applies the pull-policy precedence: flag, config file.
func configuredPullPolicy(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return strings.TrimSpace(config.Get().PullPolicy)
}

func resolvePullPolicy(policy string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", "ifnotpresent":
		if strings.TrimSpace(policy) == "" {
			return "", nil
		}
		return "IfNotPresent", nil
	case "always":
		return "Always", nil
	case "never":
		return "Never", nil
	default:
		return "", fmt.Errorf("invalid --pull-policy %q: must be one of Always, IfNotPresent, Never", policy)
	}
}

func Execute() error {
	return NewRootCmd().Execute()
}
