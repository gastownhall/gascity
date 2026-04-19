#!/usr/bin/env bash
# mock-refinery.sh — Deterministic merge agent for lifecycle demo.
#
# Works directly in the rig directory (on main). Polls for beads that
# have branch metadata set by polecats, merges each branch to main, and
# closes the bead.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name (e.g., "demo-repo/refinery")
#   GC_CITY  — path to the city directory
#   GC_DIR   — working directory (rig repo path)

set -euo pipefail

cd "$GC_DIR"

# Disable git credential prompts (K8s pods have no TTY for interactive input).
export GIT_TERMINAL_PROMPT=0

# Pull latest from origin (K8s: repo is baked in, pull gets updates).
git pull --ff-only origin main 2>/dev/null || true

AGENT_SHORT=$(basename "$GC_AGENT")
POLL_INTERVAL="${GC_REFINERY_POLL:-3}"

echo "[$AGENT_SHORT] Starting merge agent in rig dir: $GC_DIR"
echo "[$AGENT_SHORT] Polling every ${POLL_INTERVAL}s for merge-ready branches..."

# Verify we're on main.
CURRENT=$(git branch --show-current 2>/dev/null || true)
echo "[$AGENT_SHORT] Current branch: $CURRENT"

while true; do
    # Fetch latest branches from origin (K8s: polecats push to origin from
    # separate pods; local: branches exist locally, fetch is a no-op).
    git fetch origin 2>/dev/null || true

    # Scan for beads assigned to us (polecats reassign after pushing their branch).
    MERGE_BEADS=$(bd list --assignee="$GC_AGENT" --status=in_progress --json 2>/dev/null || echo "[]")

    if echo "$MERGE_BEADS" | jq -e 'length > 0' >/dev/null 2>&1; then
        # Process each bead that has branch metadata.
        echo "$MERGE_BEADS" | jq -r '.[] | select(.metadata.branch != null) | "\(.id) \(.metadata.branch) \(.title)"' 2>/dev/null | while IFS=' ' read -r BID BRANCH BTITLE; do
            [ -z "$BID" ] && continue
            [ -z "$BRANCH" ] && continue

            # In K8s the branch is on origin (polecats push from separate pods).
            # Create a local tracking branch if it doesn't exist yet.
            if ! git rev-parse --verify "$BRANCH" 2>/dev/null; then
                git branch "$BRANCH" "origin/$BRANCH" 2>/dev/null || {
                    echo "[$AGENT_SHORT] Branch $BRANCH not found locally or on origin — skipping"
                    continue
                }
            fi

            echo "[$AGENT_SHORT] Found merge-ready: $BRANCH ($BTITLE)"

            gc mail send --all "MERGING: $BRANCH ($BTITLE)" 2>/dev/null || true

            # Merge the branch into main.
            if git merge "$BRANCH" -m "merge: $BTITLE ($BID)" 2>/dev/null; then
                echo "[$AGENT_SHORT] Merged $BRANCH to main"

                # Push main to origin.
                if git remote | grep -q origin 2>/dev/null; then
                    git push origin main 2>/dev/null || true
                    echo "[$AGENT_SHORT] Pushed main to origin"
                fi

                # Close the bead.
                bd close "$BID" 2>/dev/null || true
                echo "[$AGENT_SHORT] Closed bead: $BID"

                # Cleanup branch (local + remote).
                git branch -d "$BRANCH" 2>/dev/null || git branch -D "$BRANCH" 2>/dev/null || true
                git push origin --delete "$BRANCH" 2>/dev/null || true

                gc mail send --all "MERGED: $BRANCH landed on main ($BTITLE)" 2>/dev/null || true
            else
                echo "[$AGENT_SHORT] Merge failed for $BRANCH — aborting merge"
                git merge --abort 2>/dev/null || true
                gc mail send --all "MERGE FAILED: $BRANCH ($BTITLE)" 2>/dev/null || true
            fi
        done
    fi

    sleep "$POLL_INTERVAL"
done
