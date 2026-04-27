#!/bin/bash
# Bash agent: mayor dispatch.
# Simulates the mayor role: checks inbox for instructions,
# creates work beads, exits after dispatching.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   PATH     — must include gc and bd binaries

set -euo pipefail
GC_BIN="${GC_BIN:-gc}"
cd "$GC_CITY"

while true; do
    # Check inbox for dispatch requests
    inbox=$("$GC_BIN" mail inbox "$GC_AGENT" 2>/dev/null || true)

    if echo "$inbox" | grep -q "^gc-"; then
        # Process each message
        echo "$inbox" | grep "^gc-" | while read -r line; do
            msg_id=$(echo "$line" | awk '{print $1}')

            # Read the dispatch request
            msg=$("$GC_BIN" mail read "$msg_id" 2>/dev/null || true)
            body=$(echo "$msg" | grep "^Body:" | sed 's/^Body:   //')

            # Create a work bead based on the message
            if [ -n "$body" ]; then
                bd create "$body" 2>/dev/null || true
            fi
        done
        exit 0
    fi

    sleep 0.2
done
