#!/bin/bash
# Bash agent: refinery merge processor.
# Simulates the refinery flow: check for merge requests, rebase,
# merge to main, close the bead, loop for more.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   GC_DIR   — path to the rig's repo (working copy)
#   PATH     — must include gc and bd binaries

set -euo pipefail
cd "$GC_CITY"
ASSIGNEE="${GC_SESSION_NAME:-$GC_AGENT}"

while true; do
    hooked=$(bd ready --assignee="$ASSIGNEE" 2>/dev/null || true)
    if echo "$hooked" | grep -q "^gc-"; then
        work_id=$(echo "$hooked" | grep "^gc-" | head -1 | awk '{print $1}')

        if [ -n "${GC_DIR:-}" ] && [ -d "$GC_DIR" ]; then
            cd "$GC_DIR"

            # Find the branch to merge (convention: gc/*/work_id)
            branch=$(git branch -r 2>/dev/null | grep "$work_id" | head -1 | tr -d ' ' || true)
            if [ -n "$branch" ]; then
                git checkout main 2>/dev/null || true
                git merge "$branch" --no-edit 2>/dev/null || true
            fi

            cd "$GC_CITY"
        fi

        # Close the merge bead
        bd close "$work_id" 2>/dev/null || true
        continue
    fi

    ready=$(bd ready 2>/dev/null || true)
    if echo "$ready" | grep -q "^gc-"; then
        id=$(echo "$ready" | grep "^gc-" | head -1 | awk '{print $1}')
        bd update "$id" --assignee="$ASSIGNEE" 2>/dev/null || true
        continue
    fi

    sleep 0.2
done
