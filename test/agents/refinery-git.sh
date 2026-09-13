#!/bin/bash
# Bash agent: refinery git merge processor.
# Deterministic refinery that exercises the full merge pipeline:
# poll for beads assigned to this agent → fetch → find branch →
# merge to main → push → close bead → loop.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   GC_DIR   — path to the rig's repo (working copy)
#   GIT_WORK_DIR — override for git repo path (optional, defaults to GC_DIR)
#   PATH     — must include gc, bd, and jq binaries

set -euo pipefail
cd "$GC_CITY"

REPO_DIR="${GIT_WORK_DIR:-$GC_DIR}"

while true; do
    # Check for beads assigned to this agent
    work_id=$(bd ready --assignee="$GC_AGENT" --json 2>/dev/null \
        | jq -r '.[0].id // empty' 2>/dev/null || true)

    if [ -n "$work_id" ]; then
        if [ -n "$REPO_DIR" ] && [ -d "$REPO_DIR" ]; then
            cd "$REPO_DIR"

            # Fetch latest from origin (polecat pushed to a different clone)
            git fetch origin 2>/dev/null || true

            default_branch=$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null | sed 's@^origin/@@' || true)
            if [ -z "$default_branch" ]; then
                default_branch="main"
            fi

            # Find the remote branch matching this work_id
            branch=$(git branch -r 2>/dev/null | grep "$work_id" | head -1 | tr -d ' ' || true)

            if [ -n "$branch" ]; then
                # Merge to the remote default branch and push. Let merge/push
                # failures abort the loop so tests do not silently close work
                # without landing the fix.
                git checkout "$default_branch" >/dev/null 2>&1
                git merge "$branch" --no-edit >/dev/null 2>&1
                git push origin "$default_branch" >/dev/null 2>&1
            fi

            cd "$GC_CITY"
        fi

        # Close the bead
        bd close "$work_id" 2>/dev/null || true
        continue
    fi

    sleep 0.2
done
