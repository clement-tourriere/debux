package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/clement-tourriere/debux/internal/entrypoint"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

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
	extraEnv, err := debugExtraEnv(opts.Env, opts.Tools)
	if err != nil {
		return "", "", err
	}
	ephemeralContainer.Env = append(ephemeralContainer.Env, kubernetesEnvVars(extraEnv)...)

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
	// disruption (consolidation, drift) while the copy pod runs. debux uses
	// the boolean "true" form; the TTL bounds the protection indirectly
	// because Karpenter ignores terminal pods once activeDeadlineSeconds
	// expires them. (Karpenter also accepts a Go-duration value, but the
	// deadline already bounds the window, so the boolean form is simpler.)
	karpenterDoNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"
	// karpenterDoNotEvictAnnotation and karpenterDoNotConsolidateAnnotation
	// are the pre-v1beta1 equivalents of do-not-disrupt. Karpenter dropped
	// them in v1.0, so they only take effect on v0.x clusters; v1.0+ silently
	// ignores them. Kept so copy pods stay protected on older clusters.
	karpenterDoNotEvictAnnotation       = "karpenter.sh/do-not-evict"
	karpenterDoNotConsolidateAnnotation = "karpenter.sh/do-not-consolidate"
	// clusterAutoscalerSafeToEvictAnnotation is the cluster-autoscaler
	// equivalent: "false" blocks scale-down of the node while the pod runs.
	clusterAutoscalerSafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

// buildKubernetesCopyPod renders the copy-mode debug pod for a source pod. It
// returns the pod to create and the name of the debug container inside it.

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
