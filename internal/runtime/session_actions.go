package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AttachDebugSession attaches to the selected debug process without creating,
// replacing, or re-resolving a session from its application target.
func AttachDebugSession(ctx context.Context, session DebugSessionInfo, kubeconfig string) error {
	return actOnDebugSession(ctx, session, kubeconfig, false)
}

// KillDebugSession stops only the selected session, not the first session that
// happens to share its target. Immutable IDs protect against name recycling.
func KillDebugSession(ctx context.Context, session DebugSessionInfo, kubeconfig string) error {
	return actOnDebugSession(ctx, session, kubeconfig, true)
}

func actOnDebugSession(ctx context.Context, session DebugSessionInfo, kubeconfig string, kill bool) error {
	if session.ID == "" || session.DebugName == "" {
		return fmt.Errorf("session has no immutable identity; refresh the session list")
	}
	switch session.Runtime {
	case "docker":
		target, err := ParseTarget(session.Target)
		if err != nil {
			return err
		}
		cli, err := dockerClientForTarget(target)
		if err != nil {
			return err
		}
		defer func() { _ = cli.Close() }()
		info, err := inspectDockerContainer(ctx, cli, session.ID)
		if err != nil {
			return fmt.Errorf("selected session no longer exists: %w", err)
		}
		if err := validateDockerSession(info, session); err != nil {
			return err
		}
		if kill {
			_, err = cli.ContainerRemove(ctx, session.ID, client.ContainerRemoveOptions{Force: true})
			return err
		}
		return execInContainer(ctx, cli, session.ID, nil)
	case "kubernetes":
		config, cs, err := getK8sClient(kubeconfig, session.Context)
		if err != nil {
			return err
		}
		pod, err := cs.CoreV1().Pods(session.Namespace).Get(ctx, session.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := validateKubernetesSession(pod, session); err != nil {
			return err
		}
		if kill {
			if session.Kind == DebugSessionKindKubernetesCopyPod {
				uid := pod.UID
				return cs.CoreV1().Pods(session.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
			}
			return killInContainer(ctx, config, cs, session.Namespace, pod.Name, session.DebugName)
		}
		if session.Kind == DebugSessionKindKubernetesCopyPod {
			return kubernetesReattachToCopyPod(ctx, config, cs, session.Namespace, pod, session.TargetContainer, session.Context, DebugOpts{})
		}
		if err := validateKubernetesTargetNamespace(pod); err != nil {
			return err
		}
		label := kubernetesDebugTargetLabel(session.Context, session.Namespace, pod.Name, session.TargetContainer)
		return execInPodWithMetadata(ctx, config, cs, session.Namespace, pod.Name, session.DebugName, label, session.Context, nil)
	default:
		return fmt.Errorf("unsupported session runtime %q", session.Runtime)
	}
}

func validateDockerSession(info container.InspectResponse, session DebugSessionInfo) error {
	if info.ID != session.ID || strings.TrimPrefix(info.Name, "/") != session.DebugName || info.Config == nil || info.State == nil || !info.State.Running {
		return fmt.Errorf("selected Docker session changed or stopped; refresh the session list")
	}
	if !isDebuxDockerSidecar(container.Summary{Names: []string{info.Name}, Labels: info.Config.Labels, Image: info.Config.Image}) {
		return fmt.Errorf("selected container is not a debux sidecar")
	}
	return nil
}

func validateKubernetesSession(pod *corev1.Pod, session DebugSessionInfo) error {
	if string(pod.UID) != session.ID || pod.DeletionTimestamp != nil {
		return fmt.Errorf("selected pod was replaced or is terminating; refresh the session list")
	}
	for _, current := range kubernetesSessionsForPod(pod, session.Context) {
		if current.Kind == session.Kind && current.DebugName == session.DebugName && current.TargetContainer == session.TargetContainer {
			return nil
		}
	}
	return fmt.Errorf("selected debug container %q is no longer running; refresh the session list", session.DebugName)
}
