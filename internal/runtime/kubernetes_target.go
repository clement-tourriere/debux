package runtime

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// /proc/1/root is the selected application's root only with a private target
// PID namespace. Never silently read/write the sandbox or host filesystem.
// Runtime-specific cgroup/PID discovery is not reliable across cgroup namespaces.
func validateKubernetesTargetNamespace(pod *corev1.Pod) error {
	if pod.Spec.HostPID {
		return fmt.Errorf("pod %s/%s uses the host PID namespace; in-place debugging, debux cp, and --copy are not supported for hostPID pods. Use debux node for intentional host access", pod.Namespace, pod.Name)
	}
	if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
		return fmt.Errorf("pod %s/%s shares a pod/host PID namespace; in-place filesystem targeting and debux cp are not supported for this layout (PID 1 is not the selected container). Use --copy only after evaluating the side effects of starting a new workload instance", pod.Namespace, pod.Name)
	}
	return nil
}
