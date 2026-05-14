---
name: debux-pr-batch-merge
description: Use this skill when the user asks to review and merge a batch of ready Debux pull requests, especially Dependabot updates. It validates candidate PRs, simulates the combined squash merge locally, runs project checks, and then merges the PRs sequentially with gh CLI.
---

# Debux PR batch merge skill

This skill merges a safe batch of Debux pull requests from the repository root.

Use it only when the user explicitly asks to merge PRs or confirms that merging is OK.

By default, the helper targets open Dependabot PRs that:
- target `main`
- are not drafts
- have `CLEAN` merge state
- have only successful/neutral/skipped checks

Pass explicit PR numbers to merge a custom batch.

## Preferred command

Run the helper:

```bash
bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh
```

The helper does the full safe path:

1. Verifies required tools: `git`, `gh`, `python3`, and `mise`.
2. Verifies GitHub CLI authentication.
3. Requires a clean `main` branch.
4. Fetches `origin/main` and fast-forwards local `main` if needed.
5. Selects ready PRs (default: open Dependabot PRs on `main`).
6. Simulates the combined squash merge in a temporary worktree.
7. Runs:
   - `mise run test`
   - `mise run check`
   - `mise run vulncheck`
8. Squash-merges the PRs one by one with `gh pr merge`.
9. Fast-forwards local `main` again.
10. Prints merged PRs and any remaining open PRs.

## Options

Use options when the user asks for them or when targeting a specific batch:

```bash
bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh --dry-run
bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh 14 15 18
bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh --author app/dependabot
bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh --skip-local-checks
bash .claude/skills/debux-pr-batch-merge/scripts/merge-prs.sh --repo OWNER/REPO
```

## Agent workflow

When using this skill:

1. Start by running `git status --short --branch` and `gh auth status` if the user did not already show the repo is ready.
2. Run the helper from the repository root.
3. If the helper reports "No ready PRs matched the selection" or "Nothing to do", summarize that there were no eligible PRs.
4. If batch simulation or local checks fail, stop and summarize the failing PR or command. Do not merge anything remotely.
5. If a PR stops being ready between simulation and merge, stop and report which PR blocked the batch.
6. On success, summarize:
   - merged PR numbers/titles
   - whether local simulation/checks ran
   - whether any PRs remain open

## Manual fallback

If the helper cannot be used, follow this sequence:

```bash
git status --short --branch
gh auth status
git fetch origin main --tags
git switch main
git pull --ff-only origin main
gh pr list --repo clement-tourriere/debux --state open
```

Then inspect the candidate PRs with `gh pr view <n> --json ...`, simulate merges locally if needed, and merge ready PRs with:

```bash
gh pr merge <n> --repo clement-tourriere/debux --squash
```
