package runtime

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/entrypoint"
	dbximage "github.com/clement-tourriere/debux/internal/image"
	"github.com/clement-tourriere/debux/internal/store"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/moby/term"
)

// ContainerInfo holds metadata about a running Docker container.
type ContainerInfo struct {
	ID              string
	Name            string
	Image           string
	Status          string
	HasDebuxSession bool // true if a debux sidecar is running for this container
}

// DockerList returns running Docker containers, excluding debux sidecars.
func DockerList(ctx context.Context) ([]ContainerInfo, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	containers, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	// Collect names of running debux sidecars to mark active sessions
	debuxTargets := make(map[string]bool)
	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if strings.HasPrefix(name, "debux-") && c.State == "running" {
			debuxTargets[strings.TrimPrefix(name, "debux-")] = true
		}
	}

	var result []ContainerInfo
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		// Skip debux sidecars themselves
		if strings.HasPrefix(name, "debux-") {
			continue
		}
		result = append(result, ContainerInfo{
			ID:              c.ID[:12],
			Name:            name,
			Image:           c.Image,
			Status:          c.Status,
			HasDebuxSession: debuxTargets[name],
		})
	}
	return result, nil
}

// DockerExec launches a debug sidecar sharing namespaces with the target container.
// The sidecar runs in daemon mode (tail -f /dev/null) and persists between sessions,
// matching K8s ephemeral container behavior. Interactive shells are started via exec.
func DockerExec(ctx context.Context, target *Target, opts DebugOpts) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer cli.Close()

	// Verify target container exists and is running
	targetInfo, err := cli.ContainerInspect(ctx, target.Name)
	if err != nil {
		return fmt.Errorf("inspecting target container %q: %w", target.Name, err)
	}
	if !targetInfo.State.Running {
		return fmt.Errorf("target container %q is not running", target.Name)
	}

	targetID := targetInfo.ID
	targetName := strings.TrimPrefix(targetInfo.Name, "/")
	containerName := fmt.Sprintf("debux-%s", targetName)

	// Try to reuse an existing running debux sidecar
	if !opts.Fresh {
		if info, err := cli.ContainerInspect(ctx, containerName); err == nil && info.State.Running {
			fmt.Printf("Reusing debug container %q\n", containerName)
			fmt.Printf("Debugging %s (container: %s)\n", target.Name, containerName)
			return execInContainer(ctx, cli, info.ID)
		}
	}

	// Ensure debug image is available
	if err := dbximage.EnsureImage(ctx, cli, opts.Image); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}

	// Ensure persistent nix volumes
	if err := store.EnsureVolumes(ctx, cli); err != nil {
		return fmt.Errorf("ensuring store volumes: %w", err)
	}

	config := &container.Config{
		Image:      opts.Image,
		Entrypoint: []string{"/bin/sh", "-c", entrypoint.Script},
		Tty:        true,
		Env: []string{
			fmt.Sprintf("DEBUX_TARGET=%s", target.Name),
			fmt.Sprintf("DEBUX_TARGET_ID=%s", targetID),
			"DEBUX_TARGET_ROOT=/proc/1/root",
			"DEBUX_DAEMON=1",
		},
	}

	// Share IPC only if the target allows it
	ipcMode := container.IpcMode(fmt.Sprintf("container:%s", targetID))
	if targetInfo.HostConfig != nil && targetInfo.HostConfig.IpcMode != "" && targetInfo.HostConfig.IpcMode != "shareable" {
		ipcMode = "private"
	}

	hostConfig := &container.HostConfig{
		NetworkMode: container.NetworkMode(fmt.Sprintf("container:%s", targetID)),
		PidMode:     container.PidMode(fmt.Sprintf("container:%s", targetID)),
		IpcMode:     ipcMode,
		CapAdd:      []string{"SYS_PTRACE"},
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: store.NixStoreVolume,
				Target: "/nix/store",
			},
			{
				Type:   mount.TypeVolume,
				Source: store.NixVarVolume,
				Target: "/nix/var",
			},
		},
		Privileged: opts.Privileged,
	}

	// Share target container's volumes
	if opts.ShareVolumes {
		shared := targetMounts(targetInfo)
		if len(shared) > 0 {
			fmt.Printf("Sharing %d volume(s) from %s\n", len(shared), targetName)
			hostConfig.Mounts = append(hostConfig.Mounts, shared...)
		}
	}

	if opts.User != "" {
		config.User = opts.User
	}

	// Remove any existing (stopped) debug container with the same name
	_ = cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})

	fmt.Printf("Creating debug container for %s...\n", target.Name)

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, containerName)
	if err != nil {
		return fmt.Errorf("creating debug container: %w", err)
	}

	// Start the sidecar in daemon mode (entrypoint does setup, then tail -f /dev/null)
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("starting debug container: %w", err)
	}

	// Show entrypoint output (volumes, warnings)
	showEntrypointOutput(ctx, cli, resp.ID)

	fmt.Printf("Debugging %s (container: %s)\n", target.Name, containerName)

	return execInContainer(ctx, cli, resp.ID)
}

// runInteractiveContainer attaches to a created container, starts it, streams
// I/O (with raw terminal mode and TTY resize), and waits for it to exit.
func runInteractiveContainer(ctx context.Context, cli *client.Client, containerID string) error {
	attachOpts := container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	}

	hijacked, err := cli.ContainerAttach(ctx, containerID, attachOpts)
	if err != nil {
		return fmt.Errorf("attaching to container: %w", err)
	}
	defer hijacked.Close()

	if err := cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	stdinFd, isTerminal := term.GetFdInfo(os.Stdin)
	if isTerminal {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
	}

	if isTerminal {
		resizeTTY(ctx, cli, containerID, stdinFd)
	}

	outputDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(os.Stdout, hijacked.Reader)
		outputDone <- err
	}()

	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		_, _ = io.Copy(hijacked.Conn, os.Stdin)
	}()

	statusCh, errCh := cli.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("waiting for container: %w", err)
		}
	case <-statusCh:
	case <-inputDone:
		select {
		case <-outputDone:
		case <-statusCh:
		}
	}

	return nil
}

// DockerImage debugs a Docker image by copying its filesystem into a debug container.
// This works for ALL images including scratch/distroless — the target image is never started.
func DockerImage(ctx context.Context, imageRef string, opts ImageOpts) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer cli.Close()

	// Check if the target image exists locally; if not, try pulling it.
	// Unlike the debug image, the target may be a local-only build that
	// should never be pulled from a registry.
	_, _, inspectErr := cli.ImageInspectWithRaw(ctx, imageRef)
	if inspectErr != nil {
		// Image not found locally — attempt a pull (works for remote images)
		if pullErr := dbximage.EnsureImage(ctx, cli, imageRef); pullErr != nil {
			return fmt.Errorf("image %q not found locally and could not be pulled: %w", imageRef, pullErr)
		}
	}

	// Create a stopped container from the target image to access its filesystem.
	// We use "true" as the command — it's never started, we just need the container layer.
	targetName := fmt.Sprintf("debux-image-target-%s", sanitizeImageRef(imageRef))
	_ = cli.ContainerRemove(ctx, targetName, container.RemoveOptions{Force: true})

	fmt.Printf("Creating target container from %s...\n", imageRef)
	targetResp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: imageRef,
		Cmd:   []string{"true"},
	}, nil, nil, nil, targetName)
	if err != nil {
		return fmt.Errorf("creating target container: %w", err)
	}
	targetID := targetResp.ID
	defer func() {
		_ = cli.ContainerRemove(context.Background(), targetID, container.RemoveOptions{Force: true})
	}()

	// Stream the entire target filesystem
	fmt.Printf("Copying filesystem from %s...\n", imageRef)
	tarReader, _, err := cli.CopyFromContainer(ctx, targetID, "/")
	if err != nil {
		return fmt.Errorf("copying filesystem from target: %w", err)
	}
	defer func() { _ = tarReader.Close() }()

	// Ensure debug image and nix volumes
	if err := dbximage.EnsureImage(ctx, cli, opts.DebugImage); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}
	if err := store.EnsureVolumes(ctx, cli); err != nil {
		return fmt.Errorf("ensuring store volumes: %w", err)
	}

	// Create the debug container
	debugName := fmt.Sprintf("debux-image-%s", sanitizeImageRef(imageRef))
	_ = cli.ContainerRemove(ctx, debugName, container.RemoveOptions{Force: true})

	config := &container.Config{
		Image:        opts.DebugImage,
		Entrypoint:   []string{"/bin/sh", "-c", entrypoint.ImageScript},
		Tty:          true,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env: []string{
			fmt.Sprintf("DEBUX_TARGET=%s", imageRef),
		},
	}

	hostConfig := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: store.NixStoreVolume,
				Target: "/nix/store",
			},
			{
				Type:   mount.TypeVolume,
				Source: store.NixVarVolume,
				Target: "/nix/var",
			},
		},
		AutoRemove: opts.AutoRemove,
		Privileged: opts.Privileged,
	}

	if opts.User != "" {
		config.User = opts.User
	}

	debugResp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, debugName)
	if err != nil {
		return fmt.Errorf("creating debug container: %w", err)
	}
	debugID := debugResp.ID

	if !opts.AutoRemove {
		defer func() {
			_ = cli.ContainerRemove(context.Background(), debugID, container.RemoveOptions{Force: true})
		}()
	}

	// Create /target directory inside the debug container via a tar archive
	if err := mkdirViaTar(ctx, cli, debugID, "target"); err != nil {
		return fmt.Errorf("creating /target directory: %w", err)
	}

	// Copy the target filesystem into /target inside the debug container
	if err := cli.CopyToContainer(ctx, debugID, "/target", tarReader, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("copying filesystem to debug container: %w", err)
	}

	fmt.Printf("Debugging image %s (container: %s)\n", imageRef, debugName)

	return runInteractiveContainer(ctx, cli, debugID)
}

// mkdirViaTar creates a directory at /<name> inside a stopped container by
// copying a minimal tar archive containing a single directory entry.
func mkdirViaTar(ctx context.Context, cli *client.Client, containerID, name string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name + "/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return cli.CopyToContainer(ctx, containerID, "/", &buf, container.CopyToContainerOptions{})
}

// sanitizeImageRef converts an image reference into a valid container name suffix.
// e.g. "gcr.io/distroless/static:latest" → "gcr-io-distroless-static-latest"
func sanitizeImageRef(ref string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		":", "-",
		".", "-",
		"@", "-",
	)
	return replacer.Replace(ref)
}

// targetMounts extracts the target container's mounts and converts them to
// mount.Mount entries for the debug container, skipping paths reserved by debux.
func targetMounts(info types.ContainerJSON) []mount.Mount {
	if info.Mounts == nil {
		return nil
	}
	// Paths used by the debug container itself — skip conflicts
	reserved := map[string]bool{
		"/nix/store": true,
		"/nix/var":   true,
	}
	var mounts []mount.Mount
	for _, mp := range info.Mounts {
		if reserved[mp.Destination] {
			continue
		}
		m := mount.Mount{
			Type:     mp.Type,
			Target:   mp.Destination,
			ReadOnly: !mp.RW,
		}
		switch mp.Type {
		case mount.TypeVolume:
			m.Source = mp.Name
		case mount.TypeBind:
			m.Source = mp.Source
			if mp.Propagation != "" {
				m.BindOptions = &mount.BindOptions{Propagation: mp.Propagation}
			}
		case mount.TypeTmpfs:
			// no source needed
		default:
			continue // skip unknown types
		}
		mounts = append(mounts, m)
	}
	return mounts
}

func resizeTTY(ctx context.Context, cli *client.Client, containerID string, fd uintptr) {
	resize := func() {
		size, err := term.GetWinsize(fd)
		if err != nil || size == nil {
			return
		}
		_ = cli.ContainerResize(ctx, containerID, container.ResizeOptions{
			Height: uint(size.Height),
			Width:  uint(size.Width),
		})
	}

	// Initial resize
	resize()

	// Watch for SIGWINCH is handled by the terminal library
	// The raw mode terminal will propagate resize events
}

// execInContainer starts an interactive zsh session inside a running container
// using docker exec, similar to how K8s uses exec into daemon ephemeral containers.
func execInContainer(ctx context.Context, cli *client.Client, containerID string) error {
	resp, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          []string{"zsh"},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
	})
	if err != nil {
		return fmt.Errorf("creating exec session: %w", err)
	}

	hijacked, err := cli.ContainerExecAttach(ctx, resp.ID, container.ExecAttachOptions{
		Tty: true,
	})
	if err != nil {
		return fmt.Errorf("attaching to exec session: %w", err)
	}
	defer hijacked.Close()

	stdinFd, isTerminal := term.GetFdInfo(os.Stdin)
	if isTerminal {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
	}

	if isTerminal {
		size, err := term.GetWinsize(stdinFd)
		if err == nil && size != nil {
			_ = cli.ContainerExecResize(ctx, resp.ID, container.ResizeOptions{
				Height: uint(size.Height),
				Width:  uint(size.Width),
			})
		}
	}

	outputDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(os.Stdout, hijacked.Reader)
		outputDone <- err
	}()

	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		_, _ = io.Copy(hijacked.Conn, os.Stdin)
	}()

	select {
	case <-outputDone:
	case <-inputDone:
		<-outputDone
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

// showEntrypointOutput streams the sidecar's entrypoint output (volume listing,
// warnings) to stdout. The entrypoint prints info then enters daemon mode
// (tail -f /dev/null). We follow the logs until we see a blank line marking
// the end of the entrypoint output, with a timeout as safety net.
func showEntrypointOutput(ctx context.Context, cli *client.Client, containerID string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	reader, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return
	}
	defer func() { _ = reader.Close() }()

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		// Empty line (possibly with \r from TTY) marks end of entrypoint output
		if strings.TrimRight(line, "\r") == "" {
			break
		}
		fmt.Println(strings.TrimRight(line, "\r"))
	}
}
