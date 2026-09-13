#!/bin/bash
# Bash agent: polecat work lifecycle.
# Simulates the polecat work formula: claim work, create branch,
# make a commit, push, assign refinery, close work bead, exit.
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

        # Step 1: Create feature branch
        if [ -n "${GC_DIR:-}" ] && [ -d "$GC_DIR" ]; then
            cd "$GC_DIR"
            branch="gc/${GC_AGENT}/${work_id}"
            git checkout -b "$branch" 2>/dev/null || git checkout "$branch" 2>/dev/null || true

            # Step 2: Make a change and commit
            echo "fix for $work_id" > "fix-${work_id}.txt"
            git add -A
            git commit -m "fix: $work_id" 2>/dev/null || true

            # Step 3: Push
            git push origin "$branch" 2>/dev/null || true

            cd "$GC_CITY"
        fi

        bd close "$work_id" 2>/dev/null || true

        exit 0
    fi

    sleep 0.2
done
