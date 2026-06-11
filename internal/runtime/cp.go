package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/moby/moby/client"
)

// DockerCopyFrom copies a path out of a Docker container (running or stopped)
// to the local filesystem.
func DockerCopyFrom(ctx context.Context, target *Target, srcPath, dstPath string) error {
	cli, name, err := dockerCpClient(ctx, target)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	copyResult, err := cli.CopyFromContainer(ctx, name, client.CopyFromContainerOptions{SourcePath: srcPath})
	if err != nil {
		return fmt.Errorf("copying %s from %s: %w", srcPath, name, err)
	}
	defer func() { _ = copyResult.Content.Close() }()

	if err := untarToLocal(copyResult.Content, dstPath); err != nil {
		return fmt.Errorf("extracting %s from %s: %w", srcPath, name, err)
	}
	fmt.Printf("Copied %s:%s to %s\n", name, srcPath, dstPath)
	return nil
}

// DockerCopyTo copies a local file or directory into a Docker container
// directory (which must exist).
func DockerCopyTo(ctx context.Context, target *Target, srcPath, dstPath string) error {
	cli, name, err := dockerCpClient(ctx, target)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	content, err := tarLocalPath(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = content.Close() }()

	if _, err := cli.CopyToContainer(ctx, name, client.CopyToContainerOptions{DestinationPath: dstPath, Content: content}); err != nil {
		return fmt.Errorf("copying %s to %s:%s (the destination directory must exist): %w", srcPath, name, dstPath, err)
	}
	fmt.Printf("Copied %s to %s:%s\n", srcPath, name, dstPath)
	return nil
}

func dockerCpClient(ctx context.Context, target *Target) (*client.Client, string, error) {
	cli, err := dockerClientForTarget(target)
	if err != nil {
		return nil, "", fmt.Errorf("connecting to Docker: %w", err)
	}
	name := target.Name
	if target.ComposeService != "" {
		name, err = resolveComposeContainer(ctx, cli, target.ComposeProject, target.ComposeService)
		if err != nil {
			_ = cli.Close()
			return nil, "", err
		}
	}
	return cli, name, nil
}

// KubernetesCopyFrom copies a path out of the target container's filesystem
// through the debux toolbox: tar runs in the toolbox, not the target, so this
// works on distroless images where kubectl cp fails.
func KubernetesCopyFrom(ctx context.Context, target *Target, opts DebugOpts, srcPath, dstPath string) error {
	session, err := kubernetesCpSession(ctx, target, opts)
	if err != nil {
		return err
	}

	srcDir := path.Dir(srcPath)
	srcBase := path.Base(srcPath)
	if srcBase == "/" || srcBase == "." || srcBase == ".." {
		return fmt.Errorf("cannot copy %q: pick a concrete file or directory", srcPath)
	}
	script := fmt.Sprintf("tar -cf - -C %s -- %s",
		shellQuote(kubernetesTargetRootPath(srcDir)), shellQuote(srcBase))

	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	go func() {
		err := execPodStream(ctx, session.config, session.clientset, session.namespace, session.podName, session.containerName,
			[]string{"/bin/sh", "-c", script}, nil, pw, &stderr)
		pw.CloseWithError(err)
	}()

	if err := untarToLocal(pr, dstPath); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("copying %s from %s/%s: %s: %w", srcPath, session.namespace, session.podName, msg, err)
		}
		return fmt.Errorf("copying %s from %s/%s: %w", srcPath, session.namespace, session.podName, err)
	}
	fmt.Printf("Copied %s/%s:%s to %s\n", session.namespace, session.podName, srcPath, dstPath)
	return nil
}

// KubernetesCopyTo copies a local file or directory into a directory of the
// target container's filesystem through the debux toolbox.
func KubernetesCopyTo(ctx context.Context, target *Target, opts DebugOpts, srcPath, dstPath string) error {
	session, err := kubernetesCpSession(ctx, target, opts)
	if err != nil {
		return err
	}

	content, err := tarLocalPath(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = content.Close() }()

	remoteDir := kubernetesTargetRootPath(dstPath)
	script := fmt.Sprintf("mkdir -p -- %s && tar -xf - -C %s", shellQuote(remoteDir), shellQuote(remoteDir))

	var stderr bytes.Buffer
	if err := execPodStream(ctx, session.config, session.clientset, session.namespace, session.podName, session.containerName,
		[]string{"/bin/sh", "-c", script}, content, io.Discard, &stderr); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("copying %s to %s/%s:%s: %s: %w", srcPath, session.namespace, session.podName, dstPath, msg, err)
		}
		return fmt.Errorf("copying %s to %s/%s:%s: %w", srcPath, session.namespace, session.podName, dstPath, err)
	}
	fmt.Printf("Copied %s to %s/%s:%s\n", srcPath, session.namespace, session.podName, dstPath)
	return nil
}

type kubernetesCpSessionInfo struct {
	config        *rest.Config
	clientset     *kubernetes.Clientset
	namespace     string
	podName       string
	containerName string
}

// kubernetesCpSession reuses or creates a debux ephemeral container on the
// target pod so tar is guaranteed to be available.
func kubernetesCpSession(ctx context.Context, target *Target, opts DebugOpts) (*kubernetesCpSessionInfo, error) {
	config, clientset, err := getK8sClient(opts.Kubeconfig, opts.KubeContext)
	if err != nil {
		return nil, err
	}
	namespace := resolveTargetNamespace(target.Namespace, opts.Kubeconfig, opts.KubeContext)

	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting pod %s/%s: %w", namespace, target.Name, err)
	}

	displayContext := kubernetesDisplayContext(opts.Kubeconfig, opts.KubeContext)
	containerName, _, err := ensureKubernetesDebugContainer(ctx, clientset, namespace, pod, target.Container, displayContext, opts)
	if err != nil {
		return nil, err
	}
	return &kubernetesCpSessionInfo{
		config:        config,
		clientset:     clientset,
		namespace:     namespace,
		podName:       pod.Name,
		containerName: containerName,
	}, nil
}

// kubernetesTargetRootPath translates a target-container path into the
// debug container's view of the shared PID namespace.
func kubernetesTargetRootPath(p string) string {
	return "/proc/1/root" + path.Clean("/"+p)
}

// execPodStream runs a non-TTY command in a pod container with raw streams.
func execPodStream(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}
