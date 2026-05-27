# debux

<p align="center">
  <strong>Debug any container — even distroless, scratch, and minimal images — with a rich Nix-powered shell.</strong>
</p>

<p align="center">
  <a href="https://github.com/clement-tourriere/debux/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/clement-tourriere/debux/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/clement-tourriere/debux/actions/workflows/docker.yml"><img alt="Docker" src="https://github.com/clement-tourriere/debux/actions/workflows/docker.yml/badge.svg"></a>
  <a href="https://github.com/clement-tourriere/debux/actions/workflows/pages.yml"><img alt="Docs" src="https://github.com/clement-tourriere/debux/actions/workflows/pages.yml/badge.svg"></a>
  <a href="https://github.com/clement-tourriere/debux/releases"><img alt="Latest release" src="https://img.shields.io/github/v/release/clement-tourriere/debux?sort=semver"></a>
  <a href="https://github.com/clement-tourriere/debux/stargazers"><img alt="GitHub stars" src="https://img.shields.io/github/stars/clement-tourriere/debux"></a>
  <a href="./LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
</p>

<p align="center">
  <a href="https://clement-tourriere.github.io/debux/"><strong>Read the docs</strong></a>
  ·
  <a href="#why-debux">Why debux?</a>
  ·
  <a href="#quick-start">Quick start</a>
  ·
  <a href="#kubernetes">Kubernetes</a>
  ·
  <a href="#troubleshooting">Troubleshooting</a>
  ·
  <a href="#community">Community</a>
</p>

---

`debux` is like `docker debug` and `orb debug`, but free, open-source, and Kubernetes-aware.

It starts a temporary debug toolbox next to your target container, shares useful namespaces, and exposes the target filesystem at `$DEBUX_TARGET_ROOT`. That means you can debug production-style containers without rebuilding them, adding a shell, or shipping troubleshooting tools in your app image.

📚 **Full documentation:** <https://clement-tourriere.github.io/debux/> — includes a `Ctrl`/`Cmd` + `K` search palette.

If `debux` saves you a debugging session, a GitHub star helps other Docker and Kubernetes users find it.

## Why debux?

- **Works when `docker exec` is useless** — distroless, scratch, Alpine, and tiny production images.
- **Docker + Kubernetes** — same workflow locally and in clusters.
- **Nix-powered shell** — zsh plus tools like `curl`, `strace`, `tcpdump`, `vim`, `jq`, `dig`, `nmap`, and more.
- **Install tools on demand** — `dctl install <pkg>` pulls from nixpkgs during a debug session.
- **Target-aware shell** — jump into the target root, inspect target processes, and reuse the target network namespace.
- **Open source** — no paid Docker Desktop or OrbStack subscription required.

## When to use it

- You have a running container or pod, but the image has no shell or package manager.
- You need incident-response tools without rebuilding or bloating production images.
- You want one debugger for local Docker containers and Kubernetes workloads.
- You need a free, open-source alternative to desktop-only container debugging features.

## How debux is different

| Usual option | Where it falls short | Debux approach |
|---|---|---|
| `docker exec` | Requires tools and a shell inside the target image. | Starts a separate toolbox and attaches it to the target. |
| `kubectl debug` | Kubernetes-only, and you still need to curate a debug image. | Provides one Docker + Kubernetes workflow with a Nix toolbox. |
| Rebuilding the app image | Slow during incidents and changes the artifact you are debugging. | Leaves the application image untouched. |
| Shipping debug tools in prod | Increases image size and attack surface. | Keeps production images minimal and installs tools on demand. |

## Quick start

Install the latest release binary:

```bash
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/debux/main/install.sh | sh

debux docker://
```

The installer supports Linux/macOS on amd64/arm64 and installs to `~/.local/bin` by default.
Release assets are checksum-verified. If `cosign` is installed and the release includes signature assets, the installer also verifies `checksums.txt` before using it.

```bash
# Pin a version
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/debux/main/install.sh | sh -s -- --version v1.2.3

# Choose another install directory
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/debux/main/install.sh | sh -s -- --bin-dir /usr/local/bin

# Explicitly opt into a source build if a release asset is unavailable
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/debux/main/install.sh | DEBUX_ALLOW_SOURCE_BUILD=1 sh

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

Full-screen target browser with Docker, Kubernetes context/namespace navigation, recent sessions, option toggles, and optional new-terminal launch support:

```bash
debux tui
# keys: / filter, enter open/drill down, ←/→ or tab cycle sources,
#       d/k/y jump to Docker/Kubernetes/History,
#       b back, s search pods, r reload
```

The dashboard presents source sections first. Kubernetes pods are loaded only after you pick a context and namespace, and the Kubernetes view includes a current context/default namespace shortcut so common cases are one click. `enter` opens in the current terminal and returns to the TUI when the shell exits. External launch with `t` is disabled unless you explicitly set `DEBUX_TERMINAL`.

Even if the target image has no shell:

```bash
docker run -d --name distroless gcr.io/distroless/static-debian12

debux distroless
```

### Kubernetes

```bash
# Current kube-context namespace
debux k8s://my-pod

# Explicit namespace in the target or with --namespace/-n
debux k8s://my-namespace/my-pod
debux k8s://my-pod --namespace my-namespace

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

Copy mode creates a temporary duplicate pod for debugging and deletes it on exit.
Use it carefully for workloads with side effects or non-idempotent startup logic.

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
- **Pinned runtime installs:** `dctl install` uses the same pinned nixpkgs revision as the debug image unless you override `NIXPKGS_REF` in a custom image.

## Usage

### Target formats

| Format | Runtime | Meaning |
|---|---|---|
| `<container>` | Docker | Debug a Docker container by name or ID. |
| `docker://` | Docker | Open the Docker picker. |
| `docker://<container>` | Docker | Debug a Docker container. |
| `k8s://` | Kubernetes | Open the pod picker in the current kube-context namespace. |
| `k8s://<pod>` | Kubernetes | Debug a pod in the current kube-context namespace. |
| `k8s://<namespace>/<pod>` | Kubernetes | Debug a pod in an explicit namespace (or use `--namespace` / `-n`). |
| `k8s://<pod>/<container> --namespace <namespace>` | Kubernetes | Debug a specific container using the namespace flag. |
| `k8s://<namespace>/<pod>/<container>` | Kubernetes | Debug a specific container. |
| `k8s://@<context>` | Kubernetes | Open the pod picker in a specific kube context. |
| `k8s://@<context>/<pod>` | Kubernetes | Debug a pod in a specific context and that context's namespace. |
| `k8s://@<context>/<namespace>/<pod>` | Kubernetes | Debug a pod in a specific context and namespace. |
| `k8s://@<context>/<namespace>/<pod>/<container>` | Kubernetes | Debug a specific container in a specific context. |

### Shell completion

Generated completions include live Docker and Kubernetes targets:

- `docker://` suggests running containers; `--image` and `debux image` suggest local images.
- `k8s://` suggests kube contexts, the default namespace, and pods from the selected namespace.
- `k8s://<namespace>/` suggests pods in that namespace; `k8s://<namespace>/<pod>/` suggests containers.
- Kubernetes pod completion is scoped and cached for speed. Typing 3+ characters also matches substrings, so `k8s://inte<Tab>` can find `webapp-internal-api-...`.

```bash
# zsh example
debux completion zsh > ~/.zfunc/_debux

# If you are replacing an older zsh completion script
rm -f ~/.zcompdump*
exec zsh
```

Use `debux completion <bash|zsh|fish|powershell>` for other shells.

### Common flags

| Flag | Description |
|---|---|
| `--image <image>` | Override the debug image. |
| `--fresh` | Force a new debug container instead of reusing an existing session. |
| `--copy` | Kubernetes: create a copied debug pod instead of an ephemeral container. |
| `--no-volumes` | Do not mount target volumes directly. This is not an isolation boundary if the debug container can access `/proc/1/root`. |
| `--read-only-volumes` | Mount target volumes read-only in the debug container to reduce accidental writes. This is not a security boundary if `/proc/1/root` is accessible. |
| `--pull-policy <policy>` | Debug image pull policy for Docker/Kubernetes: `Always`, `IfNotPresent`, `Never`. |
| `--profile <profile>` | Kubernetes security profile: `general`, `baseline`, `restricted`, `netadmin`, `sysadmin`. |
| `--user <uid[:gid]>` | Run the debug container as a specific user. |
| `--kubeconfig <path>` | Override kubeconfig path. |
| `--context <name>` | Kubernetes kube context name. |
| `-n, --namespace <name>` | Kubernetes namespace for pod pickers, pod targets without an inline namespace, `kill`, and `doctor`. |

### Standalone Kubernetes debug pod

```bash
debux pod                    # current kube-context namespace
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
debux kill k8s://my-pod --namespace my-namespace

# Kill all sessions in the selected runtime
debux kill --all
debux kill k8s://my-namespace/ --all
debux kill --all --namespace my-namespace

# Inspect or clean persistent Docker Nix stores
debux store info
debux store clean

# Run a one-shot command through the debug toolbox
debux docker://my-app -- curl -I localhost
debux k8s://my-namespace/my-pod/app -- ps aux

# Browse Docker, Kubernetes, and recent sessions in the full-screen TUI
debux tui

# Diagnose local Docker/Kubernetes readiness
debux doctor
debux doctor --strict
debux doctor k8s://my-namespace/my-pod --profile=restricted
debux doctor k8s://my-pod --namespace my-namespace

# Version and release metadata
debux version
debux version --json

# Generate shell completions
debux completion zsh

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

`--no-volumes` only disables direct volume mounts into the debug container, and `--read-only-volumes` makes those direct mounts read-only. Neither is a security boundary if the debug container can still access the target via `/proc/1/root`.

Docker mode persists Nix tools and shell history in Debux-managed Docker volumes. Treat those volumes as trusted debug-session state: tools installed with `dctl` can affect later sessions using the same debug image, and `debux store clean` removes that state.

RBAC implication: granting a user the ability to update `pods/ephemeralcontainers` and create `pods/exec` is effectively granting the ability to run code inside selected pods. Treat it like production shell access.

Minimal namespace-scoped RBAC for ephemeral-container debugging:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: debux-debugger
  namespace: prod
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "create"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["pods/ephemeralcontainers"]
    verbs: ["update"]
```

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

Example custom team toolbox:

```Dockerfile
FROM ghcr.io/clement-tourriere/debux:latest
ARG NIXPKGS_REF=github:NixOS/nixpkgs/1c3fe55ad329cbcb28471bb30f05c9827f724c76
RUN NIX_CONFIG="experimental-features = nix-command flakes" \
    nix profile add --profile /nix/var/debux-profile \
      "${NIXPKGS_REF}#postgresql" \
      "${NIXPKGS_REF}#redis" \
      "${NIXPKGS_REF}#kubectl" \
      "${NIXPKGS_REF}#ripgrep"
```

```bash
docker build -t ghcr.io/my-org/debux-toolbox:latest -f Dockerfile.debux .
docker push ghcr.io/my-org/debux-toolbox:latest

debux k8s://prod/api --image ghcr.io/my-org/debux-toolbox:latest
```

## Development

```bash
mise run build          # Build CLI
mise run install        # install local dev binary to ~/.local/bin
mise run uninstall      # remove local dev binary from ~/.local/bin
mise run test           # go test ./...
mise run tidy           # go mod tidy
mise run lint           # golangci-lint run
mise run vulncheck      # govulncheck with the project allowlist
mise run check          # tidy diff, tests, lint, and govulncheck
mise run fix            # hk fixes on all files
mise run hooks-install  # install hk git hooks with mise integration
mise run image-build    # build debug image
mise run release:bump   # bump version/changelog/tag with Commitizen
mise run release:dry-run # build release artifacts locally without publishing
mise run release:push   # push main + tags to trigger GitHub release
mise run e2e:docker     # run Docker end-to-end smoke tests
mise run e2e:kubernetes # run Kubernetes e2e against the current kube-context
mise run docs           # serve docs at http://localhost:8000
mise run docs:open      # open local docs in your browser

debux docs              # print documentation URL
debux docs --open       # open documentation in your browser
```

The repository uses [hk](https://hk.jdx.dev/) for git hooks, [pkl](https://pkl-lang.org/) for hk configuration, and [Commitizen](https://commitizen-tools.github.io/commitizen/) for release bumps. Commitizen is installed by mise via `pipx:commitizen`.

`mise run e2e:kubernetes` creates and deletes a namespace in your current kube-context. By default it only manages namespaces matching `debux-e2e-*`; set `DEBUX_E2E_ALLOW_ARBITRARY_NAMESPACE=1` only when you intentionally want to override that guard.

## Releases and distribution

GitHub Releases publish prebuilt `debux` binaries for Linux and macOS on amd64/arm64 using GoReleaser. The one-line installer and `debux update` require `checksums.txt` and refuse to install unverifiable release assets. New releases also publish keyless cosign signatures for `checksums.txt`; clients verify them automatically when `cosign` is available.

Release flow:

```bash
mise run release:bump      # updates .cz.toml/changelog and creates a vX.Y.Z tag
mise run release:push      # git push origin main --follow-tags
```

If there are no commits since the latest version tag, or if the commits are not release-eligible conventional commits such as `ci:`/`docs:`, `mise run release:bump` exits successfully and tells you no bump is needed. If Commitizen already bumped the version but tag creation was interrupted, the task recreates the missing `vX.Y.Z` tag without GPG signing.

Pushing a valid `vX.Y.Z` tag reachable from `main` runs `.github/workflows/release.yml`, publishes the debug image to GHCR as `X.Y.Z`, `X.Y`, `X`, and `latest`, signs the image with keyless cosign, then creates the GitHub Release with signed checksums and CLI archives. Release binaries pin their default debug image to the matching `X.Y.Z` image tag; development builds keep using `latest`. You can also run the **Release** workflow manually for an existing pushed tag.

Verify a released image:

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/clement-tourriere/debux/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/clement-tourriere/debux:0.2.0
```

If `HOMEBREW_TAP_GITHUB_TOKEN` is configured, GoReleaser also publishes a Homebrew formula to `clement-tourriere/homebrew-tap`.

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

## Community

- Questions, ideas, and usage help: [GitHub Discussions](https://github.com/clement-tourriere/debux/discussions).
- Bugs: [open an issue](https://github.com/clement-tourriere/debux/issues/new/choose) with the generated template.
- Security reports: [open a private security advisory](https://github.com/clement-tourriere/debux/security/advisories/new) — see [`SECURITY.md`](SECURITY.md).
- If Debux is useful to you, consider [starring the repo](https://github.com/clement-tourriere/debux/stargazers).

## License

MIT — see [`LICENSE`](LICENSE).
