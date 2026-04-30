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

Install the latest release binary:

```bash
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/debux/main/install.sh | sh

debux docker://
```

The installer supports Linux/macOS on amd64/arm64 and installs to `~/.local/bin` by default.

```bash
# Pin a version
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/debux/main/install.sh | sh -s -- --version v1.2.3

# Choose another install directory
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/debux/main/install.sh | sh -s -- --bin-dir /usr/local/bin

# Later, check or update from GitHub Releases
debux update --check
debux update
```

For development from source, use [mise](https://mise.jdx.dev):

```bash
git clone https://github.com/clement-tourriere/debux.git
cd debux

mise run install         # Build and copy debux to ~/.local/bin
mise run image-build     # Build ghcr.io/clement-tourriere/debux:latest locally
# For Kubernetes after image changes:
# docker push ghcr.io/clement-tourriere/debux:latest
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

# Explicit kube context in the target
debux k8s://@eks-preprod-01/my-namespace/my-pod/my-container

# Or use --context, useful for context names containing slashes
debux k8s://my-namespace/my-pod --context arn:aws:eks:us-west-2:123:cluster/preprod

# Interactive pod picker
debux k8s://
debux k8s://@eks-preprod-01

# If the pod name is not exact, debux proposes running pods matching the substring
debux k8s://my-namespace/webapp-internal-api
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

Packages are backed by [nixpkgs](https://search.nixos.org/packages).

Persistence model:

- **Docker:** installed tools and shell history live in image-specific Nix volumes, so they survive across Docker sessions without breaking rebuilt debug images.
- **Kubernetes:** ephemeral containers cannot add arbitrary new volumes, so debux cannot mount your local Docker toolbox/history into pods. Reusing the same debug container on the same pod keeps its tools and history; a fresh debug container starts from the debug image.
- **Cross-pod Kubernetes toolbox:** bake common tools into a custom debug image and pass it with `--image`, or rebuild/push the default debug image and use `--pull-policy=Always`.
- **Restricted Kubernetes profile:** `dctl install` works with the current debug image. If you see Nix lock-file permission errors, rebuild/push the image and start a fresh session.

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
| `k8s://@<context>` | Kubernetes | Open the pod picker in a specific kube context. |
| `k8s://@<context>/<pod>` | Kubernetes | Debug a pod in a specific context and that context's namespace. |
| `k8s://@<context>/<namespace>/<pod>` | Kubernetes | Debug a pod in a specific context and namespace. |
| `k8s://@<context>/<namespace>/<pod>/<container>` | Kubernetes | Debug a specific container in a specific context. |

### Common flags

| Flag | Description |
|---|---|
| `--image <image>` | Override the debug image. |
| `--fresh` | Force a new debug container instead of reusing an existing session. |
| `--copy` | Kubernetes: create a copied debug pod instead of an ephemeral container. |
| `--no-volumes` | Do not mount target volumes directly. This is not an isolation boundary if the debug container can access `/proc/1/root`. |
| `--pull-policy <policy>` | Kubernetes image pull policy: `Always`, `IfNotPresent`, `Never`. |
| `--profile <profile>` | Kubernetes security profile: `general`, `baseline`, `restricted`, `netadmin`, `sysadmin`. |
| `--user <uid[:gid]>` | Run the debug container as a specific user. |
| `--kubeconfig <path>` | Override kubeconfig path. |
| `--context <name>` | Kubernetes kube context name. |

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

# Open the documentation
debux docs
debux docs --open
```

## Security model

`debux` is a debugger, not a sandbox. The default Kubernetes profile is intentionally powerful because production debugging often needs process, filesystem, and network visibility.

With the default Kubernetes `general` profile, debux can usually:

- run a root debug process inside the pod;
- use the pod network namespace, so `localhost` is the pod's localhost;
- target the selected container's PID namespace when the runtime supports ephemeral-container targeting;
- list pod processes with tools like `ps`;
- expose the target filesystem through `/proc/1/root` and `$DEBUX_TARGET_ROOT`;
- mount the target container's volumes directly by default;
- use debugging capabilities such as `SYS_PTRACE`, `SYS_ADMIN`, and `SYS_CHROOT`.

That means a default debux session can read secrets and service-account files mounted in the pod and can read/write files that Linux permissions and container capabilities allow.

It does **not** automatically grant:

- root on the Kubernetes node or host filesystem;
- access to other pods' filesystems;
- Kubernetes API permissions beyond the pod's service account and your own RBAC;
- a way to bypass PodSecurity, admission webhooks, seccomp, AppArmor, or runtime policy;
- local Docker toolbox/history persistence inside Kubernetes pods.

`--no-volumes` only disables direct volume mounts into the debug container. It is not a security boundary if the debug container can still access the target via `/proc/1/root`.

RBAC implication: granting a user the ability to update `pods/ephemeralcontainers` and create `pods/exec` is effectively granting the ability to run code inside selected pods. Treat it like production shell access.

## Kubernetes security profiles

| Profile | Purpose |
|---|---|
| `general` | Default. Runs as root and adds practical debugging capabilities such as ptrace/chroot. Best UX, highest access inside the pod. |
| `baseline` | No explicit security context. Useful when cluster policy should decide defaults. Not a non-root guarantee because the image default user is root. |
| `restricted` | Non-root, drops capabilities, runtime default seccomp. Shell startup and `dctl install` work with the current debug image, but deep target integration such as chrooting into the target filesystem is limited by Kubernetes/Linux permissions. |
| `netadmin` | Adds network capabilities for tools like `tcpdump`. |
| `sysadmin` | Privileged debug container. Last resort for deep debugging. |

Examples:

```bash
# Lower-privilege Kubernetes debug shell
debux k8s://prod/api/app --profile=baseline --user 65534:65534

# Full privileged debug shell
debux k8s://prod/api/app --profile=sysadmin
```

Kubernetes note: ephemeral containers cannot add new volumes, so debux cannot
mount your local Docker toolbox/history into arbitrary pods. Reusing the same
debug container on the same pod keeps its installed tools/history; across pods,
bake common tools into a custom debug image and pass it with `--image`.

## Development

```bash
mise run build          # Build CLI
mise run install        # install local dev binary to ~/.local/bin
mise run uninstall      # remove local dev binary from ~/.local/bin
mise run test           # go test ./...
mise run lint           # golangci-lint run
mise run check          # hk checks on all files
mise run fix            # hk fixes on all files
mise run hooks-install  # install hk git hooks with mise integration
mise run image-build    # build debug image
mise run release:bump   # bump version/changelog/tag with Commitizen
mise run docs           # serve docs at http://localhost:8000
mise run docs:open      # open local docs in your browser

debux docs              # print documentation URL
debux docs --open       # open documentation in your browser
```

The repository uses [hk](https://hk.jdx.dev/) for git hooks, [pkl](https://pkl-lang.org/) for hk configuration, and [Commitizen](https://commitizen-tools.github.io/commitizen/) for release bumps. Commitizen is installed by mise via `pipx:commitizen`.

## Releases and distribution

GitHub Releases publish prebuilt `debux` binaries for Linux and macOS on amd64/arm64 using GoReleaser. The one-line installer downloads those release assets and verifies `checksums.txt` when available.

After installation, keep the CLI current with:

```bash
debux update --check
debux update
```

The debug toolbox image is published separately to GHCR:

```text
ghcr.io/clement-tourriere/debux:latest
```

Docker mode pulls it automatically when needed. Kubernetes pulls it from inside the cluster; after changing the image, push it and run with `--fresh --pull-policy=Always`.

## Documentation site

The documentation site lives in [`docs/`](docs/) and is deployed by GitHub Actions on pushes to `main`.

Run it locally with:

```bash
mise run docs
mise run docs:open
# or choose a port
PORT=9000 mise run docs
```

Deployment workflow:

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

### Kubernetes restricted: `dctl install` says permission denied

The pod likely pulled an older debug image whose Nix store was root-only. Rebuild and push the current image, then force Kubernetes to pull it and create a fresh debug container:

```bash
mise run image-build
docker push ghcr.io/clement-tourriere/debux:latest

debux k8s://my-namespace/my-pod \
  --profile=restricted \
  --fresh \
  --pull-policy=Always
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
