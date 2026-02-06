# debux

Universal container debugging tool. Debug any container — even distroless, scratch, or minimal images — with a rich, NixOS-powered shell.

Existing solutions like `kubectl debug` or `docker exec` fall short when your container has no shell, no tools, nothing. **debux** launches a sidecar that shares namespaces with the target container and gives you a full debugging environment with persistent package management.

## Features

- **Works on any container** — distroless, scratch, alpine, you name it
- **Rich shell** — zsh with syntax highlighting, autosuggestions, and a colorized prompt
- **40+ pre-installed tools** — curl, htop, strace, vim, jq, tcpdump, nmap, git, and more
- **On-demand packages** — `dctl install python3` or just type a missing command and get prompted to install it
- **Persistent store** — packages survive across sessions via Docker volumes
- **Multi-runtime** — Docker and Kubernetes (containerd planned)
- **Namespace sharing** — see the target's processes, network, and filesystem

## Install

### From source

```bash
git clone https://github.com/clement-tourriere/debux.git
cd debux
make install        # Builds and copies to /usr/local/bin
make image-build    # Build the debug image
```

## Quick start

### Docker

```bash
# Start a target container
docker run -d --name my-app nginx:alpine

# Debug it
debux exec my-app
```

You'll land in a zsh shell with full access to the target's network, processes, and filesystem:

```
[debux] my-app ~ #
```

Even on distroless containers where `docker exec` is useless:

```bash
docker run -d --name distroless gcr.io/distroless/static-debian12
debux exec distroless
```

### Kubernetes

```bash
# Debug a running pod (uses ephemeral containers)
debux exec k8s://my-pod
debux exec k8s://my-namespace/my-pod
debux exec k8s://my-namespace/my-pod/my-container

# Create a standalone debug pod
debux pod -n my-namespace
```

## Usage

### `debux exec`

Debug a running container by attaching a sidecar with shared namespaces.

```
debux exec <target>
```

**Target formats:**

| Format | Runtime |
|---|---|
| `<container>` | Docker (default) |
| `docker://<container>` | Docker |
| `containerd://<container>` | containerd (v0.2) |
| `nerdctl://<container>` | containerd (v0.2) |
| `k8s://<pod>` | Kubernetes (default namespace) |
| `k8s://<namespace>/<pod>` | Kubernetes |
| `k8s://<namespace>/<pod>/<container>` | Kubernetes (specific container) |

**Flags:**

| Flag | Description |
|---|---|
| `--kubeconfig <path>` | Override kubeconfig path |

### `debux pod`

Create a standalone debug pod in Kubernetes.

```
debux pod [flags]
```

**Flags:**

| Flag | Description |
|---|---|
| `-n, --namespace <ns>` | Kubernetes namespace (default: `default`) |
| `--kubeconfig <path>` | Override kubeconfig path |
| `--keep` | Keep the pod after exiting (default: auto-delete) |
| `--host-network` | Use the host network |

### `debux store`

Manage the persistent Nix store.

```bash
debux store info     # Show store volumes and sizes
debux store clean    # Remove all persistent store volumes
```

### Global flags

| Flag | Description |
|---|---|
| `--image <image>` | Override debug image (default: `ghcr.io/clement-tourriere/debux:latest`) |
| `--privileged` | Run debug container in privileged mode |
| `--user <uid:gid>` | Run as a specific user |
| `--rm` | Auto-remove debug container on exit (default: `true`) |

## Inside the debug shell

### Pre-installed tools

The debug image comes with these tools ready to use:

| Category | Tools |
|---|---|
| Network | curl, wget, dig, nmap, tcpdump, nettools, iproute2 |
| Debugging | strace, ltrace, htop, procps |
| Editors | vim |
| Text | jq, less, grep, awk, diff |
| Files | find, file, tree |
| Other | git, openssh |

### Installing more tools

Use `dctl`, the built-in package manager:

```bash
dctl install python3     # Install a package
dctl install go rust     # Install multiple at once
dctl search postgres     # Search available packages
dctl list                # List installed packages
dctl remove python3      # Remove a package
dctl update              # Update package index
```

Packages are backed by [nixpkgs](https://search.nixos.org/packages) — virtually everything is available.

### Command-not-found auto-install

Just type a command that doesn't exist:

```
[debux] my-app ~ # python3
python3: command not found

  Available in nixpkgs. Install with:
    dctl install python3

  Install now? [y/N] y
Installing python3...
Python 3.12.0
>>>
```

### Accessing the target's filesystem

The target container's root filesystem is available at:

```bash
ls $DEBUX_TARGET_ROOT         # Browse target's files
cat $DEBUX_TARGET_ROOT/etc/nginx/nginx.conf

# Or use the shortcut alias
target    # cd into the target's root
```

### Accessing the target's processes and network

Since namespaces are shared, you can directly:

```bash
ps aux                  # See the target's processes
ss -tlnp                # See the target's listening ports
curl localhost:8080      # Hit the target's services
strace -p 1              # Trace the target's PID 1 (may need --privileged)
```

## How it works

### Docker

When you run `debux exec my-container`, debux:

1. Inspects the target container
2. Pulls the debug image if needed
3. Creates/ensures persistent Docker volumes (`debux-nix-store`, `debux-nix-profile`)
4. Launches a sidecar container that shares the target's **PID**, **network**, and **IPC** namespaces
5. Mounts the persistent Nix volumes so installed packages survive across sessions
6. Attaches your terminal to the sidecar's zsh shell

### Kubernetes

**Ephemeral containers** (`debux exec k8s://...`): Patches the target pod to add an ephemeral debug container that shares the target container's PID namespace. Attaches via SPDY.

**Standalone pods** (`debux pod`): Creates a new pod with the debug image. Useful for cluster-level debugging or when ephemeral containers aren't available. Auto-deleted on exit unless `--keep` is set.

### Persistence

Packages installed with `dctl` persist across debug sessions:

- **Docker**: Named volumes `debux-nix-store` and `debux-nix-profile`
- Run `debux store info` to see volume sizes
- Run `debux store clean` to reclaim space

## Build

```bash
make build          # Build static binary → bin/debux
make build-dev      # Build with CGO (for development)
make image-build    # Build the debug Docker image
make image-push     # Push image to ghcr.io
make dev            # Build both binary and image
make test           # Run tests
make lint           # Run go vet
make clean          # Remove build artifacts
```

## Roadmap

- [x] Docker runtime
- [x] Kubernetes runtime (ephemeral containers + standalone pods)
- [x] Persistent Nix store via Docker volumes
- [x] NixOS debug image with dctl, zsh, command-not-found handler
- [ ] containerd runtime
- [ ] Port forwarding
- [ ] Remote Docker host support
- [ ] Homebrew formula

## License

MIT
