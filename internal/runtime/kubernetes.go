package runtime

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/entrypoint"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
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
	extraEnv, err := debugExtraEnv(opts.Env, opts.Tools)
	if err != nil {
		return err
	}
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, kubernetesEnvVars(extraEnv)...)

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
