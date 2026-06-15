package runtime

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func KubernetesSessions(ctx context.Context, kubeconfig, kubeContext, namespace string, allNamespaces bool) ([]DebugSessionInfo, error) {
	_, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}

	listNamespace := metav1.NamespaceAll
	if !allNamespaces {
		listNamespace = resolveTargetNamespace(namespace, kubeconfig, kubeContext)
	}
	displayContext := kubernetesDisplayContext(kubeconfig, kubeContext)

	var result []DebugSessionInfo
	listOptions := metav1.ListOptions{
		FieldSelector: "status.phase=Running",
		Limit:         kubernetesPodListLimit,
	}
	for {
		pods, err := clientset.CoreV1().Pods(listNamespace).List(ctx, listOptions)
		if err != nil {
			return nil, fmt.Errorf("listing pods: %w", err)
		}
		for i := range pods.Items {
			result = append(result, kubernetesSessionsForPod(&pods.Items[i], displayContext)...)
		}
		if pods.Continue == "" {
			break
		}
		listOptions.Continue = pods.Continue
	}

	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Runtime != result[j].Runtime {
			return result[i].Runtime < result[j].Runtime
		}
		return result[i].Target < result[j].Target
	})
	return result, nil
}

func kubernetesSessionsForPod(pod *corev1.Pod, displayContext string) []DebugSessionInfo {
	var result []DebugSessionInfo

	if isKubernetesCopyPod(pod) {
		if debugName := findCopyPodDebugContainer(pod); debugName != "" && podContainerRunning(pod, debugName) {
			image, profile, user := kubernetesRegularContainerDebugMetadata(pod, debugName)
			session := DebugSessionInfo{
				Runtime:         "kubernetes",
				Kind:            DebugSessionKindKubernetesCopyPod,
				Target:          kubernetesTargetURI(displayContext, pod.Namespace, pod.Name),
				Name:            pod.Name,
				Context:         displayContext,
				Namespace:       pod.Namespace,
				TargetContainer: pod.Annotations[debuxTargetContainerAnnotation],
				DebugName:       debugName,
				Source:          pod.Annotations[debuxSourcePodAnnotation],
				Image:           image,
				Profile:         profile,
				User:            user,
				Status:          string(pod.Status.Phase),
			}
			if remaining, ok := copyPodTimeRemaining(pod); ok {
				session.ExpiresIn = remaining
				session.HasExpiry = true
			}
			if pod.Status.StartTime != nil {
				session.StartedAt = pod.Status.StartTime.Time
			}
			result = append(result, session)
		}
		return result
	}

	running := runningDebuxEphemeralContainers(pod)
	for _, ec := range pod.Spec.EphemeralContainers {
		if _, ok := running[ec.Name]; !ok || !debuxEphemeralContainerHasMetadata(ec) {
			continue
		}
		profile, user := kubernetesDebugEnvMetadata(ec.Env)
		startedAt := time.Time{}
		for _, cs := range pod.Status.EphemeralContainerStatuses {
			if cs.Name == ec.Name && cs.State.Running != nil {
				startedAt = cs.State.Running.StartedAt.Time
				break
			}
		}
		result = append(result, DebugSessionInfo{
			Runtime:         "kubernetes",
			Kind:            DebugSessionKindKubernetesEphemeral,
			Target:          kubernetesTargetURIWithContainer(displayContext, pod.Namespace, pod.Name, ec.TargetContainerName),
			Name:            pod.Name,
			Context:         displayContext,
			Namespace:       pod.Namespace,
			TargetContainer: ec.TargetContainerName,
			DebugName:       ec.Name,
			Image:           ec.Image,
			Profile:         profile,
			User:            user,
			Status:          string(pod.Status.Phase),
			StartedAt:       startedAt,
		})
	}
	return result
}

func kubernetesRegularContainerDebugMetadata(pod *corev1.Pod, name string) (image, profile, user string) {
	for _, c := range pod.Spec.Containers {
		if c.Name != name {
			continue
		}
		profile, user = kubernetesDebugEnvMetadata(c.Env)
		return c.Image, profile, user
	}
	return "", "", ""
}

func kubernetesDebugEnvMetadata(env []corev1.EnvVar) (profile, user string) {
	for _, e := range env {
		switch e.Name {
		case "DEBUX_SECURITY_PROFILE":
			profile = e.Value
		case "DEBUX_DEBUG_USER":
			user = e.Value
		}
	}
	return profile, user
}

// KubernetesBrowsePods returns lightweight running pod metadata for interactive navigation.
// It avoids per-pod GET enrichment so large namespaces remain responsive.

func KubernetesKill(ctx context.Context, target *Target, kubeconfig string, kubeContext string) error {
	config, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return err
	}

	namespace := resolveTargetNamespace(target.Namespace, kubeconfig, kubeContext)

	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting pod %s/%s: %w", namespace, target.Name, err)
	}

	// Copy pods are debux-owned: kill deletes the whole pod rather than just
	// the debug session inside it.
	if isKubernetesCopyPod(pod) {
		if err := clientset.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("deleting debug copy pod %s/%s: %w", namespace, pod.Name, err)
		}
		fmt.Printf("Deleted debug copy pod %s/%s\n", namespace, pod.Name)
		return nil
	}

	containerName := findRunningDebuxContainerForKill(pod, target.Container)
	if containerName == "" {
		if target.Container != "" {
			return fmt.Errorf("no running debux session found for container %q on pod %s/%s", target.Container, namespace, target.Name)
		}
		return fmt.Errorf("no running debux session found on pod %s/%s", namespace, target.Name)
	}

	if err := killInContainer(ctx, config, clientset, namespace, target.Name, containerName); err != nil {
		return fmt.Errorf("killing debug session on %s/%s: %w", namespace, target.Name, err)
	}

	fmt.Printf("Killed debug session on %s/%s (container: %s)\n", namespace, target.Name, containerName)
	return nil
}

// KubernetesKillAll terminates all running debux ephemeral containers across
// all pods in the resolved namespace.

func KubernetesKillAll(ctx context.Context, kubeconfig string, kubeContext string, namespace string) error {
	config, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return err
	}

	namespace = resolveTargetNamespace(namespace, kubeconfig, kubeContext)

	killed := 0
	listOptions := metav1.ListOptions{
		FieldSelector: "status.phase=Running",
		Limit:         kubernetesPodListLimit,
	}
	for {
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, listOptions)
		if err != nil {
			return fmt.Errorf("listing pods: %w", err)
		}

		for _, pod := range pods.Items {
			running := runningDebuxEphemeralContainers(&pod)
			for _, ec := range pod.Spec.EphemeralContainers {
				if _, ok := running[ec.Name]; !ok || !debuxEphemeralContainerHasMetadata(ec) {
					continue
				}
				if err := killInContainer(ctx, config, clientset, namespace, pod.Name, ec.Name); err != nil {
					fmt.Printf("Warning: failed to kill %s on %s/%s: %v\n", ec.Name, namespace, pod.Name, err)
					continue
				}
				fmt.Printf("Killed %s on %s/%s\n", ec.Name, namespace, pod.Name)
				killed++
			}
		}

		if pods.Continue == "" {
			break
		}
		listOptions.Continue = pods.Continue
	}

	// Copy pods are swept regardless of phase so kept and expired
	// (DeadlineExceeded) ones are cleaned up too.
	deletedCopies, err := deleteAllKubernetesCopyPods(ctx, clientset, namespace)
	if err != nil {
		return err
	}

	if killed == 0 && deletedCopies == 0 {
		fmt.Println("No running debux sessions found")
		return nil
	}
	if killed > 0 {
		fmt.Printf("Killed %d debug session(s)\n", killed)
	}
	if deletedCopies > 0 {
		fmt.Printf("Deleted %d debug copy pod(s)\n", deletedCopies)
	}
	return nil
}

// deleteAllKubernetesCopyPods deletes every debux copy pod in the namespace,
// including terminated ones left behind by activeDeadlineSeconds.

func deleteAllKubernetesCopyPods(ctx context.Context, clientset *kubernetes.Clientset, namespace string) (int, error) {
	deleted := 0
	listOptions := metav1.ListOptions{
		LabelSelector: debuxManagedByLabelKey + "=" + debuxManagedByLabelValue + "," + debuxModeLabelKey + "=" + debuxModeCopy,
		Limit:         kubernetesPodListLimit,
	}
	for {
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, listOptions)
		if err != nil {
			return deleted, fmt.Errorf("listing debug copy pods: %w", err)
		}
		for _, pod := range pods.Items {
			if err := clientset.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
				fmt.Printf("Warning: failed to delete copy pod %s/%s: %v\n", namespace, pod.Name, err)
				continue
			}
			fmt.Printf("Deleted debug copy pod %s/%s\n", namespace, pod.Name)
			deleted++
		}
		if pods.Continue == "" {
			break
		}
		listOptions.Continue = pods.Continue
	}
	return deleted, nil
}

// killDebuxDaemonScript locates and signals the debux daemon process inside a
// debug container. The debug container shares the TARGET container's PID
// namespace, so PID 1 there is the target application's init process —
// signaling it would stop (and restart) the production container.
//
// The daemon records its own PID in /tmp/.debux-daemon.pid (private to the
// debug container). For sessions from older debux versions, or if the file is
// unusable, fall back to scanning /proc for the daemon shell matched by its
// entrypoint cmdline — NOT by the DEBUX_DAEMON environment marker, which the
// daemon's sleep child inherits; killing that child only makes the daemon
// respawn it and survive.
const killDebuxDaemonScript = `pid="$(cat /tmp/.debux-daemon.pid 2>/dev/null || true)"
case "$pid" in
  ''|*[!0-9]*) pid="" ;;
esac
if [ -n "$pid" ] && { [ "$pid" = "1" ] || ! kill -0 "$pid" 2>/dev/null; }; then
  pid=""
fi
if [ -z "$pid" ]; then
  for d in /proc/[0-9]*; do
    p="${d##*/}"
    [ "$p" = "$$" ] && continue
    [ "$p" = "1" ] && continue
    if tr '\0' ' ' < "$d/cmdline" 2>/dev/null | grep -q 'DEBUX_TARGET_ROOT'; then
      pid="$p"
      break
    fi
  done
fi
if [ -z "$pid" ] || [ "$pid" = "1" ]; then
  echo "debux daemon process not found in this container" >&2
  exit 1
fi
kill "$pid"
`

// killInContainer terminates the debux daemon process inside a debug
// container. It must never signal PID 1: in the shared PID namespace that is
// the target application's process, not the debug daemon.

func killInContainer(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   []string{"/bin/sh", "-c", killDebuxDaemonScript},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w", msg, err)
		}
		return err
	}
	return nil
}
