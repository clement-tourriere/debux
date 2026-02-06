package cli

import (
	"github.com/spf13/cobra"
)

var (
	flagImage      string
	flagPrivileged bool
	flagUser       string
	flagRemove     bool
	flagNoVolumes  bool
	flagPullPolicy string
	flagFresh      bool
)

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debux [target]",
		Short: "Universal container debugging tool",
		Long: `Debug any container — even distroless/scratch — with a rich NixOS-powered shell.

If no target is specified, an interactive picker lists running Docker containers.
Using a schema without a name (e.g. docker://, k8s://) shows a picker for that runtime.

Target formats:
  <container>                     Docker container (default runtime)
  docker://<container>            Docker container
  containerd://<container>        containerd container
  nerdctl://<container>           containerd container (alias)
  k8s://<pod>                     Kubernetes pod (default namespace)
  k8s://<namespace>/<pod>         Kubernetes pod (specific namespace)
  k8s://<ns>/<pod>/<container>    Kubernetes pod (specific container)`,
		Args:          cobra.MaximumNArgs(1),
		RunE:          runExec,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&flagImage, "image", "", "Override debug image (default: ghcr.io/clement-tourriere/debux:latest)")
	cmd.PersistentFlags().BoolVar(&flagPrivileged, "privileged", false, "Run debug container in privileged mode")
	cmd.PersistentFlags().StringVar(&flagUser, "user", "", "Run as specific user (uid:gid)")
	cmd.PersistentFlags().BoolVar(&flagRemove, "rm", true, "Auto-remove debug container on exit")
	cmd.PersistentFlags().BoolVar(&flagNoVolumes, "no-volumes", false, "Don't share target container's volumes")
	cmd.PersistentFlags().StringVar(&flagPullPolicy, "pull-policy", "IfNotPresent", "Image pull policy for Kubernetes (Always, IfNotPresent, Never)")
	cmd.PersistentFlags().BoolVar(&flagFresh, "fresh", false, "Force a new debug container instead of reusing an existing one (Kubernetes)")
	cmd.PersistentFlags().String("kubeconfig", "", "Override kubeconfig path")

	cmd.AddCommand(newExecCmd())
	cmd.AddCommand(newPodCmd())
	cmd.AddCommand(newImageCmd())
	cmd.AddCommand(newStoreCmd())

	return cmd
}

func Execute() error {
	return NewRootCmd().Execute()
}
