---
name: debux-release
description: Use this skill when the user asks to cut, publish, prepare, or automate a Debux release. It runs release checks, uses Commitizen to bump the version/changelog/tag, pushes the release tag and changes, waits for the GitHub Release workflow, and polishes the GitHub release notes.
---

# Debux release skill

This skill publishes a Debux release from the repository root.

Publishing a release pushes a `v*` tag, which triggers `.github/workflows/release.yml`. Only run this skill when the user explicitly asks to make a release or confirms that publishing is OK.

## Preferred command

Run the release helper:

```bash
bash .claude/skills/debux-release/scripts/release.sh
```

The helper does the full safe path:

1. Verifies required tools: `git`, `gh`, `mise`, and `python3`.
2. Verifies GitHub CLI authentication.
3. Requires a clean `main` branch.
4. Fetches `origin/main` and tags, then fast-forwards local `main` if needed.
5. Runs release checks:
   - `mise run tidy` + verifies `go.mod`/`go.sum` stayed unchanged
   - `mise run test`
   - `mise run check`
   - `mise run vulncheck`
   - `mise run release:dry-run`
6. Runs `mise run release:bump` to let Commitizen update `.cz.toml`, `CHANGELOG.md`, commit the bump, and create the annotated tag.
7. Pushes `main` and tags with `mise run release:push`.
8. Waits for the GitHub Actions Release workflow for the new tag.
9. Waits for the GitHub Release to exist.
10. Replaces the GitHub Release body with polished notes generated from the matching `CHANGELOG.md` section.
11. Prints the final release URL.

## Options

Use options only when the user asks for them or when recovering from a failed release:

```bash
bash .claude/skills/debux-release/scripts/release.sh --skip-dry-run
bash .claude/skills/debux-release/scripts/release.sh --skip-checks
bash .claude/skills/debux-release/scripts/release.sh --timeout 5400
bash .claude/skills/debux-release/scripts/release.sh --no-edit-release-notes
```

## Agent workflow

When using this skill:

1. Start by running `git status --short --branch` and `gh auth status` if the user did not already show the repo is ready.
2. Run the helper from the repository root.
3. If the helper exits with "No release needed", report that no eligible conventional commits were found or no new tag was created.
4. If a check fails, stop and summarize the failing command. Do not push anything.
5. If the GitHub Release workflow fails, report the run URL and do not edit release notes.
6. On success, summarize:
   - released tag/version
   - GitHub Release URL
   - whether release notes were updated
   - main workflow result

## Manual fallback

If the helper cannot be used, follow this sequence:

```bash
git status --short --branch
git fetch origin main --tags
git switch main
git pull --ff-only origin main
mise run tidy
git diff --exit-code -- go.mod go.sum
mise run test
mise run check
mise run vulncheck
mise run release:dry-run
mise run release:bump
mise run release:push
```

Then wait for `.github/workflows/release.yml`, verify `gh release view <tag>`, and update the release body with a concise changelog from `CHANGELOG.md`.
