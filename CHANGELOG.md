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
