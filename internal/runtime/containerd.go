package runtime

import (
	"context"
	"fmt"
)

// ContainerdExec debugs a running containerd container.
// This is deferred to v0.2 — containerd runtime support is planned but not yet implemented.
func ContainerdExec(ctx context.Context, target *Target, opts DebugOpts) error {
	return fmt.Errorf("containerd runtime is not yet supported (planned for v0.2)\n\nFor now, use Docker or Kubernetes:\n  debux exec docker://%s\n  debux exec k8s://%s", target.Name, target.Name)
}
