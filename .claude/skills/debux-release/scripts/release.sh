#!/usr/bin/env bash
set -euo pipefail

repo="${GH_REPO:-clement-tourriere/debux}"
timeout_seconds=3600
skip_checks=0
skip_dry_run=0
edit_release_notes=1

log() {
  printf '[debux-release] %s\n' "$*" >&2
}

die() {
  printf '[debux-release] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: bash .claude/skills/debux-release/scripts/release.sh [options]

Cut and publish a Debux release:
  - verify the repository is clean and on main
  - run release checks
  - run Commitizen release bump
  - push main and tags
  - wait for the GitHub Actions Release workflow
  - update the GitHub Release notes from CHANGELOG.md

Options:
  --repo OWNER/REPO          GitHub repository. Default: clement-tourriere/debux
  --timeout SECONDS         Wait timeout for workflow/release. Default: 3600
  --skip-checks             Skip all pre-release checks. Use only for recovery.
  --skip-dry-run            Run checks but skip GoReleaser snapshot dry-run.
  --no-edit-release-notes   Do not update the GitHub Release body.
  -h, --help                Show this help.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --repo)
      [ "$#" -ge 2 ] || die "--repo requires OWNER/REPO"
      repo="$2"
      shift 2
      ;;
    --timeout)
      [ "$#" -ge 2 ] || die "--timeout requires seconds"
      timeout_seconds="$2"
      shift 2
      ;;
    --skip-checks)
      skip_checks=1
      shift
      ;;
    --skip-dry-run)
      skip_dry_run=1
      shift
      ;;
    --no-edit-release-notes)
      edit_release_notes=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "Unknown option: $1"
      ;;
  esac
done

case "$timeout_seconds" in
  ''|*[!0-9]*) die "--timeout must be a positive integer" ;;
esac
[ "$timeout_seconds" -gt 0 ] || die "--timeout must be a positive integer"

run() {
  log "+ $*"
  "$@"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"
}

require_clean_tree() {
  if [ -n "$(git status --porcelain)" ]; then
    git status --short
    die "Working tree must be clean before releasing"
  fi
}

current_version() {
  awk -F'"' '/^version = / {print $2; exit}' .cz.toml
}

latest_version_tag() {
  git tag --list 'v[0-9]*' --sort=-v:refname | head -n 1
}

previous_version_tag() {
  local tag="$1"
  git tag --list 'v[0-9]*' --sort=-v:refname | grep -v -x "$tag" | head -n 1 || true
}

write_release_notes() {
  local tag="$1"
  local version="$2"
  local previous_tag="$3"
  local notes_file="$4"

  VERSION="$version" TAG="$tag" PREVIOUS_TAG="$previous_tag" REPO="$repo" python3 <<'PY' > "$notes_file"
import os
import re
from pathlib import Path

version = os.environ["VERSION"]
tag = os.environ["TAG"]
previous_tag = os.environ.get("PREVIOUS_TAG", "")
repo = os.environ["REPO"]

changelog = Path("CHANGELOG.md")
text = changelog.read_text(encoding="utf-8") if changelog.exists() else ""

header = re.compile(rf"^##\s+v?{re.escape(version)}\s*(?:\(([^)]*)\))?\s*$", re.MULTILINE)
match = header.search(text)
release_date = ""
section = ""
if match:
    release_date = match.group(1) or ""
    start = match.end()
    next_header = re.search(r"^##\s+", text[start:], re.MULTILINE)
    end = start + next_header.start() if next_header else len(text)
    section = text[start:end].strip()

section_titles = {
    "Feat": "Features",
    "Fix": "Fixes",
    "Perf": "Performance",
    "Refactor": "Refactors",
    "Docs": "Documentation",
    "Doc": "Documentation",
    "Chore": "Maintenance",
    "Ci": "CI",
    "Build": "Build",
    "Test": "Tests",
}

pretty_lines = []
for line in section.splitlines():
    heading = re.match(r"^###\s+(.+?)\s*$", line)
    if heading:
        title = heading.group(1).strip()
        pretty_lines.append("### " + section_titles.get(title, section_titles.get(title.title(), title)))
    else:
        pretty_lines.append(line)
pretty_section = "\n".join(pretty_lines).strip()

compare_url = f"https://github.com/{repo}/compare/{previous_tag}...{tag}" if previous_tag else f"https://github.com/{repo}/releases/tag/{tag}"
image_url = f"https://github.com/{repo}/pkgs/container/debux"

print(f"# debux {tag}")
if release_date:
    print(f"\nReleased on {release_date}.")

print("\n## What's changed\n")
if pretty_section:
    print(pretty_section)
else:
    print("- See the commit history for the changes included in this release.")

print("\n## Install or update\n")
print("```bash")
print("curl -fsSL https://raw.githubusercontent.com/clement-tourriere/debux/main/install.sh | sh")
print("# Or, if debux is already installed:")
print("debux update")
print("```")

print("\n## Container image\n")
print("```text")
print(f"ghcr.io/clement-tourriere/debux:{version}")
print("```")

print("\n## Links\n")
print(f"- Full changelog: {compare_url}")
print("- Documentation: https://clement-tourriere.github.io/debux/")
print(f"- Container image: {image_url}")
PY
}

wait_for_release_run() {
  local tag="$1"
  local tag_sha="$2"
  local deadline=$(( $(date +%s) + timeout_seconds ))
  local run_id=""

  log "Waiting for Release workflow for ${tag}..."
  while [ "$(date +%s)" -lt "$deadline" ]; do
    run_id="$(gh run list \
      --repo "$repo" \
      --workflow release.yml \
      --limit 30 \
      --json databaseId,headBranch,headSha,status,event \
      --jq ".[] | select((.headBranch == \"${tag}\") or (.headSha == \"${tag_sha}\" and .event == \"push\")) | .databaseId" \
      | head -n 1)"

    if [ -n "$run_id" ]; then
      log "Release workflow run: https://github.com/${repo}/actions/runs/${run_id}"
      gh run watch "$run_id" --repo "$repo" --exit-status --interval 20
      return 0
    fi

    sleep 10
  done

  die "Timed out waiting for Release workflow to start for ${tag}"
}

wait_for_github_release() {
  local tag="$1"
  local deadline=$(( $(date +%s) + timeout_seconds ))
  local url=""

  log "Waiting for GitHub Release ${tag}..."
  while [ "$(date +%s)" -lt "$deadline" ]; do
    url="$(gh release view "$tag" --repo "$repo" --json url --jq .url 2>/dev/null || true)"
    if [ -n "$url" ]; then
      printf '%s\n' "$url"
      return 0
    fi
    sleep 10
  done

  die "Timed out waiting for GitHub Release ${tag}"
}

require_command git
require_command gh
require_command mise
require_command python3

run gh auth status

if ! git rev-parse --show-toplevel >/dev/null 2>&1; then
  die "Not inside a Git repository"
fi
cd "$(git rev-parse --show-toplevel)"

[ -f mise.toml ] || die "mise.toml not found; run from the Debux repository"
[ -f .cz.toml ] || die ".cz.toml not found; run from the Debux repository"
[ -f .github/workflows/release.yml ] || die "Release workflow not found"

branch="$(git branch --show-current)"
[ "$branch" = "main" ] || die "Release must be run from main, current branch is '${branch:-detached}'"

require_clean_tree

run git fetch origin main --tags

local_sha="$(git rev-parse HEAD)"
remote_sha="$(git rev-parse origin/main)"
if [ "$local_sha" != "$remote_sha" ]; then
  if git merge-base --is-ancestor HEAD origin/main; then
    run git pull --ff-only origin main
  else
    die "Local main is not a fast-forward of origin/main"
  fi
fi

require_clean_tree

if [ "$skip_checks" -eq 0 ]; then
  run mise run check
  if [ "$skip_dry_run" -eq 0 ]; then
    run mise run release:dry-run
  else
    log "Skipping GoReleaser dry-run because --skip-dry-run was provided"
  fi
  require_clean_tree
else
  log "Skipping pre-release checks because --skip-checks was provided"
fi

before_tag="$(latest_version_tag || true)"
log "Latest tag before bump: ${before_tag:-<none>}"

run mise run release:bump
require_clean_tree

version="$(current_version)"
[ -n "$version" ] || die "Could not read version from .cz.toml"
tag="v${version}"

if ! git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
  die "Expected release tag ${tag} was not created"
fi

tag_sha="$(git rev-list -n 1 "$tag")"
head_sha="$(git rev-parse HEAD)"
if [ "$tag_sha" != "$head_sha" ]; then
  log "No release needed: ${tag} does not point at HEAD."
  log "Tag ${tag}: ${tag_sha}"
  log "HEAD: ${head_sha}"
  exit 0
fi

previous_tag="$(previous_version_tag "$tag")"
log "Release tag: ${tag} (${tag_sha})"
log "Previous tag: ${previous_tag:-<none>}"

run mise run release:push

wait_for_release_run "$tag" "$tag_sha"
release_url="$(wait_for_github_release "$tag")"

if [ "$edit_release_notes" -eq 1 ]; then
  notes_file="$(mktemp)"
  trap 'rm -f "$notes_file"' EXIT
  write_release_notes "$tag" "$version" "$previous_tag" "$notes_file"
  log "Updating GitHub Release notes for ${tag}"
  run gh release edit "$tag" --repo "$repo" --title "debux ${tag}" --notes-file "$notes_file"
else
  log "Skipping release notes update because --no-edit-release-notes was provided"
fi

log "Release complete: ${release_url}"
