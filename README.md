# debux

Like `docker debug` and `orb debug`, but free and open-source. Works with Kubernetes too.

## Why debux

- **Free and open-source** — no paid Docker or OrbStack subscription required
- **Works on any container** — distroless, scratch, alpine, minimal images where `docker exec` is useless
- **Docker + Kubernetes** — debug Docker containers or Kubernetes pods with the same tool
- **Rich shell** — zsh with 40+ pre-installed tools (curl, strace, vim, tcpdump, ...) and on-demand packages via Nix

## Install

Requires [mise](https://mise.jdx.dev) (handles Go and all build tooling automatically).

```bash
git clone https://github.com/clement-tourriere/debux.git
cd debux
mise run install        # Builds and copies to ~/.local/bin
mise run image-build    # Build the debug image
```

## Quick start

### Docker

```bash
docker run -d --name my-app nginx:alpine
debux exec my-app
```

Even on distroless containers:

```bash
docker run -d --name distroless gcr.io/distroless/static-debian12
debux exec distroless
```

### Kubernetes

```bash
# Debug a running pod (ephemeral container)
debux exec k8s://my-pod
debux exec k8s://my-namespace/my-pod
debux exec k8s://my-namespace/my-pod/my-container

# Standalone debug pod
debux pod -n my-namespace
```

### Interactive picker

Run `debux exec` with no target to get an interactive picker that lists running containers. Use a bare runtime prefix to scope it:

```bash
debux exec              # Pick from Docker containers
debux exec docker://    # Pick from Docker containers
debux exec k8s://       # Pick from Kubernetes pods
```

## Usage

### Target formats

| Format | Runtime |
|---|---|
| `<container>` or `docker://<container>` | Docker |
| `k8s://<pod>` | Kubernetes (default namespace) |
| `k8s://<namespace>/<pod>` | Kubernetes |
| `k8s://<namespace>/<pod>/<container>` | Kubernetes (specific container) |

### `debux exec [flags] <target>`

| Flag | Description |
|---|---|
| `--image <image>` | Override debug image |
| `--privileged` | Run in privileged mode |
| `--user <uid:gid>` | Run as a specific user |
| `--kubeconfig <path>` | Override kubeconfig path |

### `debux pod [flags]`

Create a standalone debug pod in Kubernetes.

| Flag | Description |
|---|---|
| `-n, --namespace <ns>` | Kubernetes namespace (default: `default`) |
| `--keep` | Keep the pod after exiting |
| `--host-network` | Use the host network |

### `debux store`

```bash
debux store info     # Show store volumes and sizes
debux store clean    # Remove all persistent store volumes
```

## Inside the debug shell

### Pre-installed tools

| Category | Tools |
|---|---|
| Network | curl, wget, dig, nmap, tcpdump, nettools, iproute2 |
| Debugging | strace, ltrace, htop, procps |
| Editors | vim |
| Text/Files | jq, less, grep, awk, diff, find, file, tree |
| Other | git, openssh |

### Installing more tools

```bash
dctl install python3     # Install a package
dctl search postgres     # Search available packages
dctl list                # List installed packages
```

Packages are backed by [nixpkgs](https://search.nixos.org/packages) and persist across sessions via Docker volumes.

Just type a missing command and you'll be prompted to install it:

```
[debux] my-app ~ # python3
python3: command not found
  Install now? [y/N] y
```

### Accessing the target

```bash
ls $DEBUX_TARGET_ROOT                           # Target's filesystem
cat $DEBUX_TARGET_ROOT/etc/nginx/nginx.conf
target                                          # cd into the target's root

ps aux                  # Target's processes (shared PID namespace)
curl localhost:8080     # Target's network (shared network namespace)
strace -p 1            # Trace PID 1 (may need --privileged)
```

## License

MIT
