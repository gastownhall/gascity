#!/bin/sh
# worktree-setup.sh — idempotent git worktree creation for the t3bridge
# gastown demo pack.
#
# Usage: worktree-setup.sh <rig-root> <work-dir> <agent-name> [--sync]
#
# Base branch: GC_DEFAULT_BRANCH (exported by pre_start from the rig's
# configured default_branch) wins. It is empty for rigs that record no
# default_branch, so the script falls back to probing origin/HEAD and finally
# to the rig's current HEAD. Fresh worktrees are always cut from
# origin/$BRANCH, never from a possibly-stale local ref.

set -eu

rig_root="${1:?rig root required}"
work_dir="${2:?work dir required}"
agent="${3:-agent}"
mode="${4:---sync}"

agent_branch="gc/${agent}"

# Resolve the rig's mainline branch. The configured value wins; otherwise probe
# origin/HEAD. Empty means "no known base" and callers fall back to local HEAD.
resolve_base_branch() {
  candidate="${GC_DEFAULT_BRANCH:-}"
  if [ -z "$candidate" ]; then
    candidate=$(git -C "$rig_root" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)
    candidate="${candidate#origin/}"
  fi
  printf '%s' "$candidate"
}

# Re-anchor a reused worktree onto the base branch, but only when doing so is
# provably lossless: the tree has no uncommitted changes AND every commit on
# HEAD is already reachable from origin/$BRANCH. Anything else is left exactly
# as found. The rule is pure git fact — never a guess about whether the work
# "looks done".
reanchor_worktree() {
  BRANCH=$(resolve_base_branch)
  if [ -z "$BRANCH" ]; then
    echo "worktree-setup: left as-is: no base branch resolved"
    return 0
  fi
  if ! git -C "$work_dir" fetch origin "$BRANCH" >/dev/null 2>&1; then
    echo "worktree-setup: left as-is: fetch of origin/$BRANCH failed"
    return 0
  fi
  if [ -n "$(git -C "$work_dir" status --porcelain)" ]; then
    echo "worktree-setup: left as-is: dirty"
    return 0
  fi
  # The clean+ancestor proofs below are about HEAD, but checkout -B moves the
  # agent branch ref; only when HEAD IS that branch is the reset provably
  # lossless (a detached or side-branch HEAD can pass both checks while the
  # agent branch still points at unpushed commits).
  if [ "$(git -C "$work_dir" symbolic-ref --quiet --short HEAD)" != "$agent_branch" ]; then
    echo "worktree-setup: left as-is: not on $agent_branch"
    return 0
  fi
  if ! git -C "$work_dir" merge-base --is-ancestor HEAD "origin/$BRANCH"; then
    echo "worktree-setup: left as-is: unmerged commits"
    return 0
  fi
  if git -C "$work_dir" checkout -B "$agent_branch" "origin/$BRANCH"; then
    echo "worktree-setup: re-anchored $agent_branch on origin/$BRANCH"
  else
    echo "worktree-setup: left as-is: checkout of origin/$BRANCH failed"
  fi
}

mkdir -p "$(dirname "$work_dir")"

if [ -d "$work_dir/.git" ] || git -C "$work_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if [ "$mode" = "--sync" ]; then
    reanchor_worktree
  fi
  exit 0
fi

BRANCH=$(resolve_base_branch)
if [ -n "$BRANCH" ]; then
  git -C "$rig_root" fetch origin "$BRANCH" >/dev/null 2>&1 || true
fi

if git -C "$rig_root" show-ref --verify --quiet "refs/heads/$agent_branch"; then
  # The agent branch already exists and may carry unpushed work; adopt it
  # untouched rather than rewinding it onto the base branch.
  git -C "$rig_root" worktree add "$work_dir" "$agent_branch"
  echo "worktree-setup: adopted existing branch $agent_branch"
elif [ -n "$BRANCH" ] && git -C "$rig_root" rev-parse --verify --quiet "refs/remotes/origin/$BRANCH^{commit}" >/dev/null; then
  git -C "$rig_root" worktree add -b "$agent_branch" "$work_dir" "origin/$BRANCH"
  echo "worktree-setup: created $agent_branch from origin/$BRANCH"
else
  # No remote base to anchor on (no origin, or an unfetchable base branch).
  git -C "$rig_root" worktree add -b "$agent_branch" "$work_dir"
  echo "worktree-setup: created $agent_branch from rig-root HEAD (no origin/$BRANCH)"
fi
