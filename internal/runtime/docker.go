package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/clement-tourriere/debux/internal/dockerclient"
	"github.com/clement-tourriere/debux/internal/entrypoint"
	dbximage "github.com/clement-tourriere/debux/internal/image"
	"github.com/clement-tourriere/debux/internal/store"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
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

// dockerTargetScheme returns the target-URI scheme for the daemon a target
// selects, so podman sessions are recorded and matched as podman://.
func dockerTargetScheme(target *Target) string {
	if target != nil && target.PreferPodman {
		return "podman"
	}
	return "docker"
}

// DockerList returns running containers, excluding debux sidecars. A nil
// target selects the default Docker daemon; a podman:// target its socket.
func DockerList(ctx context.Context, target *Target) ([]ContainerInfo, error) {
	cli, err := dockerClientForTarget(target)
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

// DockerSessions returns running debux sidecar sessions that can be
// reattached. A nil target selects the default Docker daemon; a podman://
// target its socket.
func DockerSessions(ctx context.Context, target *Target) ([]DebugSessionInfo, error) {
	cli, err := dockerClientForTarget(target)
	if err != nil {
		return nil, fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	containers, err := listDockerContainers(ctx, cli)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	scheme := dockerTargetScheme(target)
	var result []DebugSessionInfo
	for _, c := range containers {
		if c.State != "running" || !isDebuxDockerSidecar(c) {
			continue
		}
		if session, ok := dockerSessionFromSidecar(c, scheme); ok {
			result = append(result, session)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Target < result[j].Target })
	return result, nil
}

func dockerSessionFromSidecar(c container.Summary, scheme string) (DebugSessionInfo, bool) {
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
		Target:    scheme + "://" + targetName,
		Name:      targetName,
		DebugName: debugName,
		ID:        c.ID,
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
func DockerKill(ctx context.Context, target *Target) error {
	cli, err := dockerClientForTarget(target)
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	targetName := target.Name
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
	statusf("Killed debug session for %s (%s)\n", targetName, debugName)
	return nil
}

// DockerKillAll force-removes all running debux sidecar containers on the
// daemon the target selects (nil = default Docker daemon).
func DockerKillAll(ctx context.Context, target *Target) error {
	cli, err := dockerClientForTarget(target)
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	containers, err := listDockerContainers(ctx, cli)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	killed := 0
	var failures []error
	for _, c := range containers {
		if c.State != "running" || !isDebuxDockerSidecar(c) {
			continue
		}
		name := dockerContainerPrimaryName(c)
		if _, err := cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			statusf("Warning: failed to kill %s: %v\n", name, err)
			failures = append(failures, fmt.Errorf("removing %s: %w", name, err))
			continue
		}
		statusf("Killed %s\n", name)
		killed++
	}

	if killed == 0 && len(failures) == 0 {
		statusln("No running debux sessions found")
	} else {
		statusf("Killed %d debug session(s)\n", killed)
	}
	return errors.Join(failures...)
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
	if c.Labels[dockerLabelManagedBy] == dockerLabelManagedByVal {
		return false // another managed kind must not fall through to legacy detection
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
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		// A transient daemon error is not "the name is free": surfacing it
		// here beats the raw 409 name-conflict ContainerCreate would return.
		return fmt.Errorf("checking existing container %q: %w", name, err)
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

func findDockerDebugContainer(ctx context.Context, cli *client.Client, targetID, targetName, requestedUser, requestedImage string, optionsHash ...string) (id, name string, err error) {
	containers, err := listDockerContainers(ctx, cli)
	if err != nil {
		return "", "", fmt.Errorf("listing containers: %w", err)
	}
	for _, c := range containers {
		if c.State != "running" || !isDebuxDockerSidecar(c) {
			continue
		}
		if len(optionsHash) > 0 && c.Labels[sessionOptionsLabel] != optionsHash[0] {
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
	if len(optionsHash) == 0 && targetName != "" && (requestedUser == "" || requestedUser == dockerAnyDebugUser) {
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
		statusf("Compose %s has %d replicas; using %s (target a container by name to pick another)\n", scope, len(matches), name)
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
		statusf("Resolved compose service %q to container %q\n", target.ComposeService, name)
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
		statusf("Note: target container %q is paused; its processes are frozen but filesystem and namespaces remain inspectable.\n", target.Name)
	}

	targetID := targetInfo.ID
	targetName := strings.TrimPrefix(targetInfo.Name, "/")
	targetImage := ""
	if targetInfo.Config != nil {
		targetImage = targetInfo.Config.Image
	}
	containerName := fmt.Sprintf("debux-%s", targetName)
	if err := dbximage.EnsureImageWithPolicy(ctx, cli, opts.Image, opts.PullPolicy); err != nil {
		return fmt.Errorf("ensuring debug image: %w", err)
	}
	debugImage, err := cli.ImageInspect(ctx, opts.Image)
	if err != nil {
		return fmt.Errorf("inspecting debug image: %w", err)
	}
	optionsHash := sessionOptionsHash(opts, debugImage.ID, targetInfo.State.StartedAt)
	if opts.PullPolicy == "Always" {
		opts.Fresh = true
	}

	// Try to reuse an existing running debux sidecar for this exact target
	// container. Labels avoid reusing stale sessions after a target container is
	// recreated with the same name.
	if !opts.Fresh {
		if existingID, existingName, err := findDockerDebugContainer(ctx, cli, targetID, targetName, opts.User, opts.Image, optionsHash); err != nil {
			return err
		} else if existingID != "" {
			statusf("Reusing debug container %q\n", existingName)
			statusf("Debugging %s (container: %s)\n", target.Name, existingName)
			return execInContainer(ctx, cli, existingID, opts.Command)
		}
		if existingID, existingName, err := findDockerDebugContainer(ctx, cli, targetID, targetName, opts.User, dockerAnyDebugImage); err != nil {
			return err
		} else if existingID != "" {
			statusf("Existing debug session %q has incompatible or unknown options; replacing it with %s\n", existingName, opts.Image)
		} else if otherID, otherName, err := findDockerDebugContainer(ctx, cli, targetID, targetName, dockerAnyDebugUser, dockerAnyDebugImage); err != nil {
			return err
		} else if otherID != "" {
			return fmt.Errorf("running debux session %q for %s uses a different --user; pass --fresh to replace it", otherName, target.Name)
		}
	}

	// Keep mutable tools outside the image and separate security identities.
	volumes, err := debugImageVolumes(ctx, cli, opts)
	if err != nil {
		return err
	}
	if err := store.EnsureVolumes(ctx, cli, volumes); err != nil {
		return fmt.Errorf("ensuring store volumes: %w", err)
	}

	config := &container.Config{
		Image:      opts.Image,
		Entrypoint: []string{"/bin/sh", "-c", entrypoint.Script},
		Tty:        true,
		Labels: map[string]string{
			dockerLabelManagedBy:   dockerLabelManagedByVal,
			dockerLabelKind:        dockerLabelKindSidecar,
			sessionOptionsLabel:    optionsHash,
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
		Mounts:      volumes.Mounts(),
		Privileged:  opts.Privileged,
	}

	// Share target container's volumes
	if opts.ShareVolumes {
		shared := targetMounts(targetInfo, opts.ReadOnlyVolumes)
		if len(shared) > 0 {
			statusf("Sharing %d volume(s) from %s\n", len(shared), targetName)
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

	statusf("Creating debug container for %s...\n", target.Name)

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
		cleanupResource(ctx, resp.ID, func(cleanupCtx context.Context) error {
			_, err := cli.ContainerRemove(cleanupCtx, resp.ID, client.ContainerRemoveOptions{Force: true})
			return err
		})
		return fmt.Errorf("starting debug container: %w", err)
	}

	// Show entrypoint output (volumes, warnings)
	showEntrypointOutput(ctx, cli, resp.ID)

	statusf("Debugging %s (container: %s)\n", target.Name, containerName)

	return execInContainer(ctx, cli, resp.ID, opts.Command)
}

func debugImageVolumes(ctx context.Context, cli *client.Client, opts DebugOpts) (store.VolumeSet, error) {
	info, err := cli.ImageInspect(ctx, opts.Image)
	if err != nil {
		return store.VolumeSet{}, fmt.Errorf("inspecting debug image %q: %w", opts.Image, err)
	}
	backend := ""
	if info.Config != nil {
		backend = info.Config.Labels["io.debux.toolbox"]
	}
	switch backend {
	case "mise-v1":
		if opts.User == "" && info.Config != nil {
			opts.User = info.Config.User
		}
		return store.ToolVolumesForImage(info.ID, toolStoreScope(opts)), nil
	case "": // Explicit compatibility with existing Nix-based/custom images.
		return store.VolumesForImage(info.ID), nil
	default:
		return store.VolumeSet{}, fmt.Errorf("unsupported toolbox storage format %q; upgrade debux", backend)
	}
}

func toolStoreScope(opts DebugOpts) string {
	user := opts.User
	if user == "" || user == "root" {
		user = "0"
	}
	return sessionOptionsHash(DebugOpts{User: user, Profile: opts.Profile,
		Privileged: opts.Privileged, CapAdd: opts.CapAdd})
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
	var mounts []mount.Mount
	for _, mp := range info.Mounts {
		if isReservedDebugMountPath(prefix + mp.Destination) {
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
