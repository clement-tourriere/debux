package runtime

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
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
	"k8s.io/apimachinery/pkg/util/httpstream"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
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
	HasDebuxSession bool // true if pod has a running debux ephemeral container
}

// KubernetesList returns running pods, optionally filtered by namespace.
func KubernetesList(ctx context.Context, kubeconfig string, kubeContext string, namespace string) ([]PodInfo, error) {
	return kubernetesListPods(ctx, kubeconfig, kubeContext, namespace, "")
}

const (
	kubernetesPodListLimit       int64 = 50
	kubernetesPodListResultLimit int   = 500
)

var podsResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

func kubernetesListPods(ctx context.Context, kubeconfig, kubeContext, namespace, query string) ([]PodInfo, error) {
	config, _, err := getK8sClient(kubeconfig, kubeContext)
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

	return result, nil
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

	containerName := findRunningDebuxContainer(pod)
	if containerName == "" {
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

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	killed := 0
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.EphemeralContainerStatuses {
			if strings.HasPrefix(cs.Name, "debux-") && cs.State.Running != nil {
				if err := killInContainer(ctx, config, clientset, namespace, pod.Name, cs.Name); err != nil {
					fmt.Printf("Warning: failed to kill %s on %s/%s: %v\n", cs.Name, namespace, pod.Name, err)
					continue
				}
				fmt.Printf("Killed %s on %s/%s\n", cs.Name, namespace, pod.Name)
				killed++
			}
		}
	}

	if killed == 0 {
		fmt.Println("No running debux sessions found")
	} else {
		fmt.Printf("Killed %d debug session(s)\n", killed)
	}
	return nil
}

// killInContainer execs "kill 1" inside a container to terminate PID 1.
func killInContainer(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   []string{"kill", "1"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	})
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

	// Get the target pod
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting pod %s/%s: %w", namespace, podName, err)
	}

	// Determine the target container name. Prefer a running container so PID/root
	// namespace targeting works even when the first spec container is not ready.
	targetContainer, err := selectKubernetesTargetContainer(pod, target.Container)
	if err != nil {
		return err
	}

	if opts.Copy {
		return kubernetesExecWithPodCopy(ctx, config, clientset, namespace, pod, targetContainer, opts)
	}

	// Try to reuse an existing running debux container for the same target container.
	if !opts.Fresh {
		if existing := findRunningDebuxContainerForTarget(pod, targetContainer); existing != "" {
			fmt.Printf("Reusing debug container %q\n", existing)
			fmt.Printf("Debugging %s/%s (container: %s)\n", namespace, podName, existing)
			return execInPod(ctx, config, clientset, namespace, podName, existing)
		}
	}

	// Create a new ephemeral container in daemon mode.
	// Use nanoseconds to avoid name collisions when retrying after a fast failure;
	// ephemeral containers cannot be removed from the pod spec.
	debugContainerName := fmt.Sprintf("debux-%d", time.Now().UnixNano())
	debuxTarget := kubernetesDebugTargetLabel(opts.KubeContext, namespace, podName, targetContainer)

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
				{Name: "DEBUX_TARGET_ROOT", Value: "/proc/1/root"},
				{Name: "DEBUX_DAEMON", Value: "1"},
				{Name: "HOME", Value: "/root"},
				{Name: "ZDOTDIR", Value: "/tmp"},
			},
		},
		TargetContainerName: targetContainer,
	}

	// Share target container's volume mounts (skip ones with SubPath, not allowed on ephemeral containers)
	if opts.ShareVolumes {
		for _, c := range pod.Spec.Containers {
			if c.Name == targetContainer {
				for _, vm := range c.VolumeMounts {
					if vm.SubPath == "" && vm.SubPathExpr == "" && !isReservedDebugMountPath(vm.MountPath) {
						ephemeralContainer.VolumeMounts = append(ephemeralContainer.VolumeMounts, vm)
					}
				}
				break
			}
		}
	}

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
	if sc != nil {
		ephemeralContainer.SecurityContext = sc
	}

	// Add the ephemeral container to the pod spec and update via the
	// ephemeralcontainers subresource (PUT), matching kubectl debug behavior.
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, ephemeralContainer)
	patchedPod, err := clientset.CoreV1().Pods(namespace).UpdateEphemeralContainers(ctx, podName, pod, metav1.UpdateOptions{})
	if err != nil {
		if k8serrors.IsForbidden(err) {
			return fmt.Errorf("updating ephemeral containers: %w\nHint: your RBAC may not allow pods/ephemeralcontainers. Retry with --copy to use pod-copy debug mode", err)
		}
		return fmt.Errorf("updating ephemeral containers: %w", err)
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
		return fmt.Errorf("ephemeral container %q was not created — the API server accepted the patch but the container is missing from the pod spec.\n"+
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
		return err
	}

	fmt.Printf("Debugging %s/%s (container: %s)\n", namespace, podName, debugContainerName)

	// Exec into the daemon container to start an interactive shell
	return execInPod(ctx, config, clientset, namespace, podName, debugContainerName)
}

func kubernetesExecWithPodCopy(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace string, sourcePod *corev1.Pod, targetContainer string, opts DebugOpts) error {
	spec := *sourcePod.Spec.DeepCopy()

	// The copied pod should be scheduler-managed, not pinned to the original node.
	spec.NodeName = ""
	spec.EphemeralContainers = nil

	// Match kubectl debug --copy-to --share-processes behavior.
	shareProcesses := true
	spec.ShareProcessNamespace = &shareProcesses

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
			{Name: "DEBUX_TARGET", Value: kubernetesDebugTargetLabel(opts.KubeContext, namespace, sourcePod.Name, targetContainer)},
			{Name: "DEBUX_DAEMON", Value: "1"},
			{Name: "HOME", Value: "/root"},
			{Name: "ZDOTDIR", Value: "/tmp"},
		},
	}

	if opts.ShareVolumes {
		for _, c := range sourcePod.Spec.Containers {
			if c.Name == targetContainer {
				for _, vm := range c.VolumeMounts {
					if !isReservedDebugMountPath(vm.MountPath) {
						debugContainer.VolumeMounts = append(debugContainer.VolumeMounts, vm)
					}
				}
				break
			}
		}
	}

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
	if sc != nil {
		debugContainer.SecurityContext = sc
	}

	spec.Containers = append(spec.Containers, debugContainer)

	copyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "debux-copy-",
			Namespace:    namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "debux",
				"debux.clement-tourriere/mode": "copy",
			},
			Annotations: map[string]string{
				"debux.clement-tourriere/source-pod": sourcePod.Name,
			},
		},
		Spec: spec,
	}

	created, err := clientset.CoreV1().Pods(namespace).Create(ctx, copyPod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating debug copy pod: %w", err)
	}

	defer func() {
		fmt.Printf("Deleting debug copy pod %s...\n", created.Name)
		_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	}()

	fmt.Printf("Waiting for debug copy pod %q to start...\n", created.Name)
	if err := waitForContainerRunning(ctx, clientset, namespace, created.Name, debugContainerName, created.ResourceVersion); err != nil {
		return err
	}

	runningCopyPod, err := clientset.CoreV1().Pods(namespace).Get(ctx, created.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting debug copy pod %s/%s: %w", namespace, created.Name, err)
	}

	targetContainerID := findContainerID(runningCopyPod, targetContainer)
	if targetContainerID == "" {
		fmt.Printf("Warning: could not resolve container ID for target container %q in copy pod %s/%s; filesystem/env integration may be limited\n", targetContainer, namespace, created.Name)
	}

	fmt.Printf("Debugging copy %s/%s (container: %s, source: %s)\n", namespace, created.Name, debugContainerName, sourcePod.Name)

	return execInPodWithCommand(ctx, config, clientset, namespace, created.Name, debugContainerName, copyPodShellCommand(targetContainerID))
}

func kubernetesDebugTargetLabel(kubeContext, namespace, podName, containerName string) string {
	label := fmt.Sprintf("%s/%s/%s", namespace, podName, containerName)
	if kubeContext != "" {
		return fmt.Sprintf("%s:%s", kubeContext, label)
	}
	return label
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
	case "/nix", "/nix/store", "/nix/var":
		return true
	default:
		return false
	}
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

func podHasContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
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
	return false
}

func podContainerNames(pod *corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
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

func copyPodShellCommand(targetContainerID string) []string {
	cmd := "ZDOTDIR=/tmp exec zsh"
	if targetContainerID != "" {
		shortID := targetContainerID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		cmd = fmt.Sprintf("target_cid=%q; target_cid_short=%q; target_pid=''; if [ -n \"$target_cid\" ]; then for p in /proc/[0-9]*; do [ -r \"$p/cgroup\" ] || continue; if grep -q \"$target_cid\" \"$p/cgroup\" 2>/dev/null || grep -q \"$target_cid_short\" \"$p/cgroup\" 2>/dev/null; then target_pid=\"${p##*/}\"; break; fi; done; fi; if [ -n \"$target_pid\" ] && [ -d \"/proc/$target_pid/root\" ]; then export DEBUX_TARGET_ROOT=\"/proc/$target_pid/root\"; export DEBUX_TARGET_ENVIRON=\"/proc/$target_pid/environ\"; export DEBUX_TARGET_CWD_LINK=\"/proc/$target_pid/cwd\"; fi; ZDOTDIR=/tmp exec zsh", targetContainerID, shortID)
	}
	return []string{"sh", "-c", cmd}
}

// findRunningDebuxContainer looks for an existing running ephemeral container
// with the "debux-" prefix on the given pod. Returns its name, or "" if none found.
func findRunningDebuxContainer(pod *corev1.Pod) string {
	for _, cs := range pod.Status.EphemeralContainerStatuses {
		if strings.HasPrefix(cs.Name, "debux-") && cs.State.Running != nil {
			return cs.Name
		}
	}
	return ""
}

// findRunningDebuxContainerForTarget returns a running debux ephemeral container
// that targets the same Kubernetes container. Reusing a session created for a
// different target container would put the shell in the wrong PID/root namespace.
func findRunningDebuxContainerForTarget(pod *corev1.Pod, targetContainer string) string {
	if targetContainer == "" {
		return findRunningDebuxContainer(pod)
	}

	running := make(map[string]struct{})
	for _, cs := range pod.Status.EphemeralContainerStatuses {
		if strings.HasPrefix(cs.Name, "debux-") && cs.State.Running != nil {
			running[cs.Name] = struct{}{}
		}
	}

	for _, ec := range pod.Spec.EphemeralContainers {
		if _, ok := running[ec.Name]; ok && ec.TargetContainerName == targetContainer {
			return ec.Name
		}
	}

	return ""
}

// execInPod starts a new interactive zsh session inside a running container
// using the /exec subresource (unlike attachToPod which uses /attach).
func execInPod(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string) error {
	return execInPodWithCommand(ctx, config, clientset, namespace, podName, containerName, []string{"zsh"})
}

func execInPodWithCommand(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string, command []string) error {
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
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	// Set terminal to raw mode
	stdinFd, isTerminal := term.GetFdInfo(os.Stdin)
	if isTerminal {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: &bytes.Buffer{}, // TTY merges stderr into stdout
		Tty:    true,
	}

	if isTerminal {
		tsq := newTerminalSizeQueue(stdinFd)
		defer tsq.Close()
		streamOpts.TerminalSizeQueue = tsq
	}

	return exec.StreamWithContext(ctx, streamOpts)
}

// KubernetesPod creates a standalone debug pod.
func KubernetesPod(ctx context.Context, opts PodOpts) error {
	config, clientset, err := getK8sClient(opts.Kubeconfig, opts.KubeContext)
	if err != nil {
		return err
	}

	if opts.Namespace == "default" {
		opts.Namespace = resolveNamespace(opts.Kubeconfig, opts.KubeContext)
	}

	podName := fmt.Sprintf("debux-%d", time.Now().Unix())

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
					Command:         []string{"/bin/sh", "-c", "exec zsh"},
					Stdin:           true,
					TTY:             true,
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			HostNetwork:   opts.HostNetwork,
		},
	}

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

	return attachToPod(ctx, config, clientset, opts.Namespace, podName, "debug")
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
			if event.Type == watch.Modified {
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

	// Fetch recent events for the pod
	events, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
	})
	if err == nil && len(events.Items) > 0 {
		details = append(details, "  Recent pod events:")
		// Show last 5 events
		start := 0
		if len(events.Items) > 5 {
			start = len(events.Items) - 5
		}
		for _, ev := range events.Items[start:] {
			details = append(details, fmt.Sprintf("    %s: %s: %s", ev.Type, ev.Reason, ev.Message))
		}
	}

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

	timeout := time.After(2 * time.Minute)
	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch closed while waiting for pod %q to start", podName)
			}
			if event.Type == watch.Modified || event.Type == watch.Added {
				pod, ok := event.Object.(*corev1.Pod)
				if !ok {
					continue
				}
				if pod.Status.Phase == corev1.PodRunning {
					return nil
				}
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for pod %q to start", podName)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func attachToPod(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: containerName,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	// Set terminal to raw mode
	stdinFd, isTerminal := term.GetFdInfo(os.Stdin)
	if isTerminal {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: &bytes.Buffer{}, // TTY merges stderr into stdout
		Tty:    true,
	}

	if isTerminal {
		tsq := newTerminalSizeQueue(stdinFd)
		defer tsq.Close()
		streamOpts.TerminalSizeQueue = tsq
	}

	return exec.StreamWithContext(ctx, streamOpts)
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
