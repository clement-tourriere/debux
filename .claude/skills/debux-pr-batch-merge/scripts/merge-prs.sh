#!/usr/bin/env bash
set -euo pipefail

repo="${GH_REPO:-clement-tourriere/debux}"
dry_run=0
skip_local_checks=0

authors_csv=""
requested_numbers_csv=""
selected_file=""
selection_input_file=""
refs_file=""
worktree_dir=""
merged_file=""

log() {
  printf '[debux-pr-batch-merge] %s\n' "$*" >&2
}

die() {
  printf '[debux-pr-batch-merge] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh [options] [PR_NUMBER ...]

Merge a safe batch of Debux pull requests with gh CLI.

Default behavior:
  - operate on open Dependabot PRs targeting main
  - require a clean local main branch
  - simulate the combined squash merge locally
  - run test/check/vulncheck on the simulated result
  - squash-merge the ready PRs one by one with gh

Options:
  --repo OWNER/REPO      GitHub repository. Default: clement-tourriere/debux
  --author LOGIN         Filter open PRs by author login. Repeatable.
  --dry-run              Validate and simulate the batch, but do not merge remotely.
  --skip-local-checks    Skip local mise test/check/vulncheck after simulation.
  -h, --help             Show this help.

Examples:
  bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh
  bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh --dry-run
  bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh 14 15 18
  bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh --author app/dependabot
EOF
}

cleanup() {
  if [ -n "$worktree_dir" ] && [ -d "$worktree_dir" ]; then
    git worktree remove --force "$worktree_dir" >/dev/null 2>&1 || rm -rf "$worktree_dir"
  fi

  if [ -n "$refs_file" ] && [ -f "$refs_file" ]; then
    while IFS= read -r ref; do
      [ -n "$ref" ] || continue
      git update-ref -d "$ref" >/dev/null 2>&1 || true
    done < "$refs_file"
  fi

  rm -f "$selected_file" "$selection_input_file" "$refs_file" "$merged_file"
}

trap cleanup EXIT

append_csv() {
  local value="$1"
  local current="$2"
  if [ -z "$current" ]; then
    printf '%s' "$value"
  else
    printf '%s,%s' "$current" "$value"
  fi
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --repo)
      [ "$#" -ge 2 ] || die "--repo requires OWNER/REPO"
      repo="$2"
      shift 2
      ;;
    --author)
      [ "$#" -ge 2 ] || die "--author requires a login"
      authors_csv="$(append_csv "$2" "$authors_csv")"
      shift 2
      ;;
    --dry-run)
      dry_run=1
      shift
      ;;
    --skip-local-checks)
      skip_local_checks=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      while [ "$#" -gt 0 ]; do
        case "$1" in
          ''|*[!0-9]*) die "Invalid PR number: $1" ;;
          *) requested_numbers_csv="$(append_csv "$1" "$requested_numbers_csv")" ;;
        esac
        shift
      done
      ;;
    -* )
      die "Unknown option: $1"
      ;;
    *)
      case "$1" in
        ''|*[!0-9]*) die "Invalid PR number: $1" ;;
        *) requested_numbers_csv="$(append_csv "$1" "$requested_numbers_csv")" ;;
      esac
      shift
      ;;
  esac
done

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
    die "Working tree must be clean before batch merging PRs"
  fi
}

sync_local_main() {
  run git fetch origin main --tags

  local local_sha remote_sha
  local_sha="$(git rev-parse HEAD)"
  remote_sha="$(git rev-parse origin/main)"
  if [ "$local_sha" != "$remote_sha" ]; then
    if git merge-base --is-ancestor HEAD origin/main; then
      run git pull --ff-only origin main
    else
      die "Local main is not a fast-forward of origin/main"
    fi
  fi
}

select_prs() {
  selection_input_file="$(mktemp)"
  selected_file="$(mktemp)"

  run gh pr list \
    --repo "$repo" \
    --state open \
    --limit 100 \
    --json number,title,url,author,isDraft,mergeStateStatus,baseRefName,statusCheckRollup,headRefOid,reviewDecision \
    > "$selection_input_file"

  PRS_FILE="$selection_input_file" \
  SELECTED_FILE="$selected_file" \
  REQUESTED_NUMBERS="$requested_numbers_csv" \
  AUTHORS="$authors_csv" \
  python3 <<'PY'
import json
import os
import sys

prs_path = os.environ["PRS_FILE"]
selected_path = os.environ["SELECTED_FILE"]
requested = [int(x) for x in os.environ.get("REQUESTED_NUMBERS", "").split(",") if x]
authors = [x for x in os.environ.get("AUTHORS", "").split(",") if x]

with open(prs_path, "r", encoding="utf-8") as fh:
    prs = json.load(fh)

if not requested and not authors:
    authors = ["app/dependabot"]

prs_by_number = {pr["number"]: pr for pr in prs}

if requested:
    missing = [str(n) for n in requested if n not in prs_by_number]
    if missing:
        print(
            "[debux-pr-batch-merge] ERROR: Requested PRs are not open in the repository: " + ", ".join(missing),
            file=sys.stderr,
        )
        sys.exit(1)
    ordered = [prs_by_number[n] for n in requested]
else:
    ordered = sorted(prs, key=lambda pr: pr["number"])

ok_conclusions = {"SUCCESS", "NEUTRAL", "SKIPPED"}
selected = []
skipped = []

def check_name(item):
    return (
        item.get("name")
        or item.get("context")
        or item.get("workflowName")
        or item.get("__typename")
        or "check"
    )

for pr in ordered:
    reasons = []
    author = (pr.get("author") or {}).get("login", "unknown")

    if authors and author not in authors:
        reasons.append(f"author={author}")
    if pr.get("baseRefName") != "main":
        reasons.append(f"base={pr.get('baseRefName')}")
    if pr.get("isDraft"):
        reasons.append("draft")
    if pr.get("reviewDecision") == "CHANGES_REQUESTED":
        reasons.append("changes requested")

    merge_state = pr.get("mergeStateStatus")
    if merge_state != "CLEAN":
        reasons.append(f"mergeState={merge_state or 'UNKNOWN'}")

    checks = pr.get("statusCheckRollup") or []
    if not checks:
        reasons.append("no checks reported")
    else:
        pending = []
        failing = []
        for check in checks:
            name = check_name(check)
            status = check.get("status")
            conclusion = check.get("conclusion")
            if status != "COMPLETED":
                pending.append(name)
            elif conclusion not in ok_conclusions:
                failing.append(f"{name}={conclusion or 'UNKNOWN'}")
        if pending:
            reasons.append("pending checks: " + ", ".join(pending))
        if failing:
            reasons.append("failing checks: " + ", ".join(failing))

    if reasons:
        skipped.append((pr, reasons))
    else:
        selected.append(pr)

if selected:
    print("[debux-pr-batch-merge] Ready PRs:", file=sys.stderr)
    for pr in selected:
        author = (pr.get("author") or {}).get("login", "unknown")
        print(
            f"  - #{pr['number']} {pr['title']} [{author}] {pr['url']}",
            file=sys.stderr,
        )
else:
    print("[debux-pr-batch-merge] No ready PRs matched the selection.", file=sys.stderr)

if skipped:
    print("[debux-pr-batch-merge] Skipped PRs:", file=sys.stderr)
    for pr, reasons in skipped:
        print(
            f"  - #{pr['number']} {pr['title']}: " + "; ".join(reasons),
            file=sys.stderr,
        )

if requested and skipped:
    print(
        "[debux-pr-batch-merge] ERROR: At least one explicitly requested PR is not ready.",
        file=sys.stderr,
    )
    sys.exit(1)

with open(selected_path, "w", encoding="utf-8") as out:
    for pr in selected:
        title = pr["title"].replace("\t", " ").replace("\n", " ")
        out.write(f"{pr['number']}\t{pr['headRefOid']}\t{title}\n")
PY
}

simulate_batch() {
  refs_file="$(mktemp)"
  worktree_dir="$(mktemp -d)"

  run git worktree add --quiet --detach "$worktree_dir" origin/main

  while IFS=$'\t' read -r number _ title; do
    [ -n "$number" ] || continue
    local_ref="refs/remotes/origin/debux-pr-batch-merge/${number}"

    log "Fetching PR #${number}"
    run git fetch --force origin "pull/${number}/head:${local_ref}"
    printf '%s\n' "$local_ref" >> "$refs_file"

    log "Simulating squash merge of #${number}: ${title}"
    if ! git -C "$worktree_dir" merge --squash --quiet "$local_ref"; then
      git -C "$worktree_dir" status --short || true
      die "Batch simulation failed while applying PR #${number}"
    fi

    GIT_AUTHOR_NAME='pi' \
    GIT_AUTHOR_EMAIL='pi@example.invalid' \
    GIT_COMMITTER_NAME='pi' \
    GIT_COMMITTER_EMAIL='pi@example.invalid' \
      git -C "$worktree_dir" commit --quiet -m "chore: simulate squash merge of #${number}" >/dev/null
  done < "$selected_file"

  if [ "$skip_local_checks" -eq 0 ]; then
    log "Running local checks in simulated batch state"
    (
      cd "$worktree_dir"
      mise run test
      mise run check
      mise run vulncheck
    )
  else
    log "Skipping local checks because --skip-local-checks was provided"
  fi

  log "Local batch simulation passed"
}

validate_single_pr_ready() {
  local number="$1"
  local output_file
  output_file="$(mktemp)"

  run gh pr view \
    "$number" \
    --repo "$repo" \
    --json number,title,url,state,author,isDraft,mergeStateStatus,baseRefName,statusCheckRollup,headRefOid,reviewDecision \
    > "$output_file"

  PR_FILE="$output_file" python3 <<'PY'
import json
import os
import sys

with open(os.environ["PR_FILE"], "r", encoding="utf-8") as fh:
    pr = json.load(fh)

ok_conclusions = {"SUCCESS", "NEUTRAL", "SKIPPED"}

def check_name(item):
    return (
        item.get("name")
        or item.get("context")
        or item.get("workflowName")
        or item.get("__typename")
        or "check"
    )

reasons = []
if pr.get("state") != "OPEN":
    reasons.append(f"state={pr.get('state')}")
if pr.get("baseRefName") != "main":
    reasons.append(f"base={pr.get('baseRefName')}")
if pr.get("isDraft"):
    reasons.append("draft")
if pr.get("reviewDecision") == "CHANGES_REQUESTED":
    reasons.append("changes requested")
if pr.get("mergeStateStatus") != "CLEAN":
    reasons.append(f"mergeState={pr.get('mergeStateStatus') or 'UNKNOWN'}")

checks = pr.get("statusCheckRollup") or []
if not checks:
    reasons.append("no checks reported")
else:
    pending = []
    failing = []
    for check in checks:
        name = check_name(check)
        status = check.get("status")
        conclusion = check.get("conclusion")
        if status != "COMPLETED":
            pending.append(name)
        elif conclusion not in ok_conclusions:
            failing.append(f"{name}={conclusion or 'UNKNOWN'}")
    if pending:
        reasons.append("pending checks: " + ", ".join(pending))
    if failing:
        reasons.append("failing checks: " + ", ".join(failing))

if reasons:
    print(
        f"[debux-pr-batch-merge] ERROR: PR #{pr['number']} is no longer ready: " + "; ".join(reasons),
        file=sys.stderr,
    )
    sys.exit(1)

title = pr["title"].replace("\t", " ").replace("\n", " ")
print(f"{pr['headRefOid']}\t{title}")
PY

  rm -f "$output_file"
}

merge_selected_prs() {
  merged_file="$(mktemp)"

  while IFS=$'\t' read -r number _ title; do
    [ -n "$number" ] || continue

    latest_meta="$(validate_single_pr_ready "$number")"
    latest_head_oid="${latest_meta%%$'\t'*}"
    latest_title="${latest_meta#*$'\t'}"

    log "Merging #${number}: ${latest_title}"
    run gh pr merge "$number" --repo "$repo" --squash --match-head-commit "$latest_head_oid"
    printf '#%s %s\n' "$number" "$latest_title" >> "$merged_file"
  done < "$selected_file"
}

print_summary() {
  if [ -n "$merged_file" ] && [ -s "$merged_file" ]; then
    log "Merged PRs:"
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      log "  - $line"
    done < "$merged_file"
  fi

  remaining_json="$(gh pr list --repo "$repo" --state open --limit 100 --json number,title,url)"
  REMAINING_JSON="$remaining_json" python3 <<'PY'
import json
import os
import sys

prs = json.loads(os.environ["REMAINING_JSON"])
if not prs:
    print("[debux-pr-batch-merge] Remaining open PRs: none", file=sys.stderr)
    sys.exit(0)

print("[debux-pr-batch-merge] Remaining open PRs:", file=sys.stderr)
for pr in sorted(prs, key=lambda item: item["number"]):
    print(f"  - #{pr['number']} {pr['title']} {pr['url']}", file=sys.stderr)
PY
}

require_command git
require_command gh
require_command python3
require_command mise

run gh auth status

if ! git rev-parse --show-toplevel >/dev/null 2>&1; then
  die "Not inside a Git repository"
fi
cd "$(git rev-parse --show-toplevel)"

[ -f mise.toml ] || die "mise.toml not found; run from the Debux repository"

branch="$(git branch --show-current)"
[ "$branch" = "main" ] || die "Batch merge must be run from main, current branch is '${branch:-detached}'"

require_clean_tree
sync_local_main
require_clean_tree

select_prs

if [ ! -s "$selected_file" ]; then
  log "Nothing to do."
  exit 0
fi

simulate_batch

if [ "$dry_run" -eq 1 ]; then
  log "Dry-run complete; no remote merges were performed"
  exit 0
fi

merge_selected_prs
sync_local_main
require_clean_tree
print_summary
