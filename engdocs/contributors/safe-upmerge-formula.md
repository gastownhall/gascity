---
title: Safe Upmerge Formula
description: Repeatable process for merging latest main into a feature branch without losing committed or dirty local changes.
---

## Current Snapshot

Snapshot taken on 2026-04-23 from `feature/t3-session-bridge`.

- Current branch tip: `b1df7141`
- Fetched `upstream/main`: `bc6058d3`
- Fetched `origin/main`: `afb1ecdd`
- Local `main`: `39989691`

Use a fetched remote-tracking ref as the merge target. Do not use local
`main` here: it is stale and divergent.

Current divergence:

- `upstream/main...HEAD`: upstream ahead `263`, branch ahead `16`
- `origin/main...HEAD`: origin ahead `2`, branch ahead `1094`

Current dirty files that must not be lost:

- `cmd/gc/api_state.go`
- `cmd/gc/cmd_reload.go`
- `cmd/gc/providers.go`
- `cmd/gc/template_resolve.go`

Incoming overlap on those dirty files:

- `upstream/main`: `cmd/gc/api_state.go`, `cmd/gc/providers.go`, `cmd/gc/template_resolve.go`
- `origin/main`: `cmd/gc/api_state.go`, `cmd/gc/cmd_reload.go`, `cmd/gc/providers.go`, `cmd/gc/template_resolve.go`

Isolated merge probes against current `HEAD`:

- `upstream/main` conflicts:
  `cmd/gc/agent_build_params.go`,
  `cmd/gc/cmd_citystatus.go`,
  `cmd/gc/cmd_doctor.go`,
  `cmd/gc/cmd_status.go`,
  `cmd/gc/session_beads.go`,
  `cmd/gc/template_resolve.go`,
  `examples/dolt/assets/scripts/runtime.sh`,
  `go.mod`,
  `internal/api/handler_config_test.go`,
  `internal/api/huma_handlers_config.go`,
  `internal/doctor/checks.go`
- `origin/main` conflicts:
  `CLAUDE.md`,
  `cmd/gc/agent_build_params.go`,
  `cmd/gc/beads_provider_lifecycle.go`,
  `cmd/gc/beads_provider_lifecycle_test.go`,
  `cmd/gc/cmd_prime.go`,
  `cmd/gc/gc-beads-bd`,
  `cmd/gc/session_beads.go`,
  `cmd/gc/session_beads_test.go`,
  `cmd/gc/template_resolve.go`

For this branch, `upstream/main` is the better default target.

## Formula

This is the repeatable sequence.

### 1. Refresh refs and choose target

```bash
git fetch origin main
git fetch upstream main
TARGET=upstream/main
BRANCH=$(git branch --show-current)
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
```

If you need the fork's `main`, change `TARGET=origin/main`.

### 2. Save every current change before merging

```bash
mkdir -p .git/upmerge
git branch "safety/${BRANCH##*/}-${STAMP}" HEAD
git diff --binary > ".git/upmerge/${STAMP}.worktree.patch"
git diff --binary --cached > ".git/upmerge/${STAMP}.index.patch"
git stash push -u -m "upmerge:${BRANCH}:${STAMP}"
```

Why both patch files and stash:

- the stash preserves tracked, staged, and untracked files in one command
- the patch files are a second recovery path if stash application gets messy
- the safety branch preserves the exact pre-merge commit graph

### 3. Compare before real merge

```bash
git rev-list --left-right --count "$TARGET"...HEAD
git diff --name-status "$TARGET"...HEAD
git diff --name-status HEAD.."$TARGET" -- \
  cmd/gc/api_state.go \
  cmd/gc/cmd_reload.go \
  cmd/gc/providers.go \
  cmd/gc/template_resolve.go
```

Do not skip this. It tells you whether incoming `main` edits overlap the files
you were already changing.

### 4. Merge in a detached worktree, not in the dirty working tree

```bash
SCRATCH="../gascity-upmerge-${STAMP}"
git worktree add --detach "$SCRATCH" HEAD
cd "$SCRATCH"
git switch -c "upmerge/${BRANCH##*/}-${STAMP}"
git merge --no-ff "$TARGET"
```

Why this shape:

- original working tree stays recoverable
- merge conflicts are isolated
- you can abandon the scratch worktree without touching the main checkout

### 5. Resolve and validate in scratch

Resolve conflicts in the scratch worktree. Then run the normal quality gates:

```bash
go test ./...
go vet ./...
```

If you want the previously dirty local overlay on top of the merged result,
apply it in the scratch worktree first:

```bash
git stash apply --index "stash^{/upmerge:${BRANCH}:${STAMP}}"
```

If stash apply fails, fall back to the saved patches:

```bash
git apply --3way ".git/upmerge/${STAMP}.worktree.patch"
git apply --3way --cached ".git/upmerge/${STAMP}.index.patch"
```

Re-run quality gates after restoring the overlay.

### 6. Land the scratch merge back onto the feature branch

Once the scratch branch is correct:

```bash
cd /data/projects/gascity
git switch "$BRANCH"
git merge --ff-only "upmerge/${BRANCH##*/}-${STAMP}"
```

At this point the feature branch contains the merge commit produced in the
scratch worktree. Nothing from the original dirty tree is lost because it is
still recoverable from the stash, patch files, and safety branch.

### 7. Restore the original local overlay if you did not already apply it

```bash
git stash apply --index "stash^{/upmerge:${BRANCH}:${STAMP}}"
```

If needed:

```bash
git apply --3way ".git/upmerge/${STAMP}.worktree.patch"
git apply --3way --cached ".git/upmerge/${STAMP}.index.patch"
```

Only drop the stash after the restored files look correct:

```bash
git stash drop "stash^{/upmerge:${BRANCH}:${STAMP}}"
```

### 8. Clean up scratch state

```bash
git worktree remove -f "$SCRATCH"
git branch -D "upmerge/${BRANCH##*/}-${STAMP}"
```

Keep the `safety/...` branch until the merged branch is pushed and verified.

## Recovery

If anything goes wrong, use one of these in this order:

1. `git stash apply --index "stash^{/upmerge:${BRANCH}:${STAMP}}"`
2. `git apply --3way .git/upmerge/${STAMP}.worktree.patch`
3. `git apply --3way --cached .git/upmerge/${STAMP}.index.patch`
4. `git switch "safety/${BRANCH##*/}-${STAMP}"`

The goal is simple: never do the risky merge in the only place that contains
your uncommitted work.
