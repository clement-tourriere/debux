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
