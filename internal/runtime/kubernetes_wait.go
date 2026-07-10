package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// podStartTimeout bounds each wait for a debug container/pod to start. The
// window includes image pull time, so it is sized for a cold node pulling
// the full toolbox image, not just for a warm start.
const podStartTimeout = 5 * time.Minute

// maxPodWatchRestarts caps how many benignly-closed watches (apiserver
// restart, idle LB timeout) are re-established before giving up.
const maxPodWatchRestarts = 5

// watchPod opens a pod watch scoped by name at the requested resource version.
func watchPod(ctx context.Context, clientset kubernetes.Interface, namespace, podName, resourceVersion string) (watch.Interface, error) {
	return clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   fmt.Sprintf("metadata.name=%s", podName),
		ResourceVersion: resourceVersion,
	})
}

// watchPodFromCurrentState closes the gap after a watch disconnect: fetch a
// current snapshot, establish the replacement watch at that snapshot's
// resource version, then let the caller evaluate the snapshot. Opening the
// watch before evaluation ensures a transition after the GET is queued rather
// than missed, without relying on server-specific synthetic Added events.
func watchPodFromCurrentState(ctx context.Context, clientset kubernetes.Interface, namespace, podName string) (*corev1.Pod, watch.Interface, error) {
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	watcher, err := watchPod(ctx, clientset, namespace, podName, pod.ResourceVersion)
	if err != nil {
		return nil, nil, err
	}
	return pod, watcher, nil
}

func waitForContainerRunning(ctx context.Context, clientset kubernetes.Interface, namespace, podName, containerName string) error {
	var lastReason string
	current, watcher, err := watchPodFromCurrentState(ctx, clientset, namespace, podName)
	if err != nil {
		return fmt.Errorf("watching pod from current state: %w", err)
	}
	defer func() { watcher.Stop() }()
	if done, stateErr := copyContainerStartState(current, podName, containerName, &lastReason); done {
		return stateErr
	}

	restarts := 0
	timeout := time.After(podStartTimeout)
	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if restarts >= maxPodWatchRestarts {
					return fmt.Errorf("watch closed repeatedly while waiting for debug container %q in copy pod %q to start", containerName, podName)
				}
				restarts++
				watcher.Stop()
				current, replacement, watchErr := watchPodFromCurrentState(ctx, clientset, namespace, podName)
				if watchErr != nil {
					return fmt.Errorf("re-watching pod from current state: %w", watchErr)
				}
				watcher = replacement
				if done, stateErr := copyContainerStartState(current, podName, containerName, &lastReason); done {
					return stateErr
				}
				continue
			}
			switch event.Type {
			case watch.Deleted:
				return fmt.Errorf("copy pod %q was deleted while waiting for debug container %q to start", podName, containerName)
			case watch.Error:
				return fmt.Errorf("watch error while waiting for debug container %q in copy pod %q: %w", containerName, podName, k8serrors.FromObject(event.Object))
			}
			if event.Type != watch.Modified && event.Type != watch.Added {
				continue
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if done, stateErr := copyContainerStartState(pod, podName, containerName, &lastReason); done {
				return stateErr
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for debug container %q in copy pod %q to start", containerName, podName)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func copyContainerStartState(pod *corev1.Pod, podName, containerName string, lastReason *string) (bool, error) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if cs.State.Running != nil {
			return true, nil
		}
		if cs.State.Terminated != nil {
			return true, fmt.Errorf("debug container %q in copy pod %q terminated: %s (exit code %d)",
				containerName, podName, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		}
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
				"CrashLoopBackOff", "RunContainerError", "CreateContainerError",
				"CreateContainerConfigError":
				return true, containerStartFailureError(fmt.Sprintf("debug container %q in copy pod %q", containerName, podName), w.Reason, w.Message)
			}
			if w.Reason != "" && w.Reason != *lastReason {
				statusf("  Container status: %s", w.Reason)
				if w.Message != "" {
					statusf(" (%s)", w.Message)
				}
				statusln()
				*lastReason = w.Reason
			}
		}
	}
	return false, nil
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
func describeContainerFailure(ctx context.Context, clientset kubernetes.Interface, namespace, podName, containerName string) string {
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

func waitForPodRunning(ctx context.Context, clientset kubernetes.Interface, namespace, podName string) error {
	var lastReason string
	current, watcher, err := watchPodFromCurrentState(ctx, clientset, namespace, podName)
	if err != nil {
		return fmt.Errorf("watching pod from current state: %w", err)
	}
	defer func() { watcher.Stop() }()
	if done, stateErr := podStartState(ctx, clientset, namespace, current, &lastReason); done {
		return stateErr
	}

	restarts := 0
	timeout := time.After(podStartTimeout)
	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if restarts >= maxPodWatchRestarts {
					return fmt.Errorf("watch closed repeatedly while waiting for pod %q to start\n%s",
						podName, describePodFailure(ctx, clientset, namespace, podName))
				}
				restarts++
				watcher.Stop()
				current, replacement, watchErr := watchPodFromCurrentState(ctx, clientset, namespace, podName)
				if watchErr != nil {
					return fmt.Errorf("re-watching pod from current state: %w", watchErr)
				}
				watcher = replacement
				if done, stateErr := podStartState(ctx, clientset, namespace, current, &lastReason); done {
					return stateErr
				}
				continue
			}
			switch event.Type {
			case watch.Deleted:
				return fmt.Errorf("pod %q was deleted while waiting for it to start", podName)
			case watch.Error:
				return fmt.Errorf("watch error while waiting for pod %q: %w", podName, k8serrors.FromObject(event.Object))
			case watch.Modified, watch.Added:
			default:
				continue
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if done, stateErr := podStartState(ctx, clientset, namespace, pod, &lastReason); done {
				return stateErr
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for pod %q to start\n%s",
				podName, describePodFailure(ctx, clientset, namespace, podName))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func podStartState(ctx context.Context, clientset kubernetes.Interface, namespace string, pod *corev1.Pod, lastReason *string) (bool, error) {
	podName := pod.Name
	switch pod.Status.Phase {
	case corev1.PodRunning:
		return true, nil
	case corev1.PodFailed, corev1.PodSucceeded:
		return true, fmt.Errorf("pod %q ended with phase %s before becoming ready\n%s",
			podName, pod.Status.Phase, describePodFailure(ctx, clientset, namespace, podName))
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return true, fmt.Errorf("container %q in pod %q terminated: %s (exit code %d)",
				cs.Name, podName, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		}
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
				"CrashLoopBackOff", "RunContainerError", "CreateContainerError",
				"CreateContainerConfigError":
				return true, containerStartFailureError(fmt.Sprintf("container %q in pod %q", cs.Name, podName), w.Reason, w.Message)
			}
			if w.Reason != "" && w.Reason != *lastReason {
				statusf("  Container status: %s", w.Reason)
				if w.Message != "" {
					statusf(" (%s)", w.Message)
				}
				statusln()
				*lastReason = w.Reason
			}
		}
	}
	return false, nil
}

// describePodFailure summarizes the pod's container states and recent events
// to diagnose why a debug pod failed to start — printed before the pod is
// deleted so the evidence is not destroyed with it.
func describePodFailure(ctx context.Context, clientset kubernetes.Interface, namespace, podName string) string {
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
func podEventDetails(ctx context.Context, clientset kubernetes.Interface, namespace, podName string) []string {
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
