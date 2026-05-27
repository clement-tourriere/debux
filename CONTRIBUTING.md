# Contributing to debux

Thanks for helping improve debux! Contributions that make container debugging easier, safer, or better documented are welcome.

## Quick setup

Prerequisites:

- Go, Docker, and Git
- [`mise`](https://mise.jdx.dev/) for the project task runner
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

## Pull request checklist

Before opening a PR, please:

- Keep changes focused and explain the user-facing impact.
- Add or update tests when behavior changes.
- Update `README.md` or `docs/` for user-visible changes.
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
