package runtime

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/dockerclient"
	"github.com/clement-tourriere/debux/internal/entrypoint"
	dbximage "github.com/clement-tourriere/debux/internal/image"
	"github.com/clement-tourriere/debux/internal/store"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/moby/term"
)

const (
	dockerLabelManagedBy       = "app.kubernetes.io/managed-by"
	dockerLabelKind            = "debux.clement-tourriere/kind"
	dockerLabelTargetID        = "debux.clement-tourriere/target-id"
	dockerLabelTargetName      = "debux.clement-tourriere/target-name"
	dockerLabelTargetImage     = "debux.clement-tourriere/target-image"
	dockerLabelDebugImage      = "debux.clement-tourriere/debug-image"
	dockerLabelDebugUser       = "debux.clement-tourriere/debug-user"
	dockerLabelManagedByVal    = "debux"
	dockerLabelKindSidecar     = "docker-sidecar"
	dockerLabelKindImageMode   = "docker-image"
	dockerLabelKindImageTarget = "docker-image-target"
	dockerLabelKindStoppedCopy = "docker-stopped-copy"
	dockerAnyDebugUser         = "\x00"
	dockerAnyDebugImage        = "\x00"
)

// ContainerInfo holds metadata about a running Docker container.
type ContainerInfo struct {
	ID              string
	Name            string
	Image           string
	Status          string
	HasDebuxSession bool // true if a debux sidecar is running for this container
}

// ImageInfo holds metadata about a local Docker image reference.
type ImageInfo struct {
	Ref        string
	ID         string
	Containers int64
}

// DockerList returns running Docker containers, excluding debux sidecars.
func DockerList(ctx context.Context) ([]ContainerInfo, error) {
	cli, err := dockerclient.New()
	if err != nil {
		return nil, fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	containers, err := listDockerContainers(ctx, cli)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	debuxTargetsByID := make(map[string]bool)
	debuxTargetsByName := make(map[string]bool)
	for _, c := range containers {
		if c.State != "running" || !isDebuxDockerSidecar(c) {
			continue
		}
		if targetID := c.Labels[dockerLabelTargetID]; targetID != "" {
			debuxTargetsByID[targetID] = true
		}
		if targetName := c.Labels[dockerLabelTargetName]; targetName != "" && c.Labels[dockerLabelTargetID] == "" {
			debuxTargetsByName[targetName] = true
		}
		// Legacy debux sidecars did not have labels. Keep marking them so users
		// still see active sessions after upgrading.
		if name := dockerContainerPrimaryName(c); strings.HasPrefix(name, "debux-") {
			debuxTargetsByName[strings.TrimPrefix(name, "debux-")] = true
		}
	}

	var result []ContainerInfo
	for _, c := range containers {
		if c.State != "running" || isDebuxDockerManagedContainer(c) {
			continue
		}
		name := dockerContainerPrimaryName(c)
		result = append(result, ContainerInfo{
			ID:              shortContainerID(c.ID),
			Name:            name,
			Image:           c.Image,
			Status:          c.Status,
			HasDebuxSession: debuxTargetsByID[c.ID] || debuxTargetsByName[name],
		})
	}
	return result, nil
}

// DockerSessions returns running debux sidecar sessions that can be reattached.
func DockerSessions(ctx context.Context) ([]DebugSessionInfo, error) {
	cli, err := dockerclient.New()
	if err != nil {
		return nil, fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	containers, err := listDockerContainers(ctx, cli)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var result []DebugSessionInfo
	for _, c := range containers {
		if c.State != "running" || !isDebuxDockerSidecar(c) {
			continue
		}
		if session, ok := dockerSessionFromSidecar(c); ok {
			result = append(result, session)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Target < result[j].Target })
	return result, nil
}

func dockerSessionFromSidecar(c container.Summary) (DebugSessionInfo, bool) {
	debugName := dockerContainerPrimaryName(c)
	targetName := c.Labels[dockerLabelTargetName]
	if targetName == "" {
		if targetID := c.Labels[dockerLabelTargetID]; targetID != "" {
			targetName = shortContainerID(targetID)
		}
	}
	if targetName == "" && strings.HasPrefix(debugName, "debux-") {
		targetName = strings.TrimPrefix(debugName, "debux-")
	}
	if targetName == "" {
		return DebugSessionInfo{}, false
	}

	image := c.Labels[dockerLabelDebugImage]
	if image == "" {
		image = c.Image
	}
	return DebugSessionInfo{
		Runtime:   "docker",
		Kind:      DebugSessionKindDockerSidecar,
		Target:    "docker://" + targetName,
		Name:      targetName,
		DebugName: debugName,
		Source:    c.Labels[dockerLabelTargetImage],
		Image:     image,
		User:      c.Labels[dockerLabelDebugUser],
		Status:    c.Status,
	}, true
}

// DockerImages returns locally available Docker image references.
func DockerImages(ctx context.Context) ([]ImageInfo, error) {
	cli, err := dockerclient.New()
	if err != nil {
		return nil, fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	imageList, err := cli.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}

	seen := make(map[string]struct{})
	var result []ImageInfo
	for _, img := range imageList.Items {
		shortID := shortImageID(img.ID)
		added := false
		for _, ref := range img.RepoTags {
			if ref == "" || ref == "<none>:<none>" {
				continue
			}
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			result = append(result, ImageInfo{Ref: ref, ID: shortID, Containers: img.Containers})
			added = true
		}
		if !added && shortID != "" {
			if _, ok := seen[shortID]; ok {
				continue
			}
			seen[shortID] = struct{}{}
			result = append(result, ImageInfo{Ref: shortID, ID: shortID, Containers: img.Containers})
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Ref < result[j].Ref })
	return result, nil
}

// DockerKill force-removes the debux sidecar container for the given target.
func DockerKill(ctx context.Context, targetName string) error {
	cli, err := dockerclient.New()
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	containerID := ""
	resolvedName := targetName
	if targetInfo, err := inspectDockerContainer(ctx, cli, targetName); err == nil {
		containerID = targetInfo.ID
		resolvedName = strings.TrimPrefix(targetInfo.Name, "/")
	}

	debugID, debugName, err := findDockerDebugContainer(ctx, cli, containerID, resolvedName, dockerAnyDebugUser, dockerAnyDebugImage)
	if err != nil {
		return err
	}
	if debugID == "" {
		return fmt.Errorf("no running debux session found for %s", targetName)
	}
	if _, err := cli.ContainerRemove(ctx, debugID, client.ContainerRemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("removing container %q: %w", debugName, err)
	}
	fmt.Printf("Killed debug session for %s (%s)\n", targetName, debugName)
	return nil
}

// DockerKillAll force-removes all running debux sidecar containers.
func DockerKillAll(ctx context.Context) error {
	cli, err := dockerclient.New()
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	containers, err := listDockerContainers(ctx, cli)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	killed := 0
	for _, c := range containers {
		if c.State != "running" || !isDebuxDockerSidecar(c) {
			continue
		}
		name := dockerContainerPrimaryName(c)
		if _, err := cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			fmt.Printf("Warning: failed to kill %s: %v\n", name, err)
			continue
		}
		fmt.Printf("Killed %s\n", name)
		killed++
	}

	if killed == 0 {
		fmt.Println("No running debux sessions found")
	} else {
		fmt.Printf("Killed %d debug session(s)\n", killed)
	}
	return nil
}

func listDockerContainers(ctx context.Context, cli *client.Client) ([]container.Summary, error) {
	list, err := cli.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func inspectDockerContainer(ctx context.Context, cli *client.Client, name string) (container.InspectResponse, error) {
	info, err := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return info.Container, nil
}

func dockerContainerPrimaryName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return shortContainerID(c.ID)
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func isDebuxDockerSidecar(c container.Summary) bool {
	if c.Labels[dockerLabelManagedBy] == dockerLabelManagedByVal && c.Labels[dockerLabelKind] == dockerLabelKindSidecar {
		return true
	}
	// Backward-compatible legacy detection: old debux sidecars were named
	// debux-<target> but had no labels. Require the image reference to mention
	// debux so an unrelated user container named debux-* is not removed/listed.
	name := dockerContainerPrimaryName(c)
	return strings.HasPrefix(name, "debux-") && !strings.HasPrefix(name, "debux-image-") && strings.Contains(strings.ToLower(c.Image), "debux")
}

func isDebuxDockerManagedContainer(c container.Summary) bool {
	if c.Labels[dockerLabelManagedBy] == dockerLabelManagedByVal && isDebuxDockerManagedKind(c.Labels[dockerLabelKind]) {
		return true
	}
	return isDebuxDockerSidecar(c)
}

func removeDockerContainerNameIfManaged(ctx context.Context, cli *client.Client, name string) error {
	info, err := inspectDockerContainer(ctx, cli, name)
	if err != nil {
		return nil
	}
	managed := false
	if info.Config != nil {
		labels := info.Config.Labels
		managed = labels[dockerLabelManagedBy] == dockerLabelManagedByVal && isDebuxDockerManagedKind(labels[dockerLabelKind])
		name := strings.TrimPrefix(info.Name, "/")
		managed = managed || (strings.HasPrefix(name, "debux-") && !strings.HasPrefix(name, "debux-image-") && strings.Contains(strings.ToLower(info.Config.Image), "debux"))
	}
	if !managed {
		return fmt.Errorf("container name %q is already in use by a container not managed by debux", name)
	}
	_, err = cli.ContainerRemove(ctx, info.ID, client.ContainerRemoveOptions{Force: true})
	return err
}

func isDebuxDockerManagedKind(kind string) bool {
	switch kind {
	case dockerLabelKindSidecar, dockerLabelKindImageMode, dockerLabelKindImageTarget, dockerLabelKindStoppedCopy:
		return true
	default:
		return false
	}
}

func findDockerDebugContainer(ctx context.Context, cli *client.Client, targetID, targetName, requestedUser, requestedImage string) (id, name string, err error) {
	containers, err := listDockerContainers(ctx, cli)
	if err != nil {
		return "", "", fmt.Errorf("listing containers: %w", err)
	}
	for _, c := range containers {
		if c.State != "running" || !isDebuxDockerSidecar(c) {
			continue
		}
		if !dockerDebugContainerUserMatches(c, requestedUser) {
			continue
		}
		if !dockerDebugContainerImageMatches(c, requestedImage) {
			continue
		}
		if targetID != "" && c.Labels[dockerLabelTargetID] == targetID {
			return c.ID, dockerContainerPrimaryName(c), nil
		}
		if targetName != "" && c.Labels[dockerLabelTargetName] == targetName && (targetID == "" || c.Labels[dockerLabelTargetID] == "") {
			return c.ID, dockerContainerPrimaryName(c), nil
		}
	}

	// Legacy fallback by container name for sessions created before labels.
	if targetName != "" && (requestedUser == "" || requestedUser == dockerAnyDebugUser) {
		legacyName := "debux-" + targetName
		if info, err := inspectDockerContainer(ctx, cli, legacyName); err == nil && info.State != nil && info.State.Running {
			if info.Config != nil && info.Config.Labels[dockerLabelManagedBy] == dockerLabelManagedByVal {
				return "", "", nil
			}
			if info.Config == nil || strings.Contains(strings.ToLower(info.Config.Image), "debux") {
				return info.ID, strings.TrimPrefix(info.Name, "/"), nil
			}
		}
	}
	return "", "", nil
}

func dockerDebugContainerUserMatches(c container.Summary, requestedUser string) bool {
	if requestedUser == dockerAnyDebugUser {
		return true
	}
	if c.Labels == nil {
		return requestedUser == ""
	}
	user, ok := c.Labels[dockerLabelDebugUser]
	if !ok {
		return requestedUser == ""
	}
	return user == requestedUser
}

// dockerDebugContainerImageMatches reports whether a sidecar was built from the
// requested debug image. Legacy sidecars without the label match any image so
// they keep being reused after an upgrade.
func dockerDebugContainerImageMatches(c container.Summary, requestedImage string) bool {
	if requestedImage == dockerAnyDebugImage {
		return true
	}
	if c.Labels == nil {
		return true
	}
	image, ok := c.Labels[dockerLabelDebugImage]
	if !ok || image == "" {
		return true
	}
	return image == requestedImage
}

// dockerClientForTarget connects to the daemon a target asks for: the podman
// socket for podman:// targets, the docker-CLI-selected daemon otherwise.
func dockerClientForTarget(target *Target) (*client.Client, error) {
	if target != nil && target.PreferPodman {
		return dockerclient.NewForPodman()
	}
	return dockerclient.New()
}

// resolveComposeContainer maps a compose project/service to a running
// container via the labels docker compose stamps on everything it starts.
func resolveComposeContainer(ctx context.Context, cli *client.Client, project, service string) (string, error) {
	containers, err := listDockerContainers(ctx, cli)
	if err != nil {
		return "", fmt.Errorf("listing containers: %w", err)
	}
	var matches []container.Summary
	for _, c := range containers {
		if c.State != "running" || c.Labels == nil {
			continue
		}
		if c.Labels["com.docker.compose.service"] != service {
			continue
		}
		if project != "" && c.Labels["com.docker.compose.project"] != project {
			continue
		}
		matches = append(matches, c)
	}
	scope := "service " + service
	if project != "" {
		scope = fmt.Sprintf("service %s in project %s", service, project)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no running container found for compose %s; check `docker compose ps`", scope)
	case 1:
		return dockerContainerPrimaryName(matches[0]), nil
	default:
		sort.SliceStable(matches, func(i, j int) bool {
			return dockerContainerPrimaryName(matches[i]) < dockerContainerPrimaryName(matches[j])
		})
		name := dockerContainerPrimaryName(matches[0])
		fmt.Printf("Compose %s has %d replicas; using %s (target a container by name to pick another)\n", scope, len(matches), name)
		return name, nil
	}
}

// DockerExec launches a debug sidecar sharing namespaces with the target container.
// The sidecar runs in daemon mode (tail -f /dev/null) and persists between sessions,
// matching K8s ephemeral container behavior. Interactive shells are started via exec.
func DockerExec(ctx context.Context, target *Target, opts DebugOpts) error {
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
		fmt.Printf("Resolved compose service %q to container %q\n", target.ComposeService, name)
		resolved := *target
		resolved.Name = name
		target = &resolved
	}

	// Verify the target container exists and decide how to attach to it.
	targetInfo, err := inspectDockerContainer(ctx, cli, target.Name)
	if err != nil {
		return fmt.Errorf("inspecting target container %q: %w", target.Name, err)
	}
	switch {
	case targetInfo.State == nil:
		return fmt.Errorf("target container %q has no state information", target.Name)
	case targetInfo.State.Restarting:
		// A crash-looping container's namespaces vanish on every restart;
		// debug a stable copy of its filesystem instead.
		return dockerExecCopy(ctx, cli, targetInfo, target, opts)
	case !targetInfo.State.Running:
		return dockerExecCopy(ctx, cli, targetInfo, target, opts)
	case targetInfo.State.Paused:
		fmt.Printf("Note: target container %q is paused; its processes are frozen but filesystem and namespaces remain inspectable.\n", target.Name)
	}

	targetID := targetInfo.ID
	targetName := strings.TrimPrefix(targetInfo.Name, "/")
	targetImage := ""
	if targetInfo.Config != nil {
		targetImage = targetInfo.Config.Image
	}
	containerName := fmt.Sprintf("debux-%s", targetName)

	// Try to reuse an existing running debux sidecar for this exact target
	// container. Labels avoid reusing stale sessions after a target container is
	// recreated with the same name.
	if !opts.Fresh {
		if existingID, existingName, err := findDockerDebugContainer(ctx, cli, targetID, targetName, opts.User, opts.Image); err != nil {
			return err
		} else if existingID != "" {
			fmt.Printf("Reusing debug container %q\n", existingName)
			fmt.Printf("Debugging %s (container: %s)\n", target.Name, existingName)
			return execInContainer(ctx, cli, existingID, opts.Command)
		}
		if existingID, existingName, err := findDockerDebugContainer(ctx, cli, targetID, targetName, opts.User, dockerAnyDebugImage); err != nil {
			return err
		} else if existingID != "" {
			fmt.Printf("Existing debug session %q uses a different image; replacing it with %s\n", existingName, opts.Image)
		} else if otherID, otherName, err := findDockerDebugContainer(ctx, cli, targetID, targetName, dockerAnyDebugUser, dockerAnyDebugImage); err != nil {
			return err
		} else if otherID != "" {
			return fmt.Errorf("running debux session %q for %s uses a different --user; pass --fresh to replace it", otherName, target.Name)
		}
	}

	// Ensure debug image is available
	if err := dbximage.EnsureImageWithPolicy(ctx, cli, opts.Image, opts.PullPolicy); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}

	// Ensure persistent Nix volumes for this exact debug image. The image-specific
	// names avoid mounting an old /nix/store over a rebuilt image, which can make
	// /bin/sh point at a missing store path before the container starts.
	nixVolumes, err := debugImageVolumes(ctx, cli, opts.Image)
	if err != nil {
		return err
	}
	if err := store.EnsureVolumes(ctx, cli, nixVolumes); err != nil {
		return fmt.Errorf("ensuring store volumes: %w", err)
	}

	config := &container.Config{
		Image:      opts.Image,
		Entrypoint: []string{"/bin/sh", "-c", entrypoint.Script},
		Tty:        true,
		Labels: map[string]string{
			dockerLabelManagedBy:   dockerLabelManagedByVal,
			dockerLabelKind:        dockerLabelKindSidecar,
			dockerLabelTargetID:    targetID,
			dockerLabelTargetName:  targetName,
			dockerLabelTargetImage: targetImage,
			dockerLabelDebugImage:  opts.Image,
			dockerLabelDebugUser:   opts.User,
		},
		Env: []string{
			"HOME=/root",
			"ZDOTDIR=/tmp",
			fmt.Sprintf("DEBUX_TARGET=%s", target.Name),
			fmt.Sprintf("DEBUX_TARGET_ID=%s", targetID),
			"DEBUX_TARGET_ROOT=/proc/1/root",
			"DEBUX_DAEMON=1",
		},
	}
	extraEnv, err := debugExtraEnv(opts.Env, opts.Tools)
	if err != nil {
		return err
	}
	config.Env = append(config.Env, extraEnv...)

	// Share IPC when possible: join host IPC if the target uses it, join the
	// target when its IPC is shareable, otherwise keep a private namespace.
	ipcMode := container.IpcMode(fmt.Sprintf("container:%s", targetID))
	if targetInfo.HostConfig != nil {
		switch m := targetInfo.HostConfig.IpcMode; {
		case m == "host":
			ipcMode = "host"
		case m != "" && m != "shareable":
			ipcMode = "private"
		}
	}

	hostConfig := &container.HostConfig{
		NetworkMode: container.NetworkMode(fmt.Sprintf("container:%s", targetID)),
		PidMode:     container.PidMode(fmt.Sprintf("container:%s", targetID)),
		IpcMode:     ipcMode,
		CapAdd:      append([]string{"SYS_PTRACE"}, normalizeCapabilities(opts.CapAdd)...),
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: nixVolumes.NixStore,
				Target: "/nix/store",
			},
			{
				Type:   mount.TypeVolume,
				Source: nixVolumes.NixVar,
				Target: "/nix/var",
			},
		},
		Privileged: opts.Privileged,
	}

	// Share target container's volumes
	if opts.ShareVolumes {
		shared := targetMounts(targetInfo, opts.ReadOnlyVolumes)
		if len(shared) > 0 {
			fmt.Printf("Sharing %d volume(s) from %s\n", len(shared), targetName)
			hostConfig.Mounts = append(hostConfig.Mounts, shared...)
		}
	}

	if opts.User != "" {
		config.User = opts.User
	}

	// Remove any existing debux-managed container with the same name. If a user
	// has an unrelated container named debux-<target>, leave it alone and fail
	// with a clear conflict instead of deleting it.
	if err := removeDockerContainerNameIfManaged(ctx, cli, containerName); err != nil {
		return err
	}

	fmt.Printf("Creating debug container for %s...\n", target.Name)

	resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     config,
		HostConfig: hostConfig,
		Name:       containerName,
	})
	if err != nil {
		return fmt.Errorf("creating debug container: %w", err)
	}

	// Start the sidecar in daemon mode (entrypoint does setup, then waits).
	// Clean up with an uncancelled context so a Ctrl-C between create and
	// start does not leak the created container.
	if _, err := cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), resp.ID, client.ContainerRemoveOptions{Force: true})
		return fmt.Errorf("starting debug container: %w", err)
	}

	// Show entrypoint output (volumes, warnings)
	showEntrypointOutput(ctx, cli, resp.ID)

	fmt.Printf("Debugging %s (container: %s)\n", target.Name, containerName)

	return execInContainer(ctx, cli, resp.ID, opts.Command)
}

func debugImageVolumes(ctx context.Context, cli *client.Client, imageRef string) (store.VolumeSet, error) {
	info, err := cli.ImageInspect(ctx, imageRef)
	if err != nil {
		return store.VolumeSet{}, fmt.Errorf("inspecting debug image %q: %w", imageRef, err)
	}
	return store.VolumesForImage(info.ID), nil
}

// stdioIsTTY reports whether both stdin and stdout are terminals; only then
// does debux allocate a remote TTY, mirroring docker/kubectl CLI behavior so
// piped output is not CRLF-mangled and stderr stays separate.
func stdioIsTTY() bool {
	_, stdinIsTerminal := term.GetFdInfo(os.Stdin)
	_, stdoutIsTerminal := term.GetFdInfo(os.Stdout)
	return stdinIsTerminal && stdoutIsTerminal
}

// runInteractiveContainer attaches to a created container, starts it, streams
// I/O (raw terminal mode and TTY resize when stdio is a terminal), waits for
// it to exit, and propagates the container's exit status.
func runInteractiveContainer(ctx context.Context, cli *client.Client, containerID string, tty, autoRemove bool) error {
	hijacked, err := cli.ContainerAttach(ctx, containerID, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("attaching to container: %w", err)
	}
	defer hijacked.Close()

	// Register the wait before starting so a container that exits (and, with
	// --rm, is auto-removed) immediately cannot slip past the wait.
	cond := container.WaitConditionNextExit
	if autoRemove {
		cond = container.WaitConditionRemoved
	}
	waitResult := cli.ContainerWait(ctx, containerID, client.ContainerWaitOptions{Condition: cond})

	if _, err := cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	stdinFd, _ := term.GetFdInfo(os.Stdin)
	if tty {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
		resizeTTY(ctx, cli, containerID, stdinFd)
	}

	outputDone := make(chan error, 1)
	go func() {
		var err error
		if tty {
			_, err = io.Copy(os.Stdout, hijacked.Reader)
		} else {
			_, err = stdcopy.StdCopy(os.Stdout, os.Stderr, hijacked.Reader)
		}
		outputDone <- err
	}()

	go func() {
		_, _ = io.Copy(hijacked.Conn, os.Stdin)
		// Propagate stdin EOF so piped/CI sessions can finish instead of
		// hanging until SIGKILL.
		_ = hijacked.CloseWrite()
	}()

	select {
	case err := <-waitResult.Error:
		if err != nil {
			return fmt.Errorf("waiting for container: %w", err)
		}
		return nil
	case status := <-waitResult.Result:
		// Let the output goroutine flush the tail before the deferred
		// RestoreTerminal runs.
		select {
		case <-outputDone:
		case <-time.After(2 * time.Second):
		}
		if status.Error != nil && status.Error.Message != "" {
			return fmt.Errorf("waiting for container: %s", status.Error.Message)
		}
		if status.StatusCode != 0 {
			return &ExitError{Code: int(status.StatusCode)}
		}
		return nil
	case <-ctx.Done():
		select {
		case <-outputDone:
		case <-time.After(2 * time.Second):
		}
		return ctx.Err()
	}
}

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

	fmt.Printf("Creating target container from %s...\n", imageRef)
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
		_, _ = cli.ContainerRemove(context.Background(), targetID, client.ContainerRemoveOptions{Force: true})
	}()

	// Stream the entire target filesystem
	fmt.Printf("Copying filesystem from %s...\n", imageRef)
	copyResult, err := cli.CopyFromContainer(ctx, targetID, client.CopyFromContainerOptions{SourcePath: "/"})
	if err != nil {
		return fmt.Errorf("copying filesystem from target: %w", err)
	}
	tarReader := copyResult.Content
	defer func() { _ = tarReader.Close() }()

	// Ensure debug image and image-specific Nix volumes.
	if err := dbximage.EnsureImage(ctx, cli, opts.DebugImage); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}
	nixVolumes, err := debugImageVolumes(ctx, cli, opts.DebugImage)
	if err != nil {
		return err
	}
	if err := store.EnsureVolumes(ctx, cli, nixVolumes); err != nil {
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
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: nixVolumes.NixStore,
				Target: "/nix/store",
			},
			{
				Type:   mount.TypeVolume,
				Source: nixVolumes.NixVar,
				Target: "/nix/var",
			},
		},
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
			_, _ = cli.ContainerRemove(context.Background(), debugID, client.ContainerRemoveOptions{Force: true})
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

	fmt.Printf("Debugging image %s (container: %s)\n", imageRef, debugName)

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
	fmt.Printf("Target container %q is %s; debugging a copy of its filesystem (changes outside volumes are discarded on exit).\n", targetName, status)

	copyResult, err := cli.CopyFromContainer(ctx, targetID, client.CopyFromContainerOptions{SourcePath: "/"})
	if err != nil {
		return fmt.Errorf("copying filesystem from target: %w", err)
	}
	tarReader := copyResult.Content
	defer func() { _ = tarReader.Close() }()

	if err := dbximage.EnsureImageWithPolicy(ctx, cli, opts.Image, opts.PullPolicy); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}
	nixVolumes, err := debugImageVolumes(ctx, cli, opts.Image)
	if err != nil {
		return err
	}
	if err := store.EnsureVolumes(ctx, cli, nixVolumes); err != nil {
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
			// The chroot fallback and env import read the target environment
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
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: nixVolumes.NixStore, Target: "/nix/store"},
			{Type: mount.TypeVolume, Source: nixVolumes.NixVar, Target: "/nix/var"},
		},
		AutoRemove: true,
		Privileged: opts.Privileged,
		CapAdd:     normalizeCapabilities(opts.CapAdd),
	}
	if opts.ShareVolumes {
		shared := targetMountsAt(targetInfo, opts.ReadOnlyVolumes, "/target")
		if len(shared) > 0 {
			fmt.Printf("Mounting %d volume(s) from %s under /target\n", len(shared), targetName)
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
		_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), debugID, client.ContainerRemoveOptions{Force: true})
	}()

	if err := mkdirViaTar(ctx, cli, debugID, "target"); err != nil {
		return fmt.Errorf("creating /target directory: %w", err)
	}
	fmt.Printf("Copying filesystem from %s...\n", targetName)
	if _, err := cli.CopyToContainer(ctx, debugID, client.CopyToContainerOptions{DestinationPath: "/target", Content: tarReader}); err != nil {
		return fmt.Errorf("copying filesystem to debug container: %w", err)
	}
	if err := copyTargetEnvironFile(ctx, cli, debugID, targetEnv); err != nil {
		return fmt.Errorf("writing target environment file: %w", err)
	}

	fmt.Printf("Debugging %s (container: %s, target root: /target)\n", targetName, debugName)
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

// targetMounts extracts the target container's mounts and converts them to
// mount.Mount entries for the debug container, skipping paths reserved by debux.
func targetMounts(info container.InspectResponse, readOnly bool) []mount.Mount {
	return targetMountsAt(info, readOnly, "")
}

// targetMountsAt is targetMounts with every mount destination re-rooted under
// prefix (used by filesystem-copy mode, where the target root lives at /target).
func targetMountsAt(info container.InspectResponse, readOnly bool, prefix string) []mount.Mount {
	if info.Mounts == nil {
		return nil
	}
	// Paths used by the debug container itself — skip conflicts. /tmp and
	// /root hold the debug shell's own state (ZDOTDIR, HOME); mounting target
	// volumes there breaks sessions on readOnlyRootFilesystem-style targets
	// and writes debux files into target data. The target's /tmp stays
	// reachable via $DEBUX_TARGET_ROOT/tmp.
	reserved := map[string]bool{
		"/nix":       true,
		"/nix/store": true,
		"/nix/var":   true,
		"/tmp":       true,
		"/root":      true,
	}
	var mounts []mount.Mount
	for _, mp := range info.Mounts {
		if reserved[prefix+mp.Destination] {
			continue
		}
		m := mount.Mount{
			Type:     mp.Type,
			Target:   prefix + mp.Destination,
			ReadOnly: readOnly || !mp.RW,
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
		_, _ = cli.ContainerResize(ctx, containerID, client.ContainerResizeOptions{
			Height: uint(size.Height),
			Width:  uint(size.Width),
		})
	}

	// Initial resize
	resize()

	// Watch for terminal resize signals
	sigCh, stopSig := watchSIGWINCH()
	go func() {
		defer stopSig()
		for {
			select {
			case <-sigCh:
				resize()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// execInContainer starts an interactive zsh session inside a running container
// using docker exec, similar to how K8s uses exec into daemon ephemeral containers.
func execInContainer(ctx context.Context, cli *client.Client, containerID string, command []string) error {
	if err := bootstrapDockerShell(ctx, cli, containerID); err != nil {
		return fmt.Errorf("preparing debux shell config: %w", err)
	}

	tty := stdioIsTTY()
	stdinFd, _ := term.GetFdInfo(os.Stdin)

	createOpts := client.ExecCreateOptions{
		Cmd:          debuxExecCommand(command),
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		TTY:          tty,
	}
	if tty {
		// Set the initial size at create time; resizing only after attach
		// races exec start and can leave the session at 80x24.
		if size, err := term.GetWinsize(stdinFd); err == nil && size != nil {
			createOpts.ConsoleSize = client.ConsoleSize{Height: uint(size.Height), Width: uint(size.Width)}
		}
	}

	resp, err := cli.ExecCreate(ctx, containerID, createOpts)
	if err != nil {
		return fmt.Errorf("creating exec session: %w", err)
	}

	hijacked, err := cli.ExecAttach(ctx, resp.ID, client.ExecAttachOptions{
		TTY: tty,
	})
	if err != nil {
		return fmt.Errorf("attaching to exec session: %w", err)
	}
	defer hijacked.Close()

	if tty {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}

		resizeExec := func() {
			size, err := term.GetWinsize(stdinFd)
			if err == nil && size != nil {
				_, _ = cli.ExecResize(ctx, resp.ID, client.ExecResizeOptions{
					Height: uint(size.Height),
					Width:  uint(size.Width),
				})
			}
		}

		sigCh, stopSig := watchSIGWINCH()
		go func() {
			defer stopSig()
			for {
				select {
				case <-sigCh:
					resizeExec()
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	outputDone := make(chan error, 1)
	go func() {
		var err error
		if tty {
			_, err = io.Copy(os.Stdout, hijacked.Reader)
		} else {
			_, err = stdcopy.StdCopy(os.Stdout, os.Stderr, hijacked.Reader)
		}
		outputDone <- err
	}()

	go func() {
		_, _ = io.Copy(hijacked.Conn, os.Stdin)
		// Propagate stdin EOF to the remote shell so piped/CI sessions finish
		// instead of hanging until SIGKILL.
		_ = hijacked.CloseWrite()
	}()

	select {
	case <-outputDone:
	case <-ctx.Done():
		// Wait briefly for the output goroutine to flush remaining data
		// before terminal state is restored by the deferred RestoreTerminal.
		select {
		case <-outputDone:
		case <-time.After(2 * time.Second):
		}
		return ctx.Err()
	}

	inspect, err := cli.ExecInspect(ctx, resp.ID, client.ExecInspectOptions{})
	if err == nil && !inspect.Running && inspect.ExitCode != 0 {
		return &ExitError{Code: inspect.ExitCode}
	}

	return nil
}

func bootstrapDockerShell(ctx context.Context, cli *client.Client, containerID string) error {
	resp, err := cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          []string{"/bin/sh", "-c", entrypoint.ShellBootstrapScript()},
		AttachStdout: true,
		AttachStderr: true,
		TTY:          true,
	})
	if err != nil {
		return fmt.Errorf("creating bootstrap exec: %w", err)
	}

	hijacked, err := cli.ExecAttach(ctx, resp.ID, client.ExecAttachOptions{TTY: true})
	if err != nil {
		return fmt.Errorf("attaching bootstrap exec: %w", err)
	}
	defer hijacked.Close()

	var output bytes.Buffer
	_, copyErr := io.Copy(&output, hijacked.Reader)
	if copyErr != nil {
		return fmt.Errorf("reading bootstrap output: %w", copyErr)
	}

	inspect, err := cli.ExecInspect(ctx, resp.ID, client.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspecting bootstrap exec: %w", err)
	}
	if inspect.ExitCode != 0 {
		details := strings.TrimSpace(output.String())
		if details != "" {
			return fmt.Errorf("bootstrap exited with status %d: %s", inspect.ExitCode, details)
		}
		return fmt.Errorf("bootstrap exited with status %d", inspect.ExitCode)
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

	reader, err := cli.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
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
