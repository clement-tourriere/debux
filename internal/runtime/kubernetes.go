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
	"k8s.io/apimachinery/pkg/util/httpstream"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
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
	Status          string
	Containers      []string
	HasDebuxSession bool // true if pod has a running debux ephemeral container
}

// KubernetesList returns running pods, optionally filtered by namespace.
func KubernetesList(ctx context.Context, kubeconfig string, namespace string) ([]PodInfo, error) {
	_, clientset, err := getK8sClient(kubeconfig)
	if err != nil {
		return nil, err
	}

	// Resolve namespace from kubeconfig context when using the default placeholder
	listNs := namespace
	if listNs == "default" {
		listNs = resolveNamespace(kubeconfig)
	}

	pods, err := clientset.CoreV1().Pods(listNs).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	var result []PodInfo
	for _, pod := range pods.Items {
		// Skip pods with no ready containers
		hasReady := false
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Ready {
				hasReady = true
				break
			}
		}
		if !hasReady {
			continue
		}

		var containers []string
		for _, c := range pod.Spec.Containers {
			containers = append(containers, c.Name)
		}

		hasSession := false
		for _, cs := range pod.Status.EphemeralContainerStatuses {
			if strings.HasPrefix(cs.Name, "debux-") && cs.State.Running != nil {
				hasSession = true
				break
			}
		}

		result = append(result, PodInfo{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			Status:          string(pod.Status.Phase),
			Containers:      containers,
			HasDebuxSession: hasSession,
		})
	}
	return result, nil
}

// KubernetesKill terminates the debux ephemeral container on a specific pod by
// killing PID 1 inside it. K8s ephemeral containers cannot be removed from the
// pod spec, but killing their init process terminates them.
func KubernetesKill(ctx context.Context, target *Target, kubeconfig string) error {
	config, clientset, err := getK8sClient(kubeconfig)
	if err != nil {
		return err
	}

	namespace := target.Namespace
	if namespace == "default" {
		namespace = resolveNamespace(kubeconfig)
	}

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
func KubernetesKillAll(ctx context.Context, kubeconfig string, namespace string) error {
	config, clientset, err := getK8sClient(kubeconfig)
	if err != nil {
		return err
	}

	if namespace == "default" {
		namespace = resolveNamespace(kubeconfig)
	}

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
	config, clientset, err := getK8sClient(opts.Kubeconfig)
	if err != nil {
		return err
	}

	namespace := target.Namespace
	if namespace == "default" {
		namespace = resolveNamespace(opts.Kubeconfig)
	}
	podName := target.Name

	// Get the target pod
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting pod %s/%s: %w", namespace, podName, err)
	}

	// Determine the target container name
	targetContainer := target.Container
	if targetContainer == "" && len(pod.Spec.Containers) > 0 {
		targetContainer = pod.Spec.Containers[0].Name
	}

	if opts.Copy {
		return kubernetesExecWithPodCopy(ctx, config, clientset, namespace, pod, targetContainer, opts)
	}

	// Try to reuse an existing running debux container
	if !opts.Fresh {
		if existing := findRunningDebuxContainer(pod); existing != "" {
			fmt.Printf("Reusing debug container %q\n", existing)
			fmt.Printf("Debugging %s/%s (container: %s)\n", namespace, podName, existing)
			return execInPod(ctx, config, clientset, namespace, podName, existing)
		}
	}

	// Create a new ephemeral container in daemon mode
	debugContainerName := fmt.Sprintf("debux-%d", time.Now().Unix())

	ephemeralContainer := corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:            debugContainerName,
			Image:           opts.Image,
			ImagePullPolicy: corev1.PullPolicy(opts.PullPolicy),
			Command:         []string{"/bin/sh", "-c", entrypoint.Script},
			Stdin:           true,
			TTY:             true,
			Env: []corev1.EnvVar{
				{Name: "DEBUX_TARGET", Value: target.Name},
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
					if vm.SubPath == "" && vm.SubPathExpr == "" {
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
			{Name: "DEBUX_TARGET", Value: fmt.Sprintf("%s/%s", namespace, sourcePod.Name)},
			{Name: "DEBUX_DAEMON", Value: "1"},
			{Name: "HOME", Value: "/root"},
			{Name: "ZDOTDIR", Value: "/tmp"},
		},
	}

	if opts.ShareVolumes {
		for _, c := range sourcePod.Spec.Containers {
			if c.Name == targetContainer {
				debugContainer.VolumeMounts = append(debugContainer.VolumeMounts, c.VolumeMounts...)
				break
			}
		}
	}

	sc, err := SecurityContextForProfile(opts.Profile)
	if err != nil {
		return err
	}
	if sc != nil {
		debugContainer.SecurityContext = sc
	}

	if opts.User != "" {
		debugContainer.Env = append(debugContainer.Env, corev1.EnvVar{Name: "DEBUX_USER", Value: opts.User})
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
	config, clientset, err := getK8sClient(opts.Kubeconfig)
	if err != nil {
		return err
	}

	if opts.Namespace == "default" {
		opts.Namespace = resolveNamespace(opts.Kubeconfig)
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
	if sc != nil {
		pod.Spec.Containers[0].SecurityContext = sc
	}

	if opts.User != "" {
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
			Name:  "DEBUX_USER",
			Value: opts.User,
		})
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
func resolveNamespace(kubeconfig string) string {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	ns, _, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).Namespace()
	if err != nil || ns == "" {
		return "default"
	}
	return ns
}

func getK8sClient(kubeconfig string) (*rest.Config, *kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
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
		case event := <-watcher.ResultChan():
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
							return fmt.Errorf("ephemeral container %q failed to start: %s: %s",
								containerName, w.Reason, w.Message)
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
		case event := <-watcher.ResultChan():
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
						return fmt.Errorf("debug container %q in copy pod %q failed to start: %s: %s",
							containerName, podName, w.Reason, w.Message)
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
		case event := <-watcher.ResultChan():
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
