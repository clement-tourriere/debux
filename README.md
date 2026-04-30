# debux

<p align="center">
  <strong>Debug any container — even distroless, scratch, and minimal images — with a rich Nix-powered shell.</strong>
</p>

<p align="center">
  <a href="https://github.com/clement-tourriere/debux/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/clement-tourriere/debux/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/clement-tourriere/debux/actions/workflows/docker.yml"><img alt="Docker" src="https://github.com/clement-tourriere/debux/actions/workflows/docker.yml/badge.svg"></a>
  <a href="https://github.com/clement-tourriere/debux/actions/workflows/pages.yml"><img alt="Docs" src="https://github.com/clement-tourriere/debux/actions/workflows/pages.yml/badge.svg"></a>
  <a href="./LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
</p>

<p align="center">
  <a href="https://clement-tourriere.github.io/debux/"><strong>Read the docs</strong></a>
  ·
  <a href="#quick-start">Quick start</a>
  ·
  <a href="#kubernetes">Kubernetes</a>
  ·
  <a href="#troubleshooting">Troubleshooting</a>
</p>

---

`debux` is like `docker debug` and `orb debug`, but free, open-source, and Kubernetes-aware.

It starts a temporary debug toolbox next to your target container, shares useful namespaces, and exposes the target filesystem at `$DEBUX_TARGET_ROOT`. That means you can debug production-style containers without rebuilding them, adding a shell, or shipping troubleshooting tools in your app image.

📚 **Full documentation:** <https://clement-tourriere.github.io/debux/> — includes a `Ctrl`/`Cmd` + `K` search palette.

## Why debux?

- **Works when `docker exec` is useless** — distroless, scratch, Alpine, and tiny production images.
- **Docker + Kubernetes** — same workflow locally and in clusters.
- **Nix-powered shell** — zsh plus tools like `curl`, `strace`, `tcpdump`, `vim`, `jq`, `dig`, `nmap`, and more.
- **Install tools on demand** — `dctl install <pkg>` pulls from nixpkgs during a debug session.
- **Target-aware shell** — jump into the target root, inspect target processes, and reuse the target network namespace.
- **Open source** — no paid Docker Desktop or OrbStack subscription required.

## Quick start

Requires [mise](https://mise.jdx.dev) for Go and developer tooling.

```bash
git clone https://github.com/clement-tourriere/debux.git
cd debux

mise run install         # Build and copy debux to ~/.local/bin
mise run image-build     # Build ghcr.io/clement-tourriere/debux:latest locally
```

### Docker

```bash
docker run -d --name my-app nginx:alpine

debux my-app
# or
debux docker://my-app
```

Interactive picker:

```bash
debux docker://
```

Even if the target image has no shell:

```bash
docker run -d --name distroless gcr.io/distroless/static-debian12

debux distroless
```

### Kubernetes

```bash
# Current kube-context namespace
debux k8s://my-pod

# Explicit namespace
debux k8s://my-namespace/my-pod

# Specific container in a multi-container pod
debux k8s://my-namespace/my-pod/my-container

# Interactive pod picker
debux k8s://
```

If ephemeral containers are blocked by RBAC or admission policy:

```bash
debux k8s://my-namespace/my-pod --copy
```

Force a fresh debug container and pull the newest debug image:

```bash
debux k8s://my-namespace/my-pod/my-container \
  --fresh \
  --pull-policy=Always
```

## How it works

`debux` does **not** modify your application image.

1. It starts a debug container using the debux Nix toolbox image.
2. It joins the target's useful namespaces: network and process namespaces where supported.
3. It exposes the target filesystem at:

```bash
$DEBUX_TARGET_ROOT
# usually /proc/1/root
```

Inside the debug shell:

```bash
target                              # cd into the target filesystem
ls $DEBUX_TARGET_ROOT/etc
ps aux                              # target processes
curl localhost:8080                 # target network namespace
strace -p 1                         # trace target PID 1, may require more privileges
```

## Inside the debug shell

### Pre-installed tools

| Category | Tools |
|---|---|
| Network | `curl`, `wget`, `dig`, `nmap`, `tcpdump`, `nettools`, `iproute2` |
| Debugging | `strace`, `ltrace`, `htop`, `procps` |
| Editors | `vim` |
| Text/files | `jq`, `less`, `grep`, `awk`, `diff`, `find`, `file`, `tree` |
| Other | `git`, `openssh`, `zsh` |

### Install more tools with `dctl`

```bash
dctl search postgres
dctl install postgresql
dctl list
```

If a command is missing, the shell offers to install it:

```text
[debux] my-app ~ # python3
python3: command not found

  Install with: dctl install python3

  Install now? [y/N]
```

Packages are backed by [nixpkgs](https://search.nixos.org/packages). Docker mode uses image-specific persistent Nix volumes so tools can survive across sessions without breaking rebuilt debug images.

## Usage

### Target formats

| Format | Runtime | Meaning |
|---|---|---|
| `<container>` | Docker | Debug a Docker container by name or ID. |
| `docker://` | Docker | Open the Docker picker. |
| `docker://<container>` | Docker | Debug a Docker container. |
| `k8s://` | Kubernetes | Open the pod picker in the current kube-context namespace. |
| `k8s://<pod>` | Kubernetes | Debug a pod in the current kube-context namespace. |
| `k8s://<namespace>/<pod>` | Kubernetes | Debug a pod in an explicit namespace. |
| `k8s://<namespace>/<pod>/<container>` | Kubernetes | Debug a specific container. |

### Common flags

| Flag | Description |
|---|---|
| `--image <image>` | Override the debug image. |
| `--fresh` | Force a new debug container instead of reusing an existing session. |
| `--copy` | Kubernetes: create a copied debug pod instead of an ephemeral container. |
| `--no-volumes` | Do not share target volumes with the debug container. |
| `--pull-policy <policy>` | Kubernetes image pull policy: `Always`, `IfNotPresent`, `Never`. |
| `--profile <profile>` | Kubernetes security profile: `general`, `baseline`, `restricted`, `netadmin`, `sysadmin`. |
| `--user <uid[:gid]>` | Run the debug container as a specific user. |
| `--kubeconfig <path>` | Override kubeconfig path. |

### Standalone Kubernetes debug pod

```bash
debux pod -n my-namespace

debux pod -n my-namespace --host-network

debux pod -n my-namespace --keep
```

### Debug an image without starting it

Useful when the image itself cannot boot.

```bash
debux image gcr.io/distroless/static-debian12

debux image my-app:broken
```

The image filesystem is copied into the debug container and exposed at `/target`.

### Manage debux sessions and stores

```bash
# Kill a Docker or Kubernetes debug session
debux kill docker://my-app
debux kill k8s://my-namespace/my-pod

# Kill all sessions in the selected runtime
debux kill --all
debux kill k8s://my-namespace/ --all

# Inspect or clean persistent Docker Nix stores
debux store info
debux store clean
```

## Kubernetes security profiles

| Profile | Purpose |
|---|---|
| `general` | Default. Adds practical debugging capabilities such as ptrace/chroot. |
| `baseline` | No extra security context. Useful for stricter clusters. |
| `restricted` | Non-root, drops capabilities, runtime default seccomp. |
| `netadmin` | Adds network capabilities for tools like `tcpdump`. |
| `sysadmin` | Privileged debug container. Last resort for deep debugging. |

Examples:

```bash
# Lower-privilege Kubernetes debug shell
debux k8s://prod/api/app --profile=baseline --user 65534:65534

# Full privileged debug shell
debux k8s://prod/api/app --profile=sysadmin
```

## Development

```bash
mise run build          # Build CLI
mise run test           # go test ./...
mise run lint           # golangci-lint run
mise run check          # hk checks on all files
mise run fix            # hk fixes on all files
mise run hooks-install  # install hk git hooks with mise integration
mise run image-build    # build debug image
```

The repository uses [hk](https://hk.jdx.dev/) for git hooks and [pkl](https://pkl-lang.org/) for hk configuration.

## Documentation site

The documentation site lives in [`docs/`](docs/) and is deployed by GitHub Actions on pushes to `main`:

```text
.github/workflows/pages.yml
```

If this is the first deployment for a fork or new repository, enable **GitHub Pages → Source: GitHub Actions** in repository settings.

## Troubleshooting

### Kubernetes: `openat etc/passwd: path escapes from parent`

Your cluster runtime rejected debug images with NixOS-style absolute symlinks in `/etc/passwd` or `/etc/group`. Rebuild and push the latest debux image, then force Kubernetes to pull it:

```bash
mise run image-build
docker push ghcr.io/clement-tourriere/debux:latest

debux k8s://my-namespace/my-pod --fresh --pull-policy=Always
```

### Docker: `exec: "/bin/sh": stat /bin/sh: no such file or directory`

This is usually a stale Nix store volume mounted over a rebuilt debug image. Recent debux versions use image-specific volumes. Upgrade and clean old stores if needed:

```bash
mise run install
debux store clean
debux docker://my-container --fresh
```

### Ephemeral containers denied

Your Kubernetes RBAC or admission policy may block `pods/ephemeralcontainers` or the selected security profile.

Try:

```bash
debux k8s://my-namespace/my-pod --copy
# or
debux k8s://my-namespace/my-pod --profile=baseline
```

### File permissions look broad inside debux

The target filesystem is shown as-is through `/proc/1/root`. If files are `777` inside debux, they are likely `777` in the target image or mounted volume too.

Verify with:

```bash
kubectl exec -n my-namespace my-pod -- \
  stat -c '%A %a %u:%g %n' /app /app/manage.py
```

## License

MIT — see [`LICENSE`](LICENSE).
