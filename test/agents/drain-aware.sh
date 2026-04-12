#!/bin/bash
# Bash agent: loop worker with drain awareness.
# Like loop.sh but checks gc runtime drain-check before each iteration.
# If drain-check returns 0 (draining), sends drain-ack and exits cleanly.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   PATH     — must include gc binary

set -euo pipefail
cd "$GC_CITY"
ASSIGNEE="${GC_SESSION_NAME:-$GC_AGENT}"

while true; do
    # Check if we're being drained
    if gc runtime drain-check 2>/dev/null; then
        gc runtime drain-ack 2>/dev/null || true
        exit 0
    fi

    hooked=$(bd ready --assignee="$ASSIGNEE" 2>/dev/null || true)
    if echo "$hooked" | grep -q "^gc-"; then
        id=$(echo "$hooked" | grep "^gc-" | head -1 | awk '{print $1}')
        bd close "$id"
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
