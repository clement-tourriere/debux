package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/entrypoint"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeInfo holds Kubernetes node metadata for interactive selection.
type NodeInfo struct {
	Name    string
	Status  string
	Roles   []string
	Version string
}

// KubernetesNodes lists cluster nodes for the node picker.
func KubernetesNodes(ctx context.Context, kubeconfig, kubeContext string) ([]NodeInfo, error) {
	_, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	result := make([]NodeInfo, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		info := NodeInfo{Name: node.Name, Version: node.Status.NodeInfo.KubeletVersion}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				if cond.Status == corev1.ConditionTrue {
					info.Status = "Ready"
				} else {
					info.Status = "NotReady"
				}
			}
		}
		for label := range node.Labels {
			if role, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok && role != "" {
				info.Roles = append(info.Roles, role)
			}
		}
		sort.Strings(info.Roles)
		result = append(result, info)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// KubernetesNode debugs a cluster node with a toolbox pod pinned to it: host
// PID/network/IPC namespaces, the node's root filesystem mounted at /host,
// and DEBUX_TARGET_ROOT pointing there — so the chroot fallback runs the
// node's own binaries (crictl, journalctl, systemctl) with their host paths.
func KubernetesNode(ctx context.Context, nodeName string, opts PodOpts) error {
	config, clientset, err := getK8sClient(opts.Kubeconfig, opts.KubeContext)
	if err != nil {
		return err
	}

	if opts.Namespace == "" {
		opts.Namespace = resolveNamespace(opts.Kubeconfig, opts.KubeContext)
	}

	if _, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("getting node %q: %w\nHint: run `debux node` without arguments to pick from the cluster's nodes", nodeName, err)
	}

	podName := fmt.Sprintf("debux-node-%d", time.Now().UnixNano())
	displayContext := kubernetesDisplayContext(opts.Kubeconfig, opts.KubeContext)
	displayTarget := "node/" + nodeName
	if displayContext != "" {
		displayTarget = displayContext + ":" + displayTarget
	}

	tty := stdioIsTTY()
	hostPathType := corev1.HostPathDirectory
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: opts.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "debux",
				"debux.clement-tourriere/mode": "node",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:    nodeName,
			HostNetwork: true,
			HostPID:     true,
			HostIPC:     true,
			// Node debugging must work on cordoned/tainted nodes — that is
			// often exactly why someone is debugging them.
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{{
				Name: "host-root",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/", Type: &hostPathType},
				},
			}},
			Containers: []corev1.Container{{
				Name:            "debug",
				Image:           opts.Image,
				ImagePullPolicy: corev1.PullPolicy(opts.PullPolicy),
				Command:         []string{"/bin/sh", "-c", entrypoint.Script},
				Stdin:           true,
				TTY:             tty,
				Env: []corev1.EnvVar{
					{Name: "DEBUX_TARGET", Value: displayTarget},
					{Name: "DEBUX_CONTEXT", Value: displayContext},
					// The hostPath mount is more robust than /proc/1/root,
					// which requires extra privileges to traverse.
					{Name: "DEBUX_TARGET_ROOT", Value: "/host"},
					{Name: "DEBUX_TARGET_ENVIRON", Value: "/proc/1/environ"},
					{Name: "DEBUX_TARGET_CWD_LINK", Value: "/proc/1/cwd"},
					{Name: "DEBUX_SECURITY_PROFILE", Value: opts.Profile},
					{Name: "DEBUX_DEBUG_USER", Value: opts.User},
					{Name: "HOME", Value: "/root"},
					{Name: "ZDOTDIR", Value: "/tmp"},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "host-root", MountPath: "/host"}},
			}},
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

	created, err := clientset.CoreV1().Pods(opts.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating node debug pod: %w", err)
	}

	if !opts.Keep {
		defer func() {
			fmt.Printf("Deleting node debug pod %s...\n", created.Name)
			_ = clientset.CoreV1().Pods(opts.Namespace).Delete(
				context.WithoutCancel(ctx), created.Name, metav1.DeleteOptions{})
		}()
	}

	fmt.Printf("Waiting for node debug pod %q on %s to start...\n", created.Name, nodeName)
	if err := waitForPodRunning(ctx, clientset, opts.Namespace, created.Name); err != nil {
		return err
	}

	fmt.Printf("Debugging node %s (pod: %s/%s, node root: /host)\n", nodeName, opts.Namespace, created.Name)
	return attachToPod(ctx, config, clientset, opts.Namespace, created.Name, "debug", tty)
}
