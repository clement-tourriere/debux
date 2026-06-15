package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clement-tourriere/debux/internal/entrypoint"
	"github.com/moby/term"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"
)

// SecurityContextForProfile returns the SecurityContext for the given profile.
func SecurityContextForProfile(profile string) (*corev1.SecurityContext, error) {
	switch profile {
	case ProfileGeneral, "":
		// Run as root (UID 0) with capabilities needed for debugging:
		// - SYS_PTRACE: ptrace system calls for debugging
		// - SYS_ADMIN: namespace operations and /proc/1/root access
		// - SYS_CHROOT: execute target binaries via chroot
		var uid int64 = 0
		return &corev1.SecurityContext{
			RunAsNonRoot: &[]bool{false}[0],
			RunAsUser:    &uid,
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"SYS_PTRACE", "SYS_ADMIN", "SYS_CHROOT"},
			},
		}, nil
	case ProfileBaseline:
		return nil, nil
	case ProfileRestricted:
		f := false
		var uid int64 = 65534
		return &corev1.SecurityContext{
			RunAsNonRoot:             &[]bool{true}[0],
			RunAsUser:                &uid,
			AllowPrivilegeEscalation: &f,
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		}, nil
	case ProfileNetadmin:
		return &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
			},
		}, nil
	case ProfileSysadmin:
		t := true
		return &corev1.SecurityContext{
			Privileged: &t,
		}, nil
	default:
		return nil, fmt.Errorf("unknown profile: %s", profile)
	}
}

// PodInfo holds metadata about a running Kubernetes pod.
type PodInfo struct {
	Name            string
	Namespace       string
	Context         string
	Status          string
	Containers      []string
	HasDebuxSession bool // true if pod has a running debux ephemeral container or is a debux copy pod
}

// KubeContextInfo holds kubeconfig context metadata for interactive navigation.
type KubeContextInfo struct {
	Name      string
	Namespace string
	Cluster   string
	AuthInfo  string
	Current   bool
}

// NamespaceInfo holds Kubernetes namespace metadata for interactive navigation.
type NamespaceInfo struct {
	Name   string
	Status string
}

// KubernetesList returns running pods, optionally filtered by namespace.
func KubernetesList(ctx context.Context, kubeconfig string, kubeContext string, namespace string) ([]PodInfo, error) {
	return kubernetesListPods(ctx, kubeconfig, kubeContext, namespace, "")
}

// KubernetesCurrentContext returns the current kubeconfig context name, if any.
func KubernetesCurrentContext(kubeconfig string) (string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	raw, err := loadingRules.Load()
	if err != nil {
		return "", fmt.Errorf("loading kubeconfig: %w", err)
	}
	return raw.CurrentContext, nil
}

// KubernetesDefaultNamespace returns the namespace selected by kubeconfig for a context.
func KubernetesDefaultNamespace(kubeconfig, kubeContext string) string {
	return resolveNamespace(kubeconfig, kubeContext)
}

// KubernetesContexts returns kubeconfig contexts without contacting the cluster.
func KubernetesContexts(kubeconfig string) ([]KubeContextInfo, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	raw, err := loadingRules.Load()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	contexts := make([]KubeContextInfo, 0, len(raw.Contexts))
	for name, ctx := range raw.Contexts {
		ns := ctx.Namespace
		if ns == "" {
			ns = "default"
		}
		contexts = append(contexts, KubeContextInfo{
			Name:      name,
			Namespace: ns,
			Cluster:   ctx.Cluster,
			AuthInfo:  ctx.AuthInfo,
			Current:   name == raw.CurrentContext,
		})
	}
	sort.SliceStable(contexts, func(i, j int) bool {
		if contexts[i].Current != contexts[j].Current {
			return contexts[i].Current
		}
		return contexts[i].Name < contexts[j].Name
	})
	return contexts, nil
}

// KubernetesNamespaces returns namespaces visible in the selected context.
func KubernetesNamespaces(ctx context.Context, kubeconfig, kubeContext string) ([]NamespaceInfo, error) {
	_, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	var result []NamespaceInfo
	listOptions := metav1.ListOptions{Limit: 200}
	for {
		namespaces, err := clientset.CoreV1().Namespaces().List(ctx, listOptions)
		if err != nil {
			return nil, fmt.Errorf("listing namespaces: %w", err)
		}
		for _, ns := range namespaces.Items {
			result = append(result, NamespaceInfo{Name: ns.Name, Status: string(ns.Status.Phase)})
		}
		if namespaces.Continue == "" {
			break
		}
		listOptions.Continue = namespaces.Continue
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// KubernetesSessions returns running debux Kubernetes sessions that can be reattached.
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
func KubernetesBrowsePods(ctx context.Context, kubeconfig, kubeContext, namespace, query string, maxResults int) ([]PodInfo, bool, error) {
	config, _, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return nil, false, err
	}
	metadataClient, err := metadata.NewForConfig(config)
	if err != nil {
		return nil, false, fmt.Errorf("creating Kubernetes metadata client: %w", err)
	}
	if maxResults <= 0 {
		maxResults = 300
	}
	namespace = resolveTargetNamespace(namespace, kubeconfig, kubeContext)
	query = strings.ToLower(strings.TrimSpace(query))

	listLimit := int64(200)
	if maxResults > 0 && maxResults < int(listLimit) {
		listLimit = int64(maxResults)
	}

	var result []PodInfo
	listOptions := metav1.ListOptions{
		FieldSelector: "status.phase=Running",
		Limit:         listLimit,
	}
	for {
		pods, err := metadataClient.Resource(podsResource).Namespace(namespace).List(ctx, listOptions)
		if err != nil {
			return nil, false, fmt.Errorf("listing pods: %w", err)
		}
		for _, pod := range pods.Items {
			if query != "" && !matchesPodQuery(pod.Namespace, pod.Name, query) {
				continue
			}
			result = append(result, PodInfo{Name: pod.Name, Namespace: pod.Namespace, Context: kubeContext, Status: "Running"})
			if len(result) >= maxResults {
				return result, true, nil
			}
		}
		if pods.Continue == "" {
			break
		}
		listOptions.Continue = pods.Continue
	}
	return result, false, nil
}

const (
	kubernetesPodListLimit       int64 = 50
	kubernetesPodListResultLimit int   = 500
	kubernetesPodListEnrichLimit int   = 100
)

var podsResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

func kubernetesListPods(ctx context.Context, kubeconfig, kubeContext, namespace, query string) ([]PodInfo, error) {
	config, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	metadataClient, err := metadata.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes metadata client: %w", err)
	}

	listNs := resolveTargetNamespace(namespace, kubeconfig, kubeContext)
	query = strings.ToLower(strings.TrimSpace(query))

	var result []PodInfo
	listOptions := metav1.ListOptions{
		FieldSelector: "status.phase=Running",
		Limit:         kubernetesPodListLimit,
	}

	for {
		pods, err := metadataClient.Resource(podsResource).Namespace(listNs).List(ctx, listOptions)
		if err != nil {
			return nil, fmt.Errorf("listing pods: %w", err)
		}

		for _, pod := range pods.Items {
			if query != "" && !matchesPodQuery(pod.Namespace, pod.Name, query) {
				continue
			}
			result = append(result, PodInfo{
				Name:      pod.Name,
				Namespace: pod.Namespace,
				Context:   kubeContext,
				Status:    "Running",
			})
			if len(result) >= kubernetesPodListResultLimit {
				return result, nil
			}
		}

		if pods.Continue == "" {
			break
		}
		listOptions.Continue = pods.Continue
	}

	enrichPodInfos(ctx, clientset, result, kubernetesPodListEnrichLimit)
	return result, nil
}

func enrichPodInfos(ctx context.Context, clientset *kubernetes.Clientset, pods []PodInfo, limit int) {
	if limit <= 0 {
		return
	}
	if len(pods) < limit {
		limit = len(pods)
	}
	for i := 0; i < limit; i++ {
		pod, err := clientset.CoreV1().Pods(pods[i].Namespace).Get(ctx, pods[i].Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		pods[i].Status = string(pod.Status.Phase)
		pods[i].Containers = podContainerNames(pod)
		pods[i].HasDebuxSession = findRunningDebuxContainer(pod) != "" || isKubernetesCopyPod(pod)
	}
}

func matchesPodQuery(namespace, name, query string) bool {
	name = strings.ToLower(name)
	namespacedName := strings.ToLower(namespace + "/" + name)
	return strings.Contains(name, query) || strings.Contains(namespacedName, query)
}

// KubernetesPodExists reports whether a pod exists in the resolved namespace.
func KubernetesPodExists(ctx context.Context, kubeconfig, kubeContext, namespace, podName string) (bool, error) {
	_, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return false, err
	}

	namespace = resolveTargetNamespace(namespace, kubeconfig, kubeContext)
	_, err = clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		return true, nil
	}
	if k8serrors.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("getting pod %s/%s: %w", namespace, podName, err)
}

// KubernetesFindPods returns running pods whose name contains query.
func KubernetesFindPods(ctx context.Context, kubeconfig, kubeContext, namespace, query string) ([]PodInfo, error) {
	return kubernetesListPods(ctx, kubeconfig, kubeContext, namespace, query)
}

// KubernetesRunningContainers returns the regular containers currently running
// in a pod, in pod spec order.
func KubernetesRunningContainers(ctx context.Context, kubeconfig, kubeContext, namespace, podName string) ([]string, error) {
	_, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	namespace = resolveTargetNamespace(namespace, kubeconfig, kubeContext)
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting pod %s/%s: %w", namespace, podName, err)
	}
	containers := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		if podContainerRunning(pod, c.Name) {
			containers = append(containers, c.Name)
		}
	}
	for _, sidecar := range podSidecarContainerNames(pod) {
		if podContainerRunning(pod, sidecar) {
			containers = append(containers, sidecar)
		}
	}
	// Hide the debux daemon container of a copy pod: reattaching targets the
	// app container, and a single remaining name skips the container picker.
	if isKubernetesCopyPod(pod) {
		if debugName := findCopyPodDebugContainer(pod); debugName != "" {
			withoutDebug := make([]string, 0, len(containers))
			for _, name := range containers {
				if name != debugName {
					withoutDebug = append(withoutDebug, name)
				}
			}
			// Keep the debug container as a last resort so a copy pod whose
			// app container already exited still resolves.
			if len(withoutDebug) > 0 {
				containers = withoutDebug
			}
		}
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("pod %s/%s has no running regular containers to target (available: %s)", namespace, podName, strings.Join(podContainerNames(pod), ", "))
	}
	return containers, nil
}

// KubernetesKill terminates the debux ephemeral container on a specific pod by
// killing PID 1 inside it. K8s ephemeral containers cannot be removed from the
// pod spec, but killing their init process terminates them.
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

func newRemoteExecutor(config *rest.Config, reqURL *url.URL) (remotecommand.Executor, error) {
	spdyExec, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, reqURL)
	if err != nil {
		return nil, fmt.Errorf("creating SPDY executor: %w", err)
	}

	websocketExec, err := remotecommand.NewWebSocketExecutorForProtocols(
		config,
		http.MethodGet,
		reqURL.String(),
		remotecommandconsts.StreamProtocolV5Name,
		remotecommandconsts.StreamProtocolV4Name,
		remotecommandconsts.StreamProtocolV3Name,
		remotecommandconsts.StreamProtocolV2Name,
	)
	if err != nil {
		return nil, fmt.Errorf("creating websocket executor: %w", err)
	}

	exec, err := remotecommand.NewFallbackExecutor(spdyExec, websocketExec, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return nil, fmt.Errorf("creating fallback executor: %w", err)
	}

	return exec, nil
}

// KubernetesExec debugs a running pod using ephemeral containers.
// It reuses an existing running debux container when possible, or creates a new
// one in daemon mode (DEBUX_DAEMON=1) so it stays alive between sessions.
func KubernetesExec(ctx context.Context, target *Target, opts DebugOpts) error {
	config, clientset, err := getK8sClient(opts.Kubeconfig, opts.KubeContext)
	if err != nil {
		return err
	}

	namespace := resolveTargetNamespace(target.Namespace, opts.Kubeconfig, opts.KubeContext)
	podName := target.Name
	displayContext := kubernetesDisplayContext(opts.Kubeconfig, opts.KubeContext)

	// Get the target pod
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting pod %s/%s: %w", namespace, podName, err)
	}

	// Targeting a debux copy pod (e.g. one kept with --keep) reattaches to its
	// debug container instead of debugging the copy as a fresh target.
	if isKubernetesCopyPod(pod) {
		return kubernetesReattachToCopyPod(ctx, config, clientset, namespace, pod, target.Container, displayContext, opts)
	}

	// Pod-copy mode only needs the source container's spec (mounts, env), not
	// a live process — so crash-looping pods, its main rescue scenario, work.
	if opts.Copy {
		targetContainer, err := selectKubernetesCopyTargetContainer(pod, target.Container)
		if err != nil {
			return err
		}
		return kubernetesExecWithPodCopy(ctx, config, clientset, namespace, pod, targetContainer, opts, displayContext)
	}

	containerName, debuxTarget, err := ensureKubernetesDebugContainer(ctx, clientset, namespace, pod, target.Container, displayContext, opts)
	if err != nil {
		return err
	}

	fmt.Printf("Debugging %s/%s (container: %s)\n", namespace, podName, containerName)

	// Exec into the daemon container to start an interactive shell
	return execInPodWithMetadata(ctx, config, clientset, namespace, podName, containerName, debuxTarget, displayContext, opts.Command)
}

// ensureKubernetesDebugContainer reuses a matching running debux ephemeral
// container or creates a new daemon one and waits for it to start. It returns
// the debug container name and the display label for the session.
func ensureKubernetesDebugContainer(ctx context.Context, clientset *kubernetes.Clientset, namespace string, pod *corev1.Pod, requestedContainer, displayContext string, opts DebugOpts) (string, string, error) {
	podName := pod.Name

	if pod.DeletionTimestamp != nil {
		return "", "", fmt.Errorf("pod %s/%s is terminating; wait for the replacement pod or pick another target", namespace, podName)
	}

	// Determine the target container name. Prefer a running container so PID/root
	// namespace targeting works even when the first spec container is not ready.
	targetContainer, err := selectKubernetesTargetContainer(pod, requestedContainer)
	if err != nil {
		return "", "", err
	}

	debuxTarget := kubernetesDebugTargetLabel(displayContext, namespace, podName, targetContainer)

	// Try to reuse an existing running debux container for the same target container.
	if !opts.Fresh {
		if existing := findRunningDebuxContainerForTarget(pod, targetContainer, opts.Profile, opts.User, opts.Image); existing != "" {
			fmt.Printf("Reusing debug container %q\n", existing)
			return existing, debuxTarget, nil
		}
		if other := findRunningDebuxContainerForTarget(pod, targetContainer, opts.Profile, opts.User, ""); other != "" {
			fmt.Printf("Existing debug container %q uses a different image; creating a new one with %s\n", other, opts.Image)
		}
	}

	// Create a new ephemeral container in daemon mode.
	// Use nanoseconds to avoid name collisions when retrying after a fast failure;
	// ephemeral containers cannot be removed from the pod spec.
	debugContainerName := fmt.Sprintf("debux-%d", time.Now().UnixNano())

	ephemeralContainer := corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:            debugContainerName,
			Image:           opts.Image,
			ImagePullPolicy: corev1.PullPolicy(opts.PullPolicy),
			Command:         []string{"/bin/sh", "-c", entrypoint.Script},
			Stdin:           true,
			TTY:             true,
			Env: []corev1.EnvVar{
				{Name: "DEBUX_TARGET", Value: debuxTarget},
				{Name: "DEBUX_CONTEXT", Value: displayContext},
				{Name: "DEBUX_TARGET_ROOT", Value: "/proc/1/root"},
				{Name: "DEBUX_DAEMON", Value: "1"},
				{Name: "DEBUX_SECURITY_PROFILE", Value: opts.Profile},
				{Name: "DEBUX_DEBUG_USER", Value: opts.User},
				{Name: "HOME", Value: "/root"},
				{Name: "ZDOTDIR", Value: "/tmp"},
			},
		},
		TargetContainerName: targetContainer,
	}
	ephemeralContainer.Env = append(ephemeralContainer.Env, kubernetesEnvVars(debugExtraEnv(opts.Env, opts.Tools))...)

	// Share target container's volume mounts (skip ones with SubPath, not allowed on ephemeral containers).
	if opts.ShareVolumes {
		ephemeralContainer.VolumeMounts = targetKubernetesVolumeMounts(pod, targetContainer, opts.ReadOnlyVolumes, false)
	}

	sc, err := SecurityContextForProfile(opts.Profile)
	if err != nil {
		return "", "", err
	}
	if opts.User != "" {
		sc, err = applyKubernetesUser(sc, opts.User)
		if err != nil {
			return "", "", err
		}
	}
	sc = applyExtraCapabilities(sc, opts.CapAdd)
	if sc != nil {
		ephemeralContainer.SecurityContext = sc
	}

	// Add the ephemeral container to the pod spec and update via the
	// ephemeralcontainers subresource (PUT), matching kubectl debug behavior.
	patchedPod, err := updateEphemeralContainersWithRetry(ctx, clientset, namespace, podName, pod, ephemeralContainer)
	if err != nil {
		return "", "", err
	}

	// Verify the ephemeral container actually appears in the patched pod.
	// Admission controllers or webhooks can silently strip it.
	found := false
	for _, ec := range patchedPod.Spec.EphemeralContainers {
		if ec.Name == debugContainerName {
			found = true
			break
		}
	}
	if !found {
		return "", "", fmt.Errorf("ephemeral container %q was not created — the API server accepted the patch but the container is missing from the pod spec.\n"+
			"This typically means an admission webhook or policy (e.g. Gatekeeper, Kyverno, PodSecurity) stripped it.\n"+
			"Check cluster events and webhook configurations:\n"+
			"  kubectl get events -n %s --field-selector involvedObject.name=%s\n"+
			"  kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations",
			debugContainerName, namespace, podName)
	}

	fmt.Printf("Waiting for debug container %q to start...\n", debugContainerName)

	// Wait for the ephemeral container to be running.
	// Pass the resourceVersion from the update response so the watch starts
	// from the right point and we don't miss status changes that happen
	// between the update and the watch setup.
	if err := waitForEphemeralContainer(ctx, clientset, namespace, podName, debugContainerName, patchedPod.ResourceVersion); err != nil {
		return "", "", err
	}

	return debugContainerName, debuxTarget, nil
}

// updateEphemeralContainersWithRetry handles 409 Conflicts from controllers
// touching the pod between our Get and the update by refetching the pod and
// reapplying the ephemeral container.
func updateEphemeralContainersWithRetry(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName string, pod *corev1.Pod, ec corev1.EphemeralContainer) (*corev1.Pod, error) {
	for attempt := 0; ; attempt++ {
		pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, ec)
		patched, err := clientset.CoreV1().Pods(namespace).UpdateEphemeralContainers(ctx, podName, pod, metav1.UpdateOptions{})
		if err == nil {
			return patched, nil
		}
		if k8serrors.IsConflict(err) && attempt < 2 {
			refreshed, getErr := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
			if getErr == nil {
				pod = refreshed
				continue
			}
		}
		if k8serrors.IsForbidden(err) {
			return nil, fmt.Errorf("updating ephemeral containers: %w\nHint: your RBAC may not allow pods/ephemeralcontainers. Retry with --copy to use pod-copy debug mode", err)
		}
		return nil, fmt.Errorf("updating ephemeral containers: %w", err)
	}
}

// Labels and annotations stamped on debux-created copy pods.
const (
	debuxManagedByLabelKey   = "app.kubernetes.io/managed-by"
	debuxManagedByLabelValue = "debux"
	debuxModeLabelKey        = "debux.clement-tourriere/mode"
	debuxModeCopy            = "copy"

	debuxSourcePodAnnotation       = "debux.clement-tourriere/source-pod"
	debuxTargetContainerAnnotation = "debux.clement-tourriere/target-container"

	// karpenterDoNotDisruptAnnotation blocks Karpenter's voluntary node
	// disruption (consolidation, drift) while the copy pod runs. The value can
	// be a Go duration, which bounds the protection window; Karpenter also
	// ignores terminal pods, so protection lapses once activeDeadlineSeconds
	// expires the pod either way.
	karpenterDoNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"
	// clusterAutoscalerSafeToEvictAnnotation is the cluster-autoscaler
	// equivalent: "false" blocks scale-down of the node while the pod runs.
	clusterAutoscalerSafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

// buildKubernetesCopyPod renders the copy-mode debug pod for a source pod. It
// returns the pod to create and the name of the debug container inside it.
func buildKubernetesCopyPod(namespace string, sourcePod *corev1.Pod, targetContainer string, opts DebugOpts, displayContext string) (*corev1.Pod, string, error) {
	spec := *sourcePod.Spec.DeepCopy()

	// The copied pod should be scheduler-managed, not pinned to the original node.
	spec.NodeName = ""
	spec.EphemeralContainers = nil

	// Match kubectl debug --copy-to --share-processes behavior.
	shareProcesses := true
	spec.ShareProcessNamespace = &shareProcesses

	// Nothing owns the copy pod, so one leaked by an unclean CLI exit would
	// run forever. A kubelet-enforced deadline guarantees its containers stop
	// even if this process never gets to clean up.
	if opts.TTL > 0 {
		deadline := int64(opts.TTL / time.Second)
		if deadline < 1 {
			deadline = 1
		}
		spec.ActiveDeadlineSeconds = &deadline
	} else {
		spec.ActiveDeadlineSeconds = nil
	}

	existingNames := make(map[string]struct{}, len(spec.Containers))
	for _, c := range spec.Containers {
		existingNames[c.Name] = struct{}{}
	}

	debugContainerName := "debux"
	if _, exists := existingNames[debugContainerName]; exists {
		for i := 1; ; i++ {
			candidate := fmt.Sprintf("debux-%d", i)
			if _, taken := existingNames[candidate]; !taken {
				debugContainerName = candidate
				break
			}
		}
	}

	debugContainer := corev1.Container{
		Name:            debugContainerName,
		Image:           opts.Image,
		ImagePullPolicy: corev1.PullPolicy(opts.PullPolicy),
		Command:         []string{"/bin/sh", "-c", entrypoint.Script},
		Stdin:           true,
		TTY:             true,
		Env: []corev1.EnvVar{
			{Name: "DEBUX_TARGET", Value: kubernetesDebugTargetLabel(displayContext, namespace, sourcePod.Name, targetContainer)},
			{Name: "DEBUX_CONTEXT", Value: displayContext},
			{Name: "DEBUX_DAEMON", Value: "1"},
			{Name: "DEBUX_SECURITY_PROFILE", Value: opts.Profile},
			{Name: "DEBUX_DEBUG_USER", Value: opts.User},
			{Name: "HOME", Value: "/root"},
			{Name: "ZDOTDIR", Value: "/tmp"},
		},
	}
	debugContainer.Env = append(debugContainer.Env, kubernetesEnvVars(debugExtraEnv(opts.Env, opts.Tools))...)

	if opts.ShareVolumes {
		debugContainer.VolumeMounts = targetKubernetesVolumeMounts(sourcePod, targetContainer, opts.ReadOnlyVolumes, true)
	}

	sc, err := SecurityContextForProfile(opts.Profile)
	if err != nil {
		return nil, "", err
	}
	if opts.User != "" {
		sc, err = applyKubernetesUser(sc, opts.User)
		if err != nil {
			return nil, "", err
		}
	}
	sc = applyExtraCapabilities(sc, opts.CapAdd)
	if sc != nil {
		debugContainer.SecurityContext = sc
	}

	spec.Containers = append(spec.Containers, debugContainer)

	// Bound the Karpenter protection to the TTL. Without a TTL the user
	// explicitly opted into an unbounded pod, so protect it unconditionally.
	doNotDisrupt := "true"
	if opts.TTL > 0 {
		doNotDisrupt = opts.TTL.String()
	}

	copyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "debux-copy-",
			Namespace:    namespace,
			Labels: map[string]string{
				debuxManagedByLabelKey: debuxManagedByLabelValue,
				debuxModeLabelKey:      debuxModeCopy,
			},
			Annotations: map[string]string{
				debuxSourcePodAnnotation:               sourcePod.Name,
				debuxTargetContainerAnnotation:         targetContainer,
				karpenterDoNotDisruptAnnotation:        doNotDisrupt,
				clusterAutoscalerSafeToEvictAnnotation: "false",
			},
		},
		Spec: spec,
	}
	return copyPod, debugContainerName, nil
}

func kubernetesExecWithPodCopy(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace string, sourcePod *corev1.Pod, targetContainer string, opts DebugOpts, displayContext string) error {
	copyPod, debugContainerName, err := buildKubernetesCopyPod(namespace, sourcePod, targetContainer, opts, displayContext)
	if err != nil {
		return err
	}

	created, err := clientset.CoreV1().Pods(namespace).Create(ctx, copyPod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating debug copy pod: %w", err)
	}

	// --keep is only honored once the pod is up: a copy that never started is
	// deleted even with --keep so failed attempts do not accumulate.
	keepPod := false
	defer func() {
		if keepPod {
			printKeptCopyPod(displayContext, namespace, created.Name, opts.TTL)
			return
		}
		fmt.Printf("Deleting debug copy pod %s...\n", created.Name)
		_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	}()

	fmt.Printf("Waiting for debug copy pod %q to start...\n", created.Name)
	if err := waitForContainerRunning(ctx, clientset, namespace, created.Name, debugContainerName, created.ResourceVersion); err != nil {
		return err
	}
	keepPod = opts.Keep

	runningCopyPod, err := clientset.CoreV1().Pods(namespace).Get(ctx, created.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting debug copy pod %s/%s: %w", namespace, created.Name, err)
	}

	targetContainerID := findContainerID(runningCopyPod, targetContainer)
	if targetContainerID == "" {
		fmt.Printf("Warning: could not resolve container ID for target container %q in copy pod %s/%s; filesystem/env integration may be limited\n", targetContainer, namespace, created.Name)
	}

	fmt.Printf("Debugging copy %s/%s (container: %s, source: %s)\n", namespace, created.Name, debugContainerName, sourcePod.Name)
	if opts.TTL > 0 {
		fmt.Printf("Copy pod auto-expires in %s (activeDeadlineSeconds)\n", opts.TTL)
	}

	return execInPodWithCommand(ctx, config, clientset, namespace, created.Name, debugContainerName, copyPodShellCommand(targetContainerID, opts.Command))
}

// printKeptCopyPod tells the user how to get back to (or get rid of) a copy
// pod that outlives this session.
func printKeptCopyPod(kubeContext, namespace, podName string, ttl time.Duration) {
	targetURI := kubernetesTargetURI(kubeContext, namespace, podName)
	printTerminalStatusLine("Keeping debug copy pod %s/%s", namespace, podName)
	if ttl > 0 {
		printTerminalStatusLine("  Expires:  %s after pod start, then the kubelet stops its containers", ttl)
	} else {
		printTerminalStatusLine("  Warning:  no TTL (--ttl=0); the pod runs until deleted")
	}
	printTerminalStatusLine("  Reattach: debux %s", targetURI)
	printTerminalStatusLine("  Delete:   debux kill %s", targetURI)
}

func printTerminalStatusLine(format string, args ...any) {
	if stdioIsTTY() {
		fmt.Printf(format+"\033[K\n", args...)
		return
	}
	fmt.Printf(format+"\n", args...)
}

func kubernetesTargetURI(kubeContext, namespace, podName string) string {
	return kubernetesTargetURIWithContainer(kubeContext, namespace, podName, "")
}

func kubernetesTargetURIWithContainer(kubeContext, namespace, podName, containerName string) string {
	parts := []string{url.PathEscape(namespace), url.PathEscape(podName)}
	if containerName != "" {
		parts = append(parts, url.PathEscape(containerName))
	}
	if kubeContext != "" {
		return "k8s://@" + url.PathEscape(kubeContext) + "/" + strings.Join(parts, "/")
	}
	return "k8s://" + strings.Join(parts, "/")
}

// isKubernetesCopyPod reports whether pod is a copy pod created by debux
// --copy mode, as opposed to a user pod debux debugs via ephemeral containers.
func isKubernetesCopyPod(pod *corev1.Pod) bool {
	return pod.Labels[debuxManagedByLabelKey] == debuxManagedByLabelValue &&
		pod.Labels[debuxModeLabelKey] == debuxModeCopy
}

// findCopyPodDebugContainer returns the name of the debux debug container in a
// copy pod, identified by its DEBUX_DAEMON marker env var (the name can be
// debux-N when the source pod already had a container named debux).
func findCopyPodDebugContainer(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		for _, env := range c.Env {
			if env.Name == "DEBUX_DAEMON" && env.Value == "1" {
				return c.Name
			}
		}
	}
	return ""
}

// kubernetesReattachToCopyPod opens a shell inside the debug container of an
// existing debux copy pod (typically one created with --keep), instead of
// debugging the copy pod as if it were a regular target.
func kubernetesReattachToCopyPod(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace string, pod *corev1.Pod, requestedContainer, displayContext string, opts DebugOpts) error {
	killHint := fmt.Sprintf("debux kill %s", kubernetesTargetURI(displayContext, namespace, pod.Name))

	if pod.DeletionTimestamp != nil {
		return fmt.Errorf("debug copy pod %s/%s is terminating; start a new --copy session against the source pod", namespace, pod.Name)
	}

	debugContainerName := findCopyPodDebugContainer(pod)
	if debugContainerName == "" {
		return fmt.Errorf("pod %s/%s is labeled as a debux copy pod but has no debux debug container", namespace, pod.Name)
	}

	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		reason := pod.Status.Reason
		if reason == "" {
			reason = string(pod.Status.Phase)
		}
		return fmt.Errorf("debug copy pod %s/%s already terminated (%s); delete it with %q and start a new --copy session", namespace, pod.Name, reason, killHint)
	}
	if !podContainerRunning(pod, debugContainerName) {
		return fmt.Errorf("debug container %q in copy pod %s/%s is not running; delete the pod with %q and start a new --copy session", debugContainerName, namespace, pod.Name, killHint)
	}

	// Re-resolve the chroot target: an explicit container wins, otherwise the
	// one recorded when the copy was created.
	targetContainer := requestedContainer
	if targetContainer == "" || targetContainer == debugContainerName {
		targetContainer = pod.Annotations[debuxTargetContainerAnnotation]
	}
	targetContainerID := ""
	if targetContainer != "" {
		targetContainerID = findContainerID(pod, targetContainer)
	}

	source := pod.Annotations[debuxSourcePodAnnotation]
	if source == "" {
		source = "unknown"
	}
	fmt.Printf("Reattaching to debug copy pod %s/%s (container: %s, source: %s)\n", namespace, pod.Name, debugContainerName, source)
	if remaining, ok := copyPodTimeRemaining(pod); ok {
		fmt.Printf("Copy pod expires in %s (activeDeadlineSeconds)\n", remaining)
	}

	defer fmt.Printf("Detached from %s/%s; the pod is kept. Delete it with: %s\n", namespace, pod.Name, killHint)
	return execInPodWithCommand(ctx, config, clientset, namespace, pod.Name, debugContainerName, copyPodShellCommand(targetContainerID, opts.Command))
}

// copyPodTimeRemaining returns how long the pod has left before its
// activeDeadlineSeconds deadline expires it.
func copyPodTimeRemaining(pod *corev1.Pod) (time.Duration, bool) {
	if pod.Spec.ActiveDeadlineSeconds == nil || pod.Status.StartTime == nil {
		return 0, false
	}
	expiry := pod.Status.StartTime.Add(time.Duration(*pod.Spec.ActiveDeadlineSeconds) * time.Second)
	remaining := time.Until(expiry)
	if remaining < 0 {
		remaining = 0
	}
	return remaining.Truncate(time.Second), true
}

func kubernetesDisplayContext(kubeconfig, kubeContext string) string {
	if kubeContext != "" {
		return kubeContext
	}
	current, err := KubernetesCurrentContext(kubeconfig)
	if err != nil {
		return ""
	}
	return current
}

func kubernetesDebugTargetLabel(kubeContext, namespace, podName, containerName string) string {
	label := fmt.Sprintf("%s/%s/%s", namespace, podName, containerName)
	if kubeContext != "" {
		return fmt.Sprintf("%s:%s", kubeContext, label)
	}
	return label
}

// kubernetesEnvVars converts KEY=VALUE entries (already validated by the CLI)
// into Kubernetes EnvVars.
func kubernetesEnvVars(env []string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		out = append(out, corev1.EnvVar{Name: key, Value: value})
	}
	return out
}

// applyExtraCapabilities merges --cap-add capabilities into a profile's
// security context.
func applyExtraCapabilities(sc *corev1.SecurityContext, caps []string) *corev1.SecurityContext {
	caps = normalizeCapabilities(caps)
	if len(caps) == 0 {
		return sc
	}
	if sc != nil {
		sc = sc.DeepCopy()
	} else {
		sc = &corev1.SecurityContext{}
	}
	if sc.Capabilities == nil {
		sc.Capabilities = &corev1.Capabilities{}
	}
	for _, c := range caps {
		sc.Capabilities.Add = append(sc.Capabilities.Add, corev1.Capability(c))
	}
	return sc
}

func applyKubernetesUser(sc *corev1.SecurityContext, user string) (*corev1.SecurityContext, error) {
	parts := strings.Split(user, ":")
	if len(parts) > 2 || parts[0] == "" {
		return nil, fmt.Errorf("invalid --user %q: expected uid or uid:gid", user)
	}

	uid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || uid < 0 {
		return nil, fmt.Errorf("invalid --user %q: uid must be a non-negative integer", user)
	}

	var out *corev1.SecurityContext
	if sc != nil {
		out = sc.DeepCopy()
	} else {
		out = &corev1.SecurityContext{}
	}

	out.RunAsUser = &uid
	runAsNonRoot := uid != 0
	out.RunAsNonRoot = &runAsNonRoot

	if len(parts) == 2 && parts[1] != "" {
		gid, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || gid < 0 {
			return nil, fmt.Errorf("invalid --user %q: gid must be a non-negative integer", user)
		}
		out.RunAsGroup = &gid
	}

	return out, nil
}

func isReservedDebugMountPath(mountPath string) bool {
	switch mountPath {
	// /tmp and /root hold the debug shell's own state (ZDOTDIR, HOME).
	// Mounting target volumes there breaks sessions on pods that keep an
	// emptyDir at /tmp (standard with readOnlyRootFilesystem) and writes
	// debux files into target data. The target's /tmp stays reachable via
	// $DEBUX_TARGET_ROOT/tmp.
	case "/nix", "/nix/store", "/nix/var", "/tmp", "/root":
		return true
	default:
		return false
	}
}

func targetKubernetesVolumeMounts(pod *corev1.Pod, targetContainer string, readOnly bool, allowSubPath bool) []corev1.VolumeMount {
	for _, c := range pod.Spec.Containers {
		if c.Name != targetContainer {
			continue
		}

		mounts := make([]corev1.VolumeMount, 0, len(c.VolumeMounts))
		for _, vm := range c.VolumeMounts {
			if !allowSubPath && (vm.SubPath != "" || vm.SubPathExpr != "") {
				continue
			}
			if isReservedDebugMountPath(vm.MountPath) {
				continue
			}
			if readOnly {
				vm.ReadOnly = true
			}
			mounts = append(mounts, vm)
		}
		return mounts
	}
	return nil
}

// selectKubernetesCopyTargetContainer picks the target container for pod-copy
// mode. The copy only needs the source container's spec, so containers that
// are not running (CrashLoopBackOff — the main --copy rescue case) are valid.
func selectKubernetesCopyTargetContainer(pod *corev1.Pod, requested string) (string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("pod %s/%s has no regular containers to target", pod.Namespace, pod.Name)
	}
	if requested != "" {
		if !podHasContainer(pod, requested) {
			return "", fmt.Errorf("pod %s/%s has no container %q (available: %s)",
				pod.Namespace, pod.Name, requested, strings.Join(podContainerNames(pod), ", "))
		}
		return requested, nil
	}
	// Prefer a running container, fall back to the first one in the spec.
	for _, c := range pod.Spec.Containers {
		if podContainerRunning(pod, c.Name) {
			return c.Name, nil
		}
	}
	return pod.Spec.Containers[0].Name, nil
}

func selectKubernetesTargetContainer(pod *corev1.Pod, requested string) (string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("pod %s/%s has no regular containers to target", pod.Namespace, pod.Name)
	}

	if requested != "" {
		if !podHasContainer(pod, requested) {
			return "", fmt.Errorf("pod %s/%s has no container %q (available: %s)",
				pod.Namespace, pod.Name, requested, strings.Join(podContainerNames(pod), ", "))
		}
		if !podContainerRunning(pod, requested) {
			return "", fmt.Errorf("container %q in pod %s/%s is not running; debux needs a running target container",
				requested, pod.Namespace, pod.Name)
		}
		return requested, nil
	}

	for _, c := range pod.Spec.Containers {
		if podContainerRunning(pod, c.Name) {
			return c.Name, nil
		}
	}

	return "", fmt.Errorf("pod %s/%s has no running regular containers to target (available: %s)",
		pod.Namespace, pod.Name, strings.Join(podContainerNames(pod), ", "))
}

// podSidecarContainerNames returns native sidecars (restartable init
// containers); they run for the pod's lifetime and are valid debug targets.
func podSidecarContainerNames(pod *corev1.Pod) []string {
	var names []string
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			names = append(names, c.Name)
		}
	}
	return names
}

func podHasContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	for _, sidecar := range podSidecarContainerNames(pod) {
		if sidecar == name {
			return true
		}
	}
	return false
}

func podContainerRunning(pod *corev1.Pod, name string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			return cs.State.Running != nil
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == name {
			return cs.State.Running != nil
		}
	}
	return false
}

func podContainerNames(pod *corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	names = append(names, podSidecarContainerNames(pod)...)
	return names
}

func findContainerID(pod *corev1.Pod, containerName string) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		containerID := strings.TrimSpace(cs.ContainerID)
		if idx := strings.Index(containerID, "://"); idx != -1 {
			containerID = containerID[idx+3:]
		}
		return containerID
	}
	return ""
}

func copyPodShellCommand(targetContainerID string, command []string) []string {
	cmd := debuxShellCommand(command)
	if targetContainerID != "" {
		shortID := targetContainerID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		cmd = fmt.Sprintf("target_cid=%q; target_cid_short=%q; target_pid=''; if [ -n \"$target_cid\" ]; then for p in /proc/[0-9]*; do [ -r \"$p/cgroup\" ] || continue; if grep -q \"$target_cid\" \"$p/cgroup\" 2>/dev/null || grep -q \"$target_cid_short\" \"$p/cgroup\" 2>/dev/null; then target_pid=\"${p##*/}\"; break; fi; done; fi; if [ -n \"$target_pid\" ] && [ -d \"/proc/$target_pid/root\" ]; then export DEBUX_TARGET_ROOT=\"/proc/$target_pid/root\"; export DEBUX_TARGET_ENVIRON=\"/proc/$target_pid/environ\"; export DEBUX_TARGET_CWD_LINK=\"/proc/$target_pid/cwd\"; fi; %s", targetContainerID, shortID, cmd)
	}
	return []string{"sh", "-c", cmd}
}

// findRunningDebuxContainer looks for an existing running Debux ephemeral
// container on the given pod. Returns its name, or "" if none found.
func findRunningDebuxContainer(pod *corev1.Pod) string {
	running := runningDebuxEphemeralContainers(pod)
	for _, ec := range pod.Spec.EphemeralContainers {
		if _, ok := running[ec.Name]; ok && debuxEphemeralContainerHasMetadata(ec) {
			return ec.Name
		}
	}
	return ""
}

func findRunningDebuxContainerForKill(pod *corev1.Pod, targetContainer string) string {
	if targetContainer == "" {
		return findRunningDebuxContainer(pod)
	}

	running := runningDebuxEphemeralContainers(pod)
	for _, ec := range pod.Spec.EphemeralContainers {
		if _, ok := running[ec.Name]; !ok {
			continue
		}
		if !debuxEphemeralContainerHasMetadata(ec) {
			continue
		}
		if ec.TargetContainerName == targetContainer {
			return ec.Name
		}
	}
	return ""
}

// findRunningDebuxContainerForTarget returns a running debux ephemeral container
// that targets the same Kubernetes container, security profile, user, and debug
// image. Reusing a session created for a different target container would put
// the shell in the wrong PID/root namespace; reusing a different profile, user,
// or image would silently ignore the flags the user passed. An empty image
// matches any (used to detect image-mismatched sessions for messaging).
func findRunningDebuxContainerForTarget(pod *corev1.Pod, targetContainer, profile, user, image string) string {
	running := runningDebuxEphemeralContainers(pod)

	for _, ec := range pod.Spec.EphemeralContainers {
		if _, ok := running[ec.Name]; !ok {
			continue
		}
		if !debuxEphemeralContainerHasMetadata(ec) {
			continue
		}
		if targetContainer != "" && ec.TargetContainerName != targetContainer {
			continue
		}
		if image != "" && ec.Image != image {
			continue
		}
		if !debuxEphemeralContainerProfileMatches(ec, profile) {
			continue
		}
		if !debuxEphemeralContainerUserMatches(ec, user) {
			continue
		}
		return ec.Name
	}

	return ""
}

func runningDebuxEphemeralContainers(pod *corev1.Pod) map[string]struct{} {
	running := make(map[string]struct{})
	for _, cs := range pod.Status.EphemeralContainerStatuses {
		if strings.HasPrefix(cs.Name, "debux-") && cs.State.Running != nil {
			running[cs.Name] = struct{}{}
		}
	}
	return running
}

func debuxEphemeralContainerHasMetadata(ec corev1.EphemeralContainer) bool {
	if !strings.HasPrefix(ec.Name, "debux-") {
		return false
	}
	for _, env := range ec.Env {
		if env.Name == "DEBUX_TARGET" || env.Name == "DEBUX_DAEMON" {
			return true
		}
	}
	return false
}

func debuxEphemeralContainerProfileMatches(ec corev1.EphemeralContainer, requested string) bool {
	if requested == "" {
		requested = ProfileGeneral
	}
	for _, env := range ec.Env {
		if env.Name == "DEBUX_SECURITY_PROFILE" {
			return env.Value == requested
		}
	}
	// Legacy debux containers did not record their profile. Treat them as the
	// historical default only; never satisfy an explicit lower-privilege request.
	return requested == ProfileGeneral
}

func debuxEphemeralContainerUserMatches(ec corev1.EphemeralContainer, requested string) bool {
	for _, env := range ec.Env {
		if env.Name == "DEBUX_DEBUG_USER" {
			return env.Value == requested
		}
	}
	// Legacy debux containers did not record --user. Reuse them only when the user
	// did not request a specific identity.
	return requested == ""
}

const debuxShellSetupCommand = `if [ -z "${HOME:-}" ] || [ ! -d "$HOME" ] || [ ! -w "$HOME" ]; then debux_uid="$(id -u 2>/dev/null || echo 0)"; export HOME="/tmp/debux-home-$debux_uid"; mkdir -p "$HOME" 2>/dev/null || export HOME=/tmp; unset debux_uid; fi; export ZDOTDIR=/tmp;`

const debuxZshExecCommand = debuxShellSetupCommand + ` exec zsh`

func execInPodWithMetadata(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName, targetLabel, kubeContext string, command []string) error {
	if err := bootstrapPodShell(ctx, config, clientset, namespace, podName, containerName); err != nil {
		return fmt.Errorf("preparing debux shell config: %w", err)
	}
	cmd := debuxExecCommand(command)
	cmd[2] = "export DEBUX_TARGET=" + shellQuote(targetLabel) + " DEBUX_CONTEXT=" + shellQuote(kubeContext) + "; " + cmd[2]
	return execInPodWithCommand(ctx, config, clientset, namespace, podName, containerName, cmd)
}

func bootstrapPodShell(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   []string{"/bin/sh"},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating bootstrap executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  strings.NewReader(entrypoint.ShellBootstrapScript() + "\n"),
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if err != nil {
		details := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if details != "" {
			return fmt.Errorf("running bootstrap: %w: %s", err, details)
		}
		return fmt.Errorf("running bootstrap: %w", err)
	}
	return nil
}

// kubernetesExecError converts the remote command's exit status into the
// typed ExitError so the CLI propagates the real code instead of printing a
// spurious "command terminated with exit code N".
func kubernetesExecError(err error) error {
	if err == nil {
		return nil
	}
	var codeErr k8sexec.CodeExitError
	if errors.As(err, &codeErr) {
		return &ExitError{Code: codeErr.Code}
	}
	return err
}

func execInPodWithCommand(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string, command []string) error {
	// Allocate a remote TTY only when stdio is a terminal, mirroring kubectl:
	// piped one-shot commands must not get CRLF-mangled, echo-polluted output.
	tty := stdioIsTTY()

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    !tty, // TTY merges stderr into stdout
			TTY:       tty,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Tty:    tty,
	}

	if tty {
		stdinFd, _ := term.GetFdInfo(os.Stdin)
		oldState, rawErr := term.SetRawTerminal(stdinFd)
		if rawErr == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
		tsq := newTerminalSizeQueue(stdinFd)
		defer tsq.Close()
		streamOpts.TerminalSizeQueue = tsq
		streamOpts.Stderr = &bytes.Buffer{}
	} else {
		streamOpts.Stderr = os.Stderr
	}

	return kubernetesExecError(exec.StreamWithContext(ctx, streamOpts))
}

// KubernetesPod creates a standalone debug pod.
func KubernetesPod(ctx context.Context, opts PodOpts) error {
	config, clientset, err := getK8sClient(opts.Kubeconfig, opts.KubeContext)
	if err != nil {
		return err
	}

	if opts.Namespace == "" {
		opts.Namespace = resolveNamespace(opts.Kubeconfig, opts.KubeContext)
	}

	// Nanosecond resolution avoids name collisions when several debug pods
	// are created in the same namespace within the same second.
	podName := fmt.Sprintf("debux-%d", time.Now().UnixNano())
	displayContext := kubernetesDisplayContext(opts.Kubeconfig, opts.KubeContext)
	displayTarget := fmt.Sprintf("pod/%s", podName)
	if displayContext != "" {
		displayTarget = fmt.Sprintf("%s:%s", displayContext, displayTarget)
	}

	tty := stdioIsTTY()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: opts.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "debux",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "debug",
					Image:           opts.Image,
					ImagePullPolicy: corev1.PullPolicy(opts.PullPolicy),
					Command:         []string{"/bin/sh", "-c", entrypoint.Script},
					Stdin:           true,
					TTY:             tty,
					Env: []corev1.EnvVar{
						{Name: "DEBUX_TARGET", Value: displayTarget},
						{Name: "DEBUX_CONTEXT", Value: displayContext},
						{Name: "DEBUX_SECURITY_PROFILE", Value: opts.Profile},
						{Name: "DEBUX_DEBUG_USER", Value: opts.User},
						{Name: "HOME", Value: "/root"},
						{Name: "ZDOTDIR", Value: "/tmp"},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			HostNetwork:   opts.HostNetwork,
		},
	}
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, kubernetesEnvVars(debugExtraEnv(opts.Env, opts.Tools))...)

	sc, err := SecurityContextForProfile(opts.Profile)
	if err != nil {
		return err
	}
	if opts.User != "" {
		sc, err = applyKubernetesUser(sc, opts.User)
		if err != nil {
			return err
		}
	}
	sc = applyExtraCapabilities(sc, opts.CapAdd)
	if sc != nil {
		pod.Spec.Containers[0].SecurityContext = sc
	}

	// Create the pod
	created, err := clientset.CoreV1().Pods(opts.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating debug pod: %w", err)
	}

	// Cleanup on exit
	if !opts.Keep {
		defer func() {
			fmt.Printf("Deleting debug pod %s...\n", podName)
			_ = clientset.CoreV1().Pods(opts.Namespace).Delete(
				context.Background(), podName, metav1.DeleteOptions{})
		}()
	}

	fmt.Printf("Waiting for debug pod %q to start...\n", podName)

	// Wait for the pod to be running
	if err := waitForPodRunning(ctx, clientset, opts.Namespace, created.Name); err != nil {
		return err
	}

	fmt.Printf("Attached to debug pod %s/%s\n", opts.Namespace, podName)

	return attachToPod(ctx, config, clientset, opts.Namespace, podName, "debug", tty)
}

// resolveNamespace returns the namespace from the current kubeconfig context,
// falling back to "default" if it cannot be determined.
func resolveNamespace(kubeconfig, kubeContext string) string {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	ns, _, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).Namespace()
	if err != nil || ns == "" {
		return "default"
	}
	return ns
}

func resolveTargetNamespace(namespace, kubeconfig, kubeContext string) string {
	if namespace != "" {
		return namespace
	}
	return resolveNamespace(kubeconfig, kubeContext)
}

func getK8sClient(kubeconfig, kubeContext string) (*rest.Config, *kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	if kubeconfig != "" || kubeContext != "" {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		if kubeconfig != "" {
			loadingRules.ExplicitPath = kubeconfig
		}
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			loadingRules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext},
		).ClientConfig()
	} else {
		// Try in-cluster first, then default kubeconfig
		config, err = rest.InClusterConfig()
		if err != nil {
			loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
			configOverrides := &clientcmd.ConfigOverrides{}
			config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				loadingRules, configOverrides).ClientConfig()
		}
	}

	if err != nil {
		return nil, nil, fmt.Errorf("building Kubernetes config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("creating Kubernetes client: %w", err)
	}

	return config, clientset, nil
}

func waitForEphemeralContainer(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName, containerName, resourceVersion string) error {
	watcher, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   fmt.Sprintf("metadata.name=%s", podName),
		ResourceVersion: resourceVersion,
	})
	if err != nil {
		return fmt.Errorf("watching pod: %w", err)
	}
	defer watcher.Stop()

	var lastReason string
	timeout := time.After(2 * time.Minute)
	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch closed while waiting for ephemeral container %q to start\n%s",
					containerName, describeContainerFailure(ctx, clientset, namespace, podName, containerName))
			}
			switch event.Type {
			case watch.Deleted:
				return fmt.Errorf("pod %q was deleted while waiting for debug container %q to start (rollout or eviction?)", podName, containerName)
			case watch.Error:
				return fmt.Errorf("watch error while waiting for debug container %q: %v", containerName, k8serrors.FromObject(event.Object))
			}
			if event.Type == watch.Modified || event.Type == watch.Added {
				pod, ok := event.Object.(*corev1.Pod)
				if !ok {
					continue
				}
				for _, cs := range pod.Status.EphemeralContainerStatuses {
					if cs.Name != containerName {
						continue
					}
					if cs.State.Running != nil {
						return nil
					}
					if cs.State.Terminated != nil {
						return fmt.Errorf("ephemeral container %q terminated: %s (exit code %d)",
							containerName, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
					}
					if w := cs.State.Waiting; w != nil {
						switch w.Reason {
						case "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
							"CrashLoopBackOff", "RunContainerError", "CreateContainerError",
							"CreateContainerConfigError":
							return containerStartFailureError(fmt.Sprintf("ephemeral container %q", containerName), w.Reason, w.Message)
						}
						// Print intermediate waiting status so the user can see progress
						if w.Reason != "" && w.Reason != lastReason {
							fmt.Printf("  Container status: %s", w.Reason)
							if w.Message != "" {
								fmt.Printf(" (%s)", w.Message)
							}
							fmt.Println()
							lastReason = w.Reason
						}
					}
				}
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for ephemeral container %q to start\n%s",
				containerName, describeContainerFailure(ctx, clientset, namespace, podName, containerName))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func waitForContainerRunning(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName, containerName, resourceVersion string) error {
	watcher, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   fmt.Sprintf("metadata.name=%s", podName),
		ResourceVersion: resourceVersion,
	})
	if err != nil {
		return fmt.Errorf("watching pod: %w", err)
	}
	defer watcher.Stop()

	var lastReason string
	timeout := time.After(2 * time.Minute)
	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch closed while waiting for debug container %q in copy pod %q to start", containerName, podName)
			}
			switch event.Type {
			case watch.Deleted:
				return fmt.Errorf("copy pod %q was deleted while waiting for debug container %q to start", podName, containerName)
			case watch.Error:
				return fmt.Errorf("watch error while waiting for debug container %q in copy pod %q: %v", containerName, podName, k8serrors.FromObject(event.Object))
			}
			if event.Type != watch.Modified && event.Type != watch.Added {
				continue
			}

			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}

			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name != containerName {
					continue
				}

				if cs.State.Running != nil {
					return nil
				}
				if cs.State.Terminated != nil {
					return fmt.Errorf("debug container %q in copy pod %q terminated: %s (exit code %d)",
						containerName, podName, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
				}
				if w := cs.State.Waiting; w != nil {
					switch w.Reason {
					case "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
						"CrashLoopBackOff", "RunContainerError", "CreateContainerError",
						"CreateContainerConfigError":
						return containerStartFailureError(fmt.Sprintf("debug container %q in copy pod %q", containerName, podName), w.Reason, w.Message)
					}
					if w.Reason != "" && w.Reason != lastReason {
						fmt.Printf("  Container status: %s", w.Reason)
						if w.Message != "" {
							fmt.Printf(" (%s)", w.Message)
						}
						fmt.Println()
						lastReason = w.Reason
					}
				}
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for debug container %q in copy pod %q to start", containerName, podName)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func containerStartFailureError(subject, reason, message string) error {
	msg := fmt.Sprintf("%s failed to start: %s", subject, reason)
	if message != "" {
		msg += ": " + message
	}
	if hint := kubernetesStartFailureHint(reason, message); hint != "" {
		msg += "\n\n" + hint
	}
	return fmt.Errorf("%s", msg)
}

func kubernetesStartFailureHint(reason, message string) string {
	text := strings.ToLower(reason + " " + message)
	switch {
	case strings.Contains(text, "openat etc/passwd") && strings.Contains(text, "path escapes from parent"):
		return "Hint: containerd cannot start images whose /etc/passwd is an absolute Nix store symlink. Rebuild or pull a debux image that materializes /etc/passwd and /etc/group as regular files, then retry (for example with --pull-policy=Always or --image <fixed-image>)."
	case strings.Contains(text, "imagepullbackoff") || strings.Contains(text, "errimagepull"):
		return "Hint: the node could not pull the debug image. Check the image name, registry access, imagePullSecrets, and --pull-policy."
	default:
		return ""
	}
}

// describeContainerFailure fetches the current pod status and recent events to
// help diagnose why an ephemeral container failed to start.
func describeContainerFailure(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName, containerName string) string {
	var details []string

	// Fetch latest pod status
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		details = append(details, fmt.Sprintf("  (could not fetch pod status: %v)", err))
	} else {
		found := false
		for _, cs := range pod.Status.EphemeralContainerStatuses {
			if cs.Name != containerName {
				continue
			}
			found = true
			if cs.State.Waiting != nil {
				details = append(details, fmt.Sprintf("  Container is waiting: %s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message))
			} else if cs.State.Terminated != nil {
				details = append(details, fmt.Sprintf("  Container terminated: %s (exit code %d)", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode))
			} else {
				details = append(details, "  Container state is unknown (no waiting/running/terminated status)")
			}
			break
		}
		if !found {
			details = append(details, "  Ephemeral container not found in pod status (it may not have been created)")
			details = append(details, "  Possible causes: RBAC denied ephemeral container creation, or the API server rejected it silently")
		}
	}

	details = append(details, podEventDetails(ctx, clientset, namespace, podName)...)

	if len(details) == 0 {
		return "  No additional diagnostic information available"
	}
	return strings.Join(details, "\n")
}

func waitForPodRunning(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName string) error {
	watcher, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", podName),
	})
	if err != nil {
		return fmt.Errorf("watching pod: %w", err)
	}
	defer watcher.Stop()

	var lastReason string
	timeout := time.After(2 * time.Minute)
	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch closed while waiting for pod %q to start\n%s",
					podName, describePodFailure(ctx, clientset, namespace, podName))
			}
			switch event.Type {
			case watch.Deleted:
				return fmt.Errorf("pod %q was deleted while waiting for it to start", podName)
			case watch.Error:
				return fmt.Errorf("watch error while waiting for pod %q: %v", podName, k8serrors.FromObject(event.Object))
			case watch.Modified, watch.Added:
			default:
				continue
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			switch pod.Status.Phase {
			case corev1.PodRunning:
				return nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return fmt.Errorf("pod %q ended with phase %s before becoming ready\n%s",
					podName, pod.Status.Phase, describePodFailure(ctx, clientset, namespace, podName))
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Terminated != nil {
					return fmt.Errorf("container %q in pod %q terminated: %s (exit code %d)",
						cs.Name, podName, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
				}
				if w := cs.State.Waiting; w != nil {
					switch w.Reason {
					case "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
						"CrashLoopBackOff", "RunContainerError", "CreateContainerError",
						"CreateContainerConfigError":
						return containerStartFailureError(fmt.Sprintf("container %q in pod %q", cs.Name, podName), w.Reason, w.Message)
					}
					if w.Reason != "" && w.Reason != lastReason {
						fmt.Printf("  Container status: %s", w.Reason)
						if w.Message != "" {
							fmt.Printf(" (%s)", w.Message)
						}
						fmt.Println()
						lastReason = w.Reason
					}
				}
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for pod %q to start\n%s",
				podName, describePodFailure(ctx, clientset, namespace, podName))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// describePodFailure summarizes the pod's container states and recent events
// to diagnose why a debug pod failed to start — printed before the pod is
// deleted so the evidence is not destroyed with it.
func describePodFailure(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName string) string {
	var details []string

	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		details = append(details, fmt.Sprintf("  (could not fetch pod status: %v)", err))
	} else {
		details = append(details, fmt.Sprintf("  Pod phase: %s", pod.Status.Phase))
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				details = append(details, fmt.Sprintf("  Container %s is waiting: %s: %s", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message))
			} else if cs.State.Terminated != nil {
				details = append(details, fmt.Sprintf("  Container %s terminated: %s (exit code %d)", cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode))
			}
		}
	}

	details = append(details, podEventDetails(ctx, clientset, namespace, podName)...)

	if len(details) == 0 {
		return "  No additional diagnostic information available"
	}
	return strings.Join(details, "\n")
}

// podEventDetails returns the last few Kubernetes events for a pod.
func podEventDetails(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName string) []string {
	events, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
	})
	if err != nil || len(events.Items) == 0 {
		return nil
	}
	details := []string{"  Recent pod events:"}
	start := 0
	if len(events.Items) > 5 {
		start = len(events.Items) - 5
	}
	for _, ev := range events.Items[start:] {
		details = append(details, fmt.Sprintf("    %s: %s: %s", ev.Type, ev.Reason, ev.Message))
	}
	return details
}

func attachToPod(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string, tty bool) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: containerName,
			Stdin:     true,
			Stdout:    true,
			Stderr:    !tty, // TTY merges stderr into stdout
			TTY:       tty,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Tty:    tty,
	}

	if tty {
		stdinFd, _ := term.GetFdInfo(os.Stdin)
		oldState, rawErr := term.SetRawTerminal(stdinFd)
		if rawErr == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
		tsq := newTerminalSizeQueue(stdinFd)
		defer tsq.Close()
		streamOpts.TerminalSizeQueue = tsq
		streamOpts.Stderr = &bytes.Buffer{}
	} else {
		streamOpts.Stderr = os.Stderr
	}

	return kubernetesExecError(exec.StreamWithContext(ctx, streamOpts))
}

type terminalSizeQueue struct {
	resizeChan   chan remotecommand.TerminalSize
	stopResizing chan struct{}
	done         chan struct{}
	stopOnce     sync.Once
}

func parsePositiveUint16Env(key string) (uint16, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 10, 16)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint16(n), true
}

func terminalSizeFromFd(fd uintptr) (remotecommand.TerminalSize, bool) {
	ws, err := term.GetWinsize(fd)
	if err != nil || ws == nil || ws.Width == 0 || ws.Height == 0 {
		return remotecommand.TerminalSize{}, false
	}
	return remotecommand.TerminalSize{Width: ws.Width, Height: ws.Height}, true
}

func detectTerminalSize(fd uintptr) remotecommand.TerminalSize {
	if size, ok := terminalSizeFromFd(fd); ok {
		return size
	}

	stdoutFd, stdoutIsTerminal := term.GetFdInfo(os.Stdout)
	if stdoutIsTerminal {
		if size, ok := terminalSizeFromFd(stdoutFd); ok {
			return size
		}
	}

	if cols, okCols := parsePositiveUint16Env("COLUMNS"); okCols {
		if lines, okLines := parsePositiveUint16Env("LINES"); okLines {
			return remotecommand.TerminalSize{Width: cols, Height: lines}
		}
	}

	return remotecommand.TerminalSize{Width: 80, Height: 24}
}

func newTerminalSizeQueue(fd uintptr) *terminalSizeQueue {
	tsq := &terminalSizeQueue{
		resizeChan:   make(chan remotecommand.TerminalSize, 1),
		stopResizing: make(chan struct{}),
		done:         make(chan struct{}),
	}

	tsq.resizeChan <- detectTerminalSize(fd)

	go tsq.monitorSize(fd)

	return tsq
}

func (t *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-t.resizeChan
	if !ok {
		return nil
	}
	return &size
}

func (t *terminalSizeQueue) monitorSize(fd uintptr) {
	sigCh, stopSig := watchSIGWINCH()
	defer func() {
		stopSig()
		close(t.resizeChan)
		close(t.done)
	}()

	for {
		select {
		case <-sigCh:
			size := detectTerminalSize(fd)
			select {
			case t.resizeChan <- size:
			case <-t.stopResizing:
				return
			default:
				// Queue full during a resize burst: drop the stale size so
				// the final window geometry always reaches the server.
				select {
				case <-t.resizeChan:
				default:
				}
				select {
				case t.resizeChan <- size:
				case <-t.stopResizing:
					return
				default:
				}
			}
		case <-t.stopResizing:
			return
		}
	}
}

func (t *terminalSizeQueue) Close() {
	t.stopOnce.Do(func() {
		close(t.stopResizing)
		<-t.done
	})
}
