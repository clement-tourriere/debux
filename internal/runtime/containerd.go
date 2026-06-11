package runtime

import (
	"context"
	"fmt"
)

// ContainerdExec debugs a running containerd container.
// Native containerd support is tracked upstream but not implemented; nerdctl
// users can often reach the same containers through the Docker-compatible API.
func ContainerdExec(ctx context.Context, target *Target, opts DebugOpts) error {
	return fmt.Errorf("the containerd runtime is not supported yet (https://github.com/clement-tourriere/debux/issues)\n\nAlternatives:\n  debux exec docker://%s   # if the container is also visible to a Docker-compatible daemon\n  debux exec k8s://%s      # if it runs inside a Kubernetes pod", target.Name, target.Name)
}
