## v0.8.3 (2026-06-18)

### Fix

- use boolean do-not-disrupt and add legacy Karpenter annotations

## v0.8.2 (2026-06-16)

### Fix

- improve Kubernetes session lookup and completion

## v0.8.1 (2026-06-15)

### Refactor

- split kubernetes/entrypoint internals and persist TUI options

## v0.8.0 (2026-06-15)

### Feat

- add debux session listing and attach picker

## v0.7.0 (2026-06-11)

### Feat

- add --keep/--ttl copy pod lifecycle with Karpenter protection

## v0.6.0 (2026-06-11)

### Feat

- add forward/cp/node commands, stopped-container debug, and fix review findings

### Fix

- make k8s kill actually terminate the debug daemon
- resolve compose:// targets without opening the picker

## v0.5.3 (2026-05-28)

### Fix

- set usable TERM for target commands

## v0.5.2 (2026-05-27)

### Fix

- harden release and debug session safety

## v0.5.1 (2026-05-26)

### Fix

- remove vulnerable Docker module

## v0.5.0 (2026-05-18)

### Feat

- add live target completions

## v0.4.0 (2026-05-13)

### Feat

- make release skill agent-neutral

## v0.3.0 (2026-05-13)

### Feat

- add full-screen debug TUI
- improve kubernetes debugging safety

## v0.2.3 (2026-04-30)

### Fix

- ignore malformed target environments

## v0.2.2 (2026-04-30)

### Fix

- support updated go dependencies

## v0.2.1 (2026-04-30)

### Fix

- ignore non-release commits during bump

## v0.2.0 (2026-04-30)

### Feat

- polish release safety and debug UX
- add diagnostics and release hardening

## v0.1.0 (2026-04-30)

### Feat

- add self update command
- add one-line install script
- suggest matching pods for partial names
- support kube context in targets
- improve cli help and docs command

### Fix

- recreate missing release tag after interrupted bump
- make release bump non-interactive when no matching tag exists
- make release bump idempotent when there are no new commits
- create annotated release tags for push flow
- make release bump work with tag signing enabled
- fall back to source install before releases
- allow dctl installs in restricted profile
- bootstrap restricted kubernetes shells
- restore rich kubernetes picker
- harden local install task
- use lightweight kubernetes picker
- paginate kubernetes pod listing
- simplify kubernetes picker labels
- avoid repeated install prompt after dctl failure
- isolate docker nix store volumes by image
- harden kubernetes debug UX
