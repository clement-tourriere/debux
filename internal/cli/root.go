package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/clement-tourriere/debux/internal/version"
	"github.com/spf13/cobra"
)

const docsURL = "https://clement-tourriere.github.io/debux/"

var (
	flagImage       string
	flagPrivileged  bool
	flagUser        string
	flagRemove      bool
	flagNoVolumes   bool
	flagPullPolicy  string
	flagKubeContext string
	flagFresh       bool
	flagCopy        bool
	flagProfile     string
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
  k8s://<namespace>/<pod>         Pod in an explicit namespace
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
  debux k8s://prod/api-pod
  debux k8s://prod/api-pod/app
  debux k8s://@eks-preprod-01/prod/api-pod/app

  # Partial pod names open the searchable matching-pod picker
  debux k8s://prod/webapp-internal-api

  # Restricted/non-root Kubernetes debug shell
  debux k8s://prod/api-pod/app --profile=restricted

  # If ephemeral containers are blocked by RBAC or policy
  debux k8s://prod/api-pod/app --copy

  # Pull the latest debug image and force a fresh session
  debux k8s://prod/api-pod/app --fresh --pull-policy=Always

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

	cmd.AddCommand(newExecCmd())
	cmd.AddCommand(newImageCmd())
	cmd.AddCommand(newPodCmd())
	cmd.AddCommand(newKillCmd())
	cmd.AddCommand(newStoreCmd())
	cmd.AddCommand(newCompletionCmd(cmd))
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
	cmd.Flags().StringVar(&flagUser, "user", "", "Run debug container as uid[:gid]")
	cmd.Flags().BoolVar(&flagPrivileged, "privileged", false, "Run privileged (Docker); Kubernetes alias for --profile=sysadmin")
	cmd.Flags().BoolVar(&flagCopy, "copy", false, "Kubernetes: use a temporary copied pod instead of an ephemeral container")
	cmd.Flags().StringVar(&flagPullPolicy, "pull-policy", "", "Image pull policy for the debug image (Always, IfNotPresent, Never)")
	cmd.Flags().StringVar(&flagProfile, "profile", runtime.ProfileGeneral,
		fmt.Sprintf("Kubernetes: security profile (%s)", strings.Join(runtime.ValidProfiles, ", ")))
	cmd.Flags().String("kubeconfig", "", "Kubernetes: kubeconfig path")
	cmd.Flags().StringVar(&flagKubeContext, "context", "", "Kubernetes: kube context name")
}

func addImageFlags(cmd *cobra.Command) {
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&flagImage, "image", "", fmt.Sprintf("Debug image (default %s)", runtime.DefaultImage))
	cmd.Flags().BoolVar(&flagRemove, "rm", true, "Remove the debug container after exit")
	cmd.Flags().BoolVar(&flagPrivileged, "privileged", false, "Run debug container privileged")
	cmd.Flags().StringVar(&flagUser, "user", "", "Run debug container as uid[:gid]")
}

func addPodDebugFlags(cmd *cobra.Command) {
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&flagImage, "image", "", fmt.Sprintf("Debug image (default %s)", runtime.DefaultImage))
	cmd.Flags().StringVar(&flagUser, "user", "", "Run debug container as uid[:gid]")
	cmd.Flags().BoolVar(&flagPrivileged, "privileged", false, "Alias for --profile=sysadmin")
	cmd.Flags().StringVar(&flagPullPolicy, "pull-policy", "", "Image pull policy (Always, IfNotPresent, Never)")
	cmd.Flags().StringVar(&flagProfile, "profile", runtime.ProfileGeneral,
		fmt.Sprintf("Security profile (%s)", strings.Join(runtime.ValidProfiles, ", ")))
	cmd.Flags().String("kubeconfig", "", "Kubeconfig path")
	cmd.Flags().StringVar(&flagKubeContext, "context", "", "Kube context name")
}

func addKubeconfigFlag(cmd *cobra.Command) {
	cmd.Flags().SortFlags = false
	cmd.Flags().String("kubeconfig", "", "Kubeconfig path")
	cmd.Flags().StringVar(&flagKubeContext, "context", "", "Kube context name")
}

func flagChanged(cmd *cobra.Command, name string) bool {
	flag := cmd.Flags().Lookup(name)
	return flag != nil && flag.Changed
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
		// Validate profile
		valid := false
		for _, p := range runtime.ValidProfiles {
			if flagProfile == p {
				valid = true
				break
			}
		}
		if !valid {
			return "", fmt.Errorf("invalid profile %q: must be one of %s", flagProfile, strings.Join(runtime.ValidProfiles, ", "))
		}
		return flagProfile, nil
	}

	return runtime.ProfileGeneral, nil
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
