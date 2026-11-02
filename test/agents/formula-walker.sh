#!/bin/bash
# Bash agent: formula step walker.
# Checks for claimed work, reads formula steps via bd mol current,
# closes steps in order, then closes the root bead.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   PATH     — must include gc binary

set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/bead-id.sh"
cd "$GC_CITY"
ASSIGNEE="${GC_SESSION_NAME:-$GC_AGENT}"

while true; do
    hooked=$(bd ready --assignee="$ASSIGNEE" 2>/dev/null || true)
    if echo "$hooked" | bead_id_rows | grep -q .; then
        root_id=$(echo "$hooked" | bead_id_rows | head -1 | awk '{print $1}')

        # Walk steps: close each open child bead
        children=$(bd list 2>/dev/null || true)
        if echo "$children" | bead_id_rows | grep -q .; then
            echo "$children" | bead_id_rows | while read -r line; do
                child_id=$(echo "$line" | awk '{print $1}')
                status=$(echo "$line" | awk '{print $2}')
                # Skip if already closed or if it's the root bead
                if [ "$child_id" = "$root_id" ]; then
                    continue
                fi
                if [ "$status" != "closed" ]; then
                    bd close "$child_id" 2>/dev/null || true
                fi
            done
        fi

        # Close the root bead
        bd close "$root_id" 2>/dev/null || true
        exit 0
    fi

    sleep 0.2
done
