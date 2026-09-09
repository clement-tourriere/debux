# Toolbox image security

Go dependency checks do not cover the toolbox image or tools installed later.
Both Linux architectures must pass the image vulnerability gate and behavioral
smoke tests before publication. A passing gate is not a claim of zero risk.

## Simpler image builds, on-demand runtime tools

`images/debug/Dockerfile` uses a digest-pinned **Wolfi** base with glibc and signed
APK packages, plus checksum-verified **mise** release binaries for amd64/arm64.
System tools are precompiled: there are no Nix closures, Nix databases, local
OpenSSL/Vim rebuilds, source overlays, or Nix-specific CVE exceptions to maintain.
The zsh plugins are fetched from immutable upstream commits, verified by SHA256,
and reduced to their runtime files. `gdb`, `tmux`, and `ggshield` are preinstalled
too. `ggshield` and its packaged Python dependencies come from Wolfi's signed APK
repository on both architectures and are covered by the baked-image scan.
`dbcrust` / `dbc` is a prebuilt database client from checksum-pinned upstream
release archives; no database server is built or shipped for it. The supplied
binary is scanned as part of the image, subject to the scanner's ability to
identify embedded dependencies in opaque binaries. Unused
GDB static development archives are removed in the package-install layer, rather
than hidden in a later layer. Setuid/setgid bits on system helpers are stripped.

A shared C/C++ toolchain, make/CMake/Ninja/autotools and common development
headers let optional source-only backends compile tools without root, sudo, or
modifying the system image. Release checks compile only tiny C/C++ probes to
verify static linking and LTO, not Redis or PostgreSQL servers. Debug information
is removed from selected static dependency archives without removing support
for these operations.

Build support increases the base footprint. Smoke tests enforce a **1280 MiB
uncompressed image budget** for the build-ready toolbox.
Installed/compiled tools and caches in external volumes are separate from it.

The base digest and mise checksums are pinned; APK repositories intentionally
supply current package revisions. This is **not** a promise of byte-for-byte
reproducible builds. Weekly scans rebuild without cache to pick up package fixes.

## Preserve the Debux interface

Users still use `dctl install/remove/search/list/update`, `--tools`, named tool
sets, the command-not-found prompt, and the same zsh/target-binary integration.
Tool names may include an explicit version, such as `yq@4.53.6`; installs record
exact resolved versions. Aliases such as `python3`, `nodejs`, and `rg` are mapped
internally. Python CLIs such as HTTPie use a pinned user-space uv helper,
installed on demand rather than adding it to every image. Explicit `ggshield`
version overrides use the Python backend too, avoiding Aqua's missing Linux
ARM64 binary; the bare command is already supplied by the image. Unversioned installs
reuse the already-installed pinned version, including during `--tools` startup
in fresh matching containers. `dctl update` clears metadata caches, not installed
versions; `dctl install <pkg>@latest` explicitly upgrades a tool. PostgreSQL's
installer is told not to initialize a database automatically.

Mise is **not the nixpkgs catalogue**. The included build prerequisites support
normal binary and source installs without root, but don't promise every possible
OS library/platform combination. Extra OS packages or uncommon build dependencies
can still need image customization. The installer never raises privileges,
runs a fallback package manager silently, or reports an unsupported tool as a
success. Mise's automatic OS-dependency installation and sudo support remain
disabled: common prerequisites are supplied by the image instead. Simply invoking
an installed command never auto-installs missing versions. Later installations
have their own supply-chain and vulnerability risk.

## Storage and target isolation

- System packages and mise live outside writable tool storage. Smoke tests run
  with a read-only root filesystem and verify that installing/removing overrides
  does not change baked binaries.
- Docker mounts one `/var/lib/debux` volume per image/security identity. Changing
  the user, profile, privilege flag, or added capabilities selects a separate
  store. Tools and shell history persist for matching sessions. This is trusted
  user state, not a sandbox for hostile installed tools.
- Root and UID 65534 can use the supplied store; no world-writable permissions or
  privilege changes are needed. Other UIDs may require a purpose-built image.
- Existing Nix images remain explicitly selectable with `--image`; their legacy
  volumes are not migrated, loaded into the new image, or automatically deleted.
  `debux store info/clean` still recognizes both generations.
- Kubernetes persistence remains within the same debug container. For fresh
  containers or other pods, bake shared tools into an image.
- Debux does not activate mise in the target directory. `dctl` and generated
  command launchers resolve configuration from `/`, then restore the caller's
  working directory for the actual command. Smoke tests include hostile
  `mise.toml`, `.tool-versions`, and early `.miserc.toml` files.
- Shared target mounts cannot shadow system tools, trusted configuration, or
  mutable tool storage; those target paths remain accessible through
  `$DEBUX_TARGET_ROOT`.

## Security gate and publication

`scripts/scan-image.sh` inventories once using pinned Syft and scans that same
SBOM with pinned Grype. It requires Wolfi/APK inventory and rejects suppressed
findings. All severities remain in `dist/security/`; unresolved High/Critical
matches fail the build. No blanket `wont-fix`, package, ecosystem, or severity
exclusions are used. Reports are retained even on failure.

The workflow first builds one multiarch **OCI archive**, including BuildKit SBOM
and provenance attestations. Native amd64 and arm64 workers download that same
immutable artifact, verify its whole-index digest, and scan/smoke its matching
platform. They exercise root/restricted installation, removal, persistence,
shell plugins and target-configuration isolation. The baked dbcrust client
queries an isolated SQLite file, as root and UID 65534, without a database server.
Fresh containers repeat the query with networking disabled and check binary
hashes/modification times. The compiler gets small static C/C++/LTO probes;
release checks never build Redis/PostgreSQL servers.

Only after each scan and full smoke succeeds does its worker write a validation
receipt binding the architecture, whole-index digest and report/SBOM hashes.
A dependent job requires both receipts; publishers download the archive and
validated reports by immutable artifact IDs from that successful workflow run.
They recheck the digest, receipts, inventories and vulnerability reports before
any registry copy. Missing, failed, stale or altered evidence fails closed.
Receipts are trusted CI outputs, not signatures or a way to waive tests; never
create them manually to authorize publication. Without supplied receipts,
`scripts/publish-image-archive.sh` runs both architecture gates locally first.
Skopeo copies the exact validated bytes with digest preservation: there is no
post-scan rebuild. Cosign and GitHub provenance attestations refer to the
resulting immutable digest.

These gates cover the baked image, **not** later installs, external volumes,
user-supplied images, or every dependency embedded inside opaque binaries.
See [CONTRIBUTING.md](../CONTRIBUTING.md) for the refresh procedure.
