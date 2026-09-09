package runtime

import (
	"context"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func cleanupDockerContainer(ctx context.Context, cli *client.Client, id string) {
	cleanupResource(ctx, id, func(cleanupCtx context.Context) error {
		_, err := cli.ContainerRemove(cleanupCtx, id, client.ContainerRemoveOptions{Force: true})
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return err
	})
}

func cleanupKubernetesPod(ctx context.Context, cs kubernetes.Interface, pod *corev1.Pod) {
	cleanupResource(ctx, pod.Namespace+"/"+pod.Name, func(cleanupCtx context.Context) error {
		uid := pod.UID
		err := cs.CoreV1().Pods(pod.Namespace).Delete(cleanupCtx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	})
}

const cleanupTimeout = 10 * time.Second

// Cleanup must survive Ctrl-C, but never hang exit indefinitely when a daemon
// or cluster disappears. Do not silently lose orphaned-resource diagnostics.
func cleanupResource(ctx context.Context, name string, remove func(context.Context) error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := remove(cleanupCtx); err != nil {
		statusf("Warning: could not clean up %s: %v; remove it manually when the runtime is reachable\n", name, err)
	}
}
