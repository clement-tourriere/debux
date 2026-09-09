# Security Policy

`debux` is a debugging tool, not a sandbox. A successful debug session is expected to provide strong visibility into the selected target container or pod.

## Supported versions

Security fixes target the latest released version and `main`. Please upgrade to the latest release before reporting an issue that may already be fixed.

## Reporting a vulnerability

Please do **not** open a public issue for vulnerabilities.

Use GitHub's private vulnerability reporting flow:

<https://github.com/clement-tourriere/debux/security/advisories/new>

Include as much detail as possible:

- affected debux version and platform
- Docker or Kubernetes runtime/version
- exact command and flags used
- target image or pod security context, if relevant
- reproduction steps and observed impact

## Good security reports

Examples of useful reports include:

- unexpected host or node access beyond the documented security model
- privilege escalation caused by debux behavior
- release, installer, update, checksum, or image-signing issues
- unsafe defaults that create surprising access to target data

## Expected behavior

By design, debux may expose the target filesystem, process namespace, network namespace, mounted secrets, and service-account files that the selected debug profile can access. See the README security model before reporting behavior that may be intentional.

## Trust model and operational risks

- Docker debug sessions persist installed tools and shell history in Debux-managed Docker volumes. Treat those volumes as trusted state and run `debux store clean` if you want to discard installed tools.
- The installer and self-updater require release checksums. When `cosign` is available, they also require and verify `checksums.txt` signatures with GitHub OIDC identity checks (unless checksum-only verification is explicitly requested).
- The debug image is signed with keyless cosign in release workflows. Verify images before using them in high-trust environments.
- Kubernetes e2e scripts operate on the current kube-context and delete their newly created test namespace on exit. They refuse existing namespaces and arbitrary namespace names unless `DEBUX_E2E_ALLOW_ARBITRARY_NAMESPACE=1` is set. Run them only in an isolated test cluster.
- Malformed Debux/Docker context configuration fails closed instead of silently selecting a higher-privilege profile or another daemon. Bypassing Debux config requires explicit `DEBUX_CONFIG=/dev/null`.
- Automatic reuse verifies creation-option metadata. `attach` intentionally keeps the exact existing session's privileges; changed defaults do not reduce an already running container's privileges.
- In-place debugging and file copying reject pod/host-shared PID namespaces rather than assuming PID 1 is the selected application's root. Copy mode starts new workload/init processes, may share persistent volumes, and is not a filesystem snapshot.
- Go vulnerabilities and toolbox-image vulnerabilities are separate CI checks. The weekly Toolbox security workflow inventories Wolfi/APK packages with Grype/Syft. Both architectures of the exact publication archive are scanned without exclusions; tools installed later with `dctl` still require separate assessment. See the refresh process in `CONTRIBUTING.md`.
