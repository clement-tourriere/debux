package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/moby/term"
)

// PodInfo holds metadata about a running Kubernetes pod.
type PodInfo struct {
	Name       string
	Namespace  string
	Status     string
	Containers []string
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
		result = append(result, PodInfo{
			Name:       pod.Name,
			Namespace:  pod.Namespace,
			Status:     string(pod.Status.Phase),
			Containers: containers,
		})
	}
	return result, nil
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
			Command:         []string{"/entrypoint.sh"},
			Stdin:           true,
			TTY:             true,
			Env: []corev1.EnvVar{
				{Name: "DEBUX_TARGET", Value: target.Name},
				{Name: "DEBUX_DAEMON", Value: "1"},
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

	if opts.Privileged {
		t := true
		ephemeralContainer.SecurityContext = &corev1.SecurityContext{
			Privileged: &t,
		}
	}

	// Patch the pod to add the ephemeral container
	patch := map[string]any{
		"spec": map[string]any{
			"ephemeralContainers": append(pod.Spec.EphemeralContainers, ephemeralContainer),
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshaling patch: %w", err)
	}

	_, err = clientset.CoreV1().Pods(namespace).Patch(ctx, podName,
		types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}, "ephemeralcontainers")
	if err != nil {
		return fmt.Errorf("patching pod with ephemeral container: %w", err)
	}

	fmt.Printf("Waiting for debug container %q to start...\n", debugContainerName)

	// Wait for the ephemeral container to be running
	if err := waitForEphemeralContainer(ctx, clientset, namespace, podName, debugContainerName); err != nil {
		return err
	}

	fmt.Printf("Debugging %s/%s (container: %s)\n", namespace, podName, debugContainerName)

	// Exec into the daemon container to start an interactive shell
	return execInPod(ctx, config, clientset, namespace, podName, debugContainerName)
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
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   []string{"zsh"},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, req.URL())
	if err != nil {
		return fmt.Errorf("creating SPDY executor: %w", err)
	}

	// Set terminal to raw mode
	stdinFd, isTerminal := term.GetFdInfo(os.Stdin)
	if isTerminal {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
			}()
		}
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: &bytes.Buffer{}, // TTY merges stderr into stdout
	}

	if isTerminal {
		streamOpts.TerminalSizeQueue = newTerminalSizeQueue(stdinFd)
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

	if opts.Privileged {
		t := true
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
			Privileged: &t,
		}
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

func waitForEphemeralContainer(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName, containerName string) error {
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
					}
				}
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for ephemeral container %q to start", containerName)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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

	exec, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, req.URL())
	if err != nil {
		return fmt.Errorf("creating SPDY executor: %w", err)
	}

	// Set terminal to raw mode
	stdinFd, isTerminal := term.GetFdInfo(os.Stdin)
	if isTerminal {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
			}()
		}
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: &bytes.Buffer{}, // TTY merges stderr into stdout
	}

	if isTerminal {
		streamOpts.TerminalSizeQueue = newTerminalSizeQueue(stdinFd)
	}

	return exec.StreamWithContext(ctx, streamOpts)
}

// terminalSizeQueue implements remotecommand.TerminalSizeQueue
type terminalSizeQueue struct {
	fd   uintptr
	done chan struct{}
}

func newTerminalSizeQueue(fd uintptr) *terminalSizeQueue {
	return &terminalSizeQueue{fd: fd, done: make(chan struct{})}
}

func (t *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	size, err := term.GetWinsize(t.fd)
	if err != nil || size == nil {
		return nil
	}
	return &remotecommand.TerminalSize{
		Width:  size.Width,
		Height: size.Height,
	}
}
