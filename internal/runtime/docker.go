package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ctourriere/debux/internal/entrypoint"
	dbximage "github.com/ctourriere/debux/internal/image"
	"github.com/ctourriere/debux/internal/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/moby/term"
)

// DockerExec launches a debug sidecar sharing namespaces with the target container.
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

	// Ensure debug image is available
	if err := dbximage.EnsureImage(ctx, cli, opts.Image); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}

	// Ensure persistent nix volumes
	if err := store.EnsureVolumes(ctx, cli); err != nil {
		return fmt.Errorf("ensuring store volumes: %w", err)
	}

	// Build container config
	// Docker container names from inspect have a leading /, strip it
	targetName := strings.TrimPrefix(targetInfo.Name, "/")
	containerName := fmt.Sprintf("debux-%s", targetName)

	config := &container.Config{
		Image:        opts.Image,
		Cmd:          []string{"/bin/sh", "-c", entrypoint.Script},
		Tty:          true,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env: []string{
			fmt.Sprintf("DEBUX_TARGET=%s", target.Name),
			fmt.Sprintf("DEBUX_TARGET_ID=%s", targetID),
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

	// Remove any existing debug container with the same name
	_ = cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})

	fmt.Printf("Creating debug container for %s...\n", target.Name)

	// Create the debug container
	resp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, containerName)
	if err != nil {
		return fmt.Errorf("creating debug container: %w", err)
	}

	debugID := resp.ID

	// Ensure cleanup if auto-remove is off
	if !opts.AutoRemove {
		defer func() {
			_ = cli.ContainerRemove(context.Background(), debugID, container.RemoveOptions{Force: true})
		}()
	}

	// Attach to the container
	attachOpts := container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	}

	hijacked, err := cli.ContainerAttach(ctx, debugID, attachOpts)
	if err != nil {
		return fmt.Errorf("attaching to debug container: %w", err)
	}
	defer hijacked.Close()

	// Start the container
	if err := cli.ContainerStart(ctx, debugID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting debug container: %w", err)
	}

	fmt.Printf("Debugging %s (container: %s)\n", target.Name, containerName)

	// Set terminal to raw mode
	stdinFd, isTerminal := term.GetFdInfo(os.Stdin)
	if isTerminal {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
			}()
		}
	}

	// Handle TTY resize
	if isTerminal {
		resizeTTY(ctx, cli, debugID, stdinFd)
	}

	// Stream I/O
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

	// Wait for exit
	statusCh, errCh := cli.ContainerWait(ctx, debugID, container.WaitConditionNotRunning)

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("waiting for container: %w", err)
		}
	case <-statusCh:
		// Shell exited — non-zero is normal (last command's exit code)
	case <-inputDone:
		// stdin closed, wait briefly for output
		select {
		case <-outputDone:
		case <-statusCh:
		}
	}

	return nil
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
