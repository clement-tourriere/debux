package runtime

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/entrypoint"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

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
	extraEnv, err := debugExtraEnv(opts.Env, opts.Tools)
	if err != nil {
		return nil, "", err
	}
	debugContainer.Env = append(debugContainer.Env, kubernetesEnvVars(extraEnv)...)

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
	scopeURI := kubernetesScopeURI(kubeContext, namespace)
	printTerminalStatusLine("Keeping debug copy pod %s/%s", namespace, podName)
	if ttl > 0 {
		printTerminalStatusLine("  Expires:  %s after pod start, then the kubelet stops its containers", ttl)
	} else {
		printTerminalStatusLine("  Warning:  no TTL (--ttl=0); the pod runs until deleted")
	}
	printTerminalStatusLine("  List:     debux list %s", scopeURI)
	printTerminalStatusLine("  Reattach: debux attach %s", targetURI)
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

func kubernetesScopeURI(kubeContext, namespace string) string {
	if kubeContext != "" {
		return "k8s://@" + url.PathEscape(kubeContext) + "/" + url.PathEscape(namespace) + "/"
	}
	return "k8s://" + url.PathEscape(namespace) + "/"
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
