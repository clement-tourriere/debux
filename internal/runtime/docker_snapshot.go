package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/clement-tourriere/debux/internal/dockerclient"
	"github.com/clement-tourriere/debux/internal/entrypoint"
	dbximage "github.com/clement-tourriere/debux/internal/image"
	"github.com/clement-tourriere/debux/internal/store"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// DockerImage debugs a Docker image by copying its filesystem into a debug container.
// This works for ALL images including scratch/distroless — the target image is never started.
func DockerImage(ctx context.Context, imageRef string, opts ImageOpts) error {
	cli, err := dockerclient.New()
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	// Check if the target image exists locally; if not, try pulling it.
	// Unlike the debug image, the target may be a local-only build that
	// should never be pulled from a registry.
	_, inspectErr := cli.ImageInspect(ctx, imageRef)
	if inspectErr != nil {
		// Image not found locally — attempt a pull (works for remote images)
		if pullErr := dbximage.EnsureImage(ctx, cli, imageRef); pullErr != nil {
			return fmt.Errorf("image %q not found locally and could not be pulled: %w", imageRef, pullErr)
		}
	}

	// Create a stopped container from the target image to access its filesystem.
	// We use "true" as the command — it's never started, we just need the container layer.
	targetName := fmt.Sprintf("debux-image-target-%s", sanitizeImageRef(imageRef))
	if err := removeDockerContainerNameIfManaged(ctx, cli, targetName); err != nil {
		return err
	}

	statusf("Creating target container from %s...\n", imageRef)
	targetResp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: imageRef,
			Cmd:   []string{"true"},
			Labels: map[string]string{
				dockerLabelManagedBy:  dockerLabelManagedByVal,
				dockerLabelKind:       dockerLabelKindImageTarget,
				dockerLabelTargetName: imageRef,
			},
		},
		Name: targetName,
	})
	if err != nil {
		return fmt.Errorf("creating target container: %w", err)
	}
	targetID := targetResp.ID
	defer func() {
		cleanupDockerContainer(ctx, cli, targetID)
	}()

	// Stream the entire target filesystem
	statusf("Copying filesystem from %s...\n", imageRef)
	copyResult, err := cli.CopyFromContainer(ctx, targetID, client.CopyFromContainerOptions{SourcePath: "/"})
	if err != nil {
		return fmt.Errorf("copying filesystem from target: %w", err)
	}
	tarReader := copyResult.Content
	defer func() { _ = tarReader.Close() }()

	// Ensure debug image and its matching persistent tool storage.
	if err := dbximage.EnsureImage(ctx, cli, opts.DebugImage); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}
	volumes, err := debugImageVolumes(ctx, cli, DebugOpts{Image: opts.DebugImage, User: opts.User, Privileged: opts.Privileged})
	if err != nil {
		return err
	}
	if err := store.EnsureVolumes(ctx, cli, volumes); err != nil {
		return fmt.Errorf("ensuring store volumes: %w", err)
	}

	// Create the debug container
	debugName := fmt.Sprintf("debux-image-%s", sanitizeImageRef(imageRef))
	if err := removeDockerContainerNameIfManaged(ctx, cli, debugName); err != nil {
		return err
	}

	tty := stdioIsTTY()
	config := &container.Config{
		Image:        opts.DebugImage,
		Entrypoint:   []string{"/bin/sh", "-c", entrypoint.ImageScript},
		Tty:          tty,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Labels: map[string]string{
			dockerLabelManagedBy:  dockerLabelManagedByVal,
			dockerLabelKind:       dockerLabelKindImageMode,
			dockerLabelTargetName: imageRef,
			dockerLabelDebugImage: opts.DebugImage,
			dockerLabelDebugUser:  opts.User,
		},
		Env: []string{
			"HOME=/root",
			fmt.Sprintf("DEBUX_TARGET=%s", imageRef),
		},
	}
	if len(opts.Command) > 0 {
		config.Env = append(config.Env, "DEBUX_EXEC_COMMAND="+shellJoin(opts.Command))
	}

	hostConfig := &container.HostConfig{
		Mounts:     volumes.Mounts(),
		AutoRemove: opts.AutoRemove,
		Privileged: opts.Privileged,
	}

	if opts.User != "" {
		config.User = opts.User
	}

	debugResp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     config,
		HostConfig: hostConfig,
		Name:       debugName,
	})
	if err != nil {
		return fmt.Errorf("creating debug container: %w", err)
	}
	debugID := debugResp.ID

	if opts.AutoRemove {
		defer func() {
			// Docker removes the container automatically after it exits, but this
			// also cleans up failures that happen before the container starts.
			cleanupDockerContainer(ctx, cli, debugID)
		}()
	}

	// Create /target directory inside the debug container via a tar archive
	if err := mkdirViaTar(ctx, cli, debugID, "target"); err != nil {
		return fmt.Errorf("creating /target directory: %w", err)
	}

	// Copy the target filesystem into /target inside the debug container
	if _, err := cli.CopyToContainer(ctx, debugID, client.CopyToContainerOptions{DestinationPath: "/target", Content: tarReader}); err != nil {
		return fmt.Errorf("copying filesystem to debug container: %w", err)
	}

	statusf("Debugging image %s (container: %s)\n", imageRef, debugName)

	return runInteractiveContainer(ctx, cli, debugID, tty, opts.AutoRemove)
}

const stoppedTargetEnvironPath = "/tmp/debux-target-environ"

// dockerExecCopy debugs a non-running container by copying its filesystem —
// including the writable layer with logs, crash artifacts, and modified
// config — into a fresh debug container at /target. The original container is
// never started; changes outside mounted volumes are discarded on exit.
func dockerExecCopy(ctx context.Context, cli *client.Client, targetInfo container.InspectResponse, target *Target, opts DebugOpts) error {
	targetID := targetInfo.ID
	targetName := strings.TrimPrefix(targetInfo.Name, "/")
	status := "stopped"
	if targetInfo.State != nil && targetInfo.State.Status != "" {
		status = string(targetInfo.State.Status)
	}
	statusf("Target container %q is %s; debugging a copy of its filesystem (changes outside volumes are discarded on exit).\n", targetName, status)

	copyResult, err := cli.CopyFromContainer(ctx, targetID, client.CopyFromContainerOptions{SourcePath: "/"})
	if err != nil {
		return fmt.Errorf("copying filesystem from target: %w", err)
	}
	tarReader := copyResult.Content
	defer func() { _ = tarReader.Close() }()

	if err := dbximage.EnsureImageWithPolicy(ctx, cli, opts.Image, opts.PullPolicy); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}
	volumes, err := debugImageVolumes(ctx, cli, opts)
	if err != nil {
		return err
	}
	if err := store.EnsureVolumes(ctx, cli, volumes); err != nil {
		return fmt.Errorf("ensuring store volumes: %w", err)
	}

	debugName := fmt.Sprintf("debux-%s", targetName)
	if err := removeDockerContainerNameIfManaged(ctx, cli, debugName); err != nil {
		return err
	}

	tty := stdioIsTTY()
	targetImage := ""
	var targetEnv []string
	if targetInfo.Config != nil {
		targetImage = targetInfo.Config.Image
		targetEnv = targetInfo.Config.Env
	}

	config := &container.Config{
		Image:        opts.Image,
		Entrypoint:   []string{"/bin/sh", "-c", entrypoint.ImageScript},
		Tty:          tty,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Labels: map[string]string{
			dockerLabelManagedBy:   dockerLabelManagedByVal,
			dockerLabelKind:        dockerLabelKindStoppedCopy,
			dockerLabelTargetID:    targetID,
			dockerLabelTargetName:  targetName,
			dockerLabelTargetImage: targetImage,
			dockerLabelDebugImage:  opts.Image,
			dockerLabelDebugUser:   opts.User,
		},
		Env: []string{
			"HOME=/root",
			fmt.Sprintf("DEBUX_TARGET=%s", targetName),
			fmt.Sprintf("DEBUX_TARGET_ID=%s", targetID),
			// The isolated chroot wrappers read the target environment
			// from this file since there is no live /proc/1.
			fmt.Sprintf("DEBUX_TARGET_ENVIRON=%s", stoppedTargetEnvironPath),
		},
	}
	extraEnv, err := debugExtraEnv(opts.Env, opts.Tools)
	if err != nil {
		return err
	}
	config.Env = append(config.Env, extraEnv...)
	if len(opts.Command) > 0 {
		config.Env = append(config.Env, "DEBUX_EXEC_COMMAND="+shellJoin(opts.Command))
	}
	if opts.User != "" {
		config.User = opts.User
	}

	hostConfig := &container.HostConfig{
		Mounts:     volumes.Mounts(),
		AutoRemove: true,
		Privileged: opts.Privileged,
		CapAdd:     normalizeCapabilities(opts.CapAdd),
	}
	if opts.ShareVolumes {
		shared := targetMountsAt(targetInfo, opts.ReadOnlyVolumes, "/target")
		if len(shared) > 0 {
			statusf("Mounting %d volume(s) from %s under /target\n", len(shared), targetName)
			hostConfig.Mounts = append(hostConfig.Mounts, shared...)
		}
	}

	debugResp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     config,
		HostConfig: hostConfig,
		Name:       debugName,
	})
	if err != nil {
		return fmt.Errorf("creating debug container: %w", err)
	}
	debugID := debugResp.ID
	defer func() {
		// AutoRemove covers the started case; this cleans up failures that
		// happen before the container starts.
		cleanupDockerContainer(ctx, cli, debugID)
	}()

	if err := mkdirViaTar(ctx, cli, debugID, "target"); err != nil {
		return fmt.Errorf("creating /target directory: %w", err)
	}
	statusf("Copying filesystem from %s...\n", targetName)
	if _, err := cli.CopyToContainer(ctx, debugID, client.CopyToContainerOptions{DestinationPath: "/target", Content: tarReader}); err != nil {
		return fmt.Errorf("copying filesystem to debug container: %w", err)
	}
	if err := copyTargetEnvironFile(ctx, cli, debugID, targetEnv); err != nil {
		return fmt.Errorf("writing target environment file: %w", err)
	}

	statusf("Debugging %s (container: %s, target root: /target)\n", targetName, debugName)
	return runInteractiveContainer(ctx, cli, debugID, tty, true)
}

// copyTargetEnvironFile writes the target container's configured environment
// into the debug container as a NUL-separated file — the /proc/<pid>/environ
// format the shell helpers already understand.
func copyTargetEnvironFile(ctx context.Context, cli *client.Client, containerID string, env []string) error {
	if len(env) == 0 {
		return nil
	}
	content := strings.Join(env, "\x00") + "\x00"
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(stoppedTargetEnvironPath, "/"),
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(content)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	_, err := cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: "/", Content: &buf})
	return err
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
	_, err := cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: "/", Content: &buf})
	return err
}

// sanitizeImageRef converts an image reference into a valid container name suffix.
// e.g. "gcr.io/distroless/static:latest" → "gcr-io-distroless-static-latest-abc123"
// A short hash of the original reference is appended so different refs such as
// "a/b" and "a-b" cannot collide after sanitization.
func sanitizeImageRef(ref string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		":", "-",
		".", "-",
		"@", "-",
	)
	sanitized := replacer.Replace(ref)
	hash := sha256.Sum256([]byte(ref))
	return sanitized + "-" + hex.EncodeToString(hash[:])[:8]
}
