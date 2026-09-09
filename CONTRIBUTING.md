# Contributing to debux

Thanks for helping improve debux! Contributions that make container debugging easier, safer, or better documented are welcome.

## Quick setup

Prerequisites:

- Go 1.26.8+, Docker, Git, zsh, and ShellCheck
- Node.js 20+ and npm for the documentation browser checks
- [`mise`](https://mise.jdx.dev/) for the project task runner and pinned `uv`
  (`uv run --script` selects Python 3.11+ from script metadata; no global Python switch is needed)
- Optional: `kubectl` plus access to a test cluster for Kubernetes changes

```bash
git clone https://github.com/clement-tourriere/debux.git
cd debux

mise install
mise run build
mise run test
```

## Useful commands

```bash
mise run build          # Build the CLI
mise run install        # Build and install to ~/.local/bin
mise run test           # Run Go tests
mise run tidy           # Update go.mod/go.sum
mise run check          # Run tidy diff, tests, lint, and govulncheck
mise run fix            # Auto-fix supported checks
mise run image-build    # Build the debug toolbox image
mise run docs           # Serve docs locally
mise run e2e:docker     # Docker smoke tests
mise run e2e:kubernetes # Kubernetes smoke tests against the current kube-context
```

## Regression and documentation checks

```bash
go test -race ./...
mise exec -- uv run --script scripts/test_release.py
mise exec -- uv run --script scripts/test_image_security.py
shellcheck install.sh scripts/*.sh internal/entrypoint/*.sh images/debug/dctl
zsh -n internal/entrypoint/zshrc
go run ./cmd/debux-docs          # regenerate command/RBAC reference
go run ./cmd/debux-docs --check  # CI drift check
npm ci --prefix docs
(cd docs && npx playwright install chromium && npm test)
```

Python regression scripts use PEP 723 metadata (`requires-python` and dependencies)
so `uv` selects a compatible interpreter even if the system `python3` is older.
Release bumps use `uvx --python 3.11 --from commitizen==4.13.7 cz` rather than a global
Commitizen installation. The Python regression scripts use only the standard library.

Shell behavioral tests execute zsh against temporary fixtures and mock chroot/dctl
commands; they never read a live target or need Docker. Keep the shared
`internal/entrypoint/zshrc` canonical: Go entrypoints, reattach bootstrap, and the
Dockerfile all render it. Do not reintroduce baked/embedded copies.

Docker/kind E2E builds the **candidate** toolbox on PRs. To reproduce safely, build
`debux:e2e`, use `DEBUX_IMAGE=debux:e2e DEBUX_PULL_POLICY=Never`, and, for Kubernetes,
load it into an isolated kind cluster before running the script. E2E refuses to
replace existing fixture namespaces/containers. Docker E2E uses a unique
label-only image identity so it cannot inherit or modify your existing tool
stores; its fixture volumes and containers are removed afterward. Do not point
Kubernetes E2E at production. A manual workflow image override is the
published-image compatibility test.

## Toolbox dependency maintenance

The Wolfi base, APK packages, mise/dbcrust binaries and shell plugins are security
dependencies separate from `go.mod`. Review the weekly Toolbox security workflow
and refresh the pinned base/mise/plugins **at least monthly**, and immediately
for applicable critical/high advisories:

1. Update the base digest and mise/dbcrust versions/checksums in
   `images/debug/Dockerfile`. Update the version smoke assertions too. Verify both architecture hashes
   against the official release; keep plugin sources immutable and hashed.
2. Rebuild without cache to pick up current signed APK package revisions. No
   source compilation or Nix security overlay is required to build the image
   itself. Smoke tests use the prebuilt dbcrust client against a local SQLite
   fixture and verify offline reuse. Only tiny C/C++ linking probes are compiled,
   never database servers. Review the resulting
   inventory and [security boundaries](docs/image-security.md).
3. Build both architectures, run `scripts/test-image.sh`, Docker E2E, and isolated
   kind E2E. Preserve tool aliases, `--tools`, shell behavior and non-root installs.
   Do not move release tags or prune unrelated stores while testing.
4. Run `mise exec -- scripts/scan-image.sh docker:debux:security` for each
   candidate. Keep the SBOM and complete Grype JSON, including lower-severity
   findings. Missing APK metadata and unresolved High/Critical findings fail
   closed; do not blanket-ignore unfixed findings.
5. Publication builds one multiarch OCI archive, validates its exact platform
   images on native amd64/arm64 workers, then copies that archive with digest
   preservation. Both successful scan/smoke receipts and their report hashes
   must match its whole-index digest. Artifacts are passed by immutable IDs,
   never by a mutable image tag. Database-server builds are not a release gate.
   There is no post-scan rebuild or skip-security switch. A passing scan is not
   proof of safety; ad-hoc `dctl` installs need their own review.

## Pull request checklist

Before opening a PR, please:

- Keep changes focused and explain the user-facing impact.
- Add or update tests when behavior changes.
- Update `README.md` or `docs/` for user-visible changes, regenerate CLI/RBAC references, and run the browser checks.
- Run `mise run tidy` after Go dependency changes.
- Run `mise run test` and, when possible, `mise run check`.
- Use conventional commit-style messages when practical, for example `fix: ...`, `feat: ...`, or `docs: ...`.

## Reporting bugs

Please use the bug report template and include:

- `debux version --json`
- OS and architecture
- Docker/Kubernetes versions, depending on the runtime
- The exact `debux ...` command
- Relevant logs or terminal output
- Whether the target image is distroless, scratch, Alpine, or another minimal image

## Security issues

Please do not report vulnerabilities in public issues. See [`SECURITY.md`](SECURITY.md) for private reporting instructions.
