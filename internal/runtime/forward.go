package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	dbximage "github.com/clement-tourriere/debux/internal/image"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortMapping forwards a local port to a port on the target.
type PortMapping struct {
	Local  uint16
	Remote uint16
}

// ParsePortMappings parses [LOCAL:]REMOTE specs.
func ParsePortMappings(specs []string) ([]PortMapping, error) {
	mappings := make([]PortMapping, 0, len(specs))
	for _, spec := range specs {
		local, remote, hasLocal := strings.Cut(spec, ":")
		if !hasLocal {
			remote = local
		}
		remotePort, err := parsePortNumber(remote)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q: %w", spec, err)
		}
		localPort := remotePort
		if hasLocal {
			localPort, err = parsePortNumber(local)
			if err != nil {
				return nil, fmt.Errorf("invalid port %q: %w", spec, err)
			}
		}
		mappings = append(mappings, PortMapping{Local: localPort, Remote: remotePort})
	}
	return mappings, nil
}

func parsePortNumber(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("expected a port number between 1 and 65535")
	}
	return uint16(n), nil
}

// DockerForward publishes unpublished ports of a running container on
// 127.0.0.1 by running a socat relay container on the target's network —
// no restart or recreate of the target required.
func DockerForward(ctx context.Context, target *Target, mappings []PortMapping, pullPolicy, image string) error {
	cli, err := dockerClientForTarget(target)
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	if target.ComposeService != "" {
		name, err := resolveComposeContainer(ctx, cli, target.ComposeProject, target.ComposeService)
		if err != nil {
			return err
		}
		resolved := *target
		resolved.Name = name
		target = &resolved
	}

	info, err := inspectDockerContainer(ctx, cli, target.Name)
	if err != nil {
		return fmt.Errorf("inspecting target container %q: %w", target.Name, err)
	}
	if info.State == nil || !info.State.Running {
		return fmt.Errorf("target container %q is not running; port forwarding needs a live process", target.Name)
	}
	if info.HostConfig != nil && info.HostConfig.NetworkMode.IsHost() {
		return fmt.Errorf("target container %q uses host networking; its ports are already reachable on localhost", target.Name)
	}

	netName, targetIP := dockerContainerAddress(info)
	if targetIP == "" {
		return fmt.Errorf("target container %q has no reachable network address (network mode %q)", target.Name, info.HostConfig.NetworkMode)
	}

	if err := dbximage.EnsureImageWithPolicy(ctx, cli, image, pullPolicy); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}

	relays := make([]string, 0, len(mappings))
	exposed := network.PortSet{}
	bindings := network.PortMap{}
	loopback := netip.MustParseAddr("127.0.0.1")
	for _, m := range mappings {
		relays = append(relays, fmt.Sprintf("socat TCP-LISTEN:%d,fork,reuseaddr TCP:%s:%d &", m.Remote, targetIP, m.Remote))
		port, err := network.ParsePort(fmt.Sprintf("%d/tcp", m.Remote))
		if err != nil {
			return fmt.Errorf("invalid port %d: %w", m.Remote, err)
		}
		exposed[port] = struct{}{}
		bindings[port] = []network.PortBinding{{HostIP: loopback, HostPort: strconv.Itoa(int(m.Local))}}
	}
	script := strings.Join([]string{
		// Older debug images may predate socat in the toolbox; install on demand.
		"command -v socat >/dev/null 2>&1 || dctl install socat >/dev/null 2>&1 || { echo 'socat is not available in the debug image and could not be installed' >&2; exit 1; }",
		strings.Join(relays, "\n"),
		"wait",
	}, "\n")

	name := fmt.Sprintf("debux-forward-%s-%d", sanitizeImageRef(strings.TrimPrefix(info.Name, "/")), time.Now().UnixNano()%1e9)
	resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        image,
			Entrypoint:   []string{"/bin/sh", "-c", script},
			ExposedPorts: exposed,
			Labels: map[string]string{
				dockerLabelManagedBy:  dockerLabelManagedByVal,
				dockerLabelKind:       "docker-forward",
				dockerLabelTargetID:   info.ID,
				dockerLabelTargetName: strings.TrimPrefix(info.Name, "/"),
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:  container.NetworkMode(netName),
			PortBindings: bindings,
			AutoRemove:   true,
		},
		Name: name,
	})
	if err != nil {
		return fmt.Errorf("creating forward container: %w", err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), resp.ID, client.ContainerRemoveOptions{Force: true})
	}()

	waitResult := cli.ContainerWait(ctx, resp.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting forward container: %w", err)
	}

	for _, m := range mappings {
		fmt.Printf("Forwarding 127.0.0.1:%d -> %s:%d\n", m.Local, strings.TrimPrefix(info.Name, "/"), m.Remote)
	}
	fmt.Println("Press Ctrl-C to stop forwarding")

	select {
	case <-ctx.Done():
		return nil
	case err := <-waitResult.Error:
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("forward container: %w", err)
		}
		return nil
	case status := <-waitResult.Result:
		if ctx.Err() != nil {
			return nil
		}
		logs := dockerContainerLogTail(ctx, cli, resp.ID)
		if logs != "" {
			return fmt.Errorf("forward container exited with status %d: %s", status.StatusCode, logs)
		}
		return fmt.Errorf("forward container exited with status %d", status.StatusCode)
	}
}

// dockerContainerAddress returns the first network with an IP address.
func dockerContainerAddress(info container.InspectResponse) (networkName, ip string) {
	if info.NetworkSettings == nil {
		return "", ""
	}
	for name, ep := range info.NetworkSettings.Networks {
		if ep != nil && ep.IPAddress.IsValid() && !ep.IPAddress.IsUnspecified() {
			return name, ep.IPAddress.String()
		}
	}
	return "", ""
}

func dockerContainerLogTail(ctx context.Context, cli *client.Client, containerID string) string {
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	reader, err := cli.ContainerLogs(logCtx, containerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "5"})
	if err != nil {
		return ""
	}
	defer func() { _ = reader.Close() }()
	data := make([]byte, 2048)
	n, _ := reader.Read(data)
	return strings.TrimSpace(string(data[:n]))
}

// KubernetesForward streams local ports to a pod via the Kubernetes
// port-forward subresource (same plumbing as kubectl port-forward).
func KubernetesForward(ctx context.Context, target *Target, kubeconfig, kubeContext string, mappings []PortMapping) error {
	config, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	namespace := resolveTargetNamespace(target.Namespace, kubeconfig, kubeContext)

	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting pod %s/%s: %w", namespace, target.Name, err)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("pod %s/%s is %s; port forwarding needs a running pod", namespace, target.Name, pod.Status.Phase)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(target.Name).
		SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return fmt.Errorf("creating port-forward transport: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	ports := make([]string, 0, len(mappings))
	for _, m := range mappings {
		ports = append(ports, fmt.Sprintf("%d:%d", m.Local, m.Remote))
	}

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stopCh)
	}()
	go func() {
		select {
		case <-readyCh:
			fmt.Println("Press Ctrl-C to stop forwarding")
		case <-ctx.Done():
		}
	}()

	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, ports, stopCh, readyCh, os.Stdout, os.Stderr)
	if err != nil {
		return fmt.Errorf("creating port forwarder: %w", err)
	}
	if err := fw.ForwardPorts(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("port forwarding: %w", err)
	}
	return nil
}
