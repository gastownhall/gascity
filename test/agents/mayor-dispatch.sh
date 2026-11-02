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
source "$(dirname "${BASH_SOURCE[0]}")/lib/bead-id.sh"
cd "$GC_CITY"

while true; do
    # Check inbox for dispatch requests
    inbox=$(gc mail inbox "$GC_AGENT" 2>/dev/null || true)

    if echo "$inbox" | bead_id_rows | grep -q .; then
        # Process each message
        echo "$inbox" | bead_id_rows | while read -r line; do
            msg_id=$(echo "$line" | awk '{print $1}')

            # Read the dispatch request
            msg=$(gc mail read "$msg_id" 2>/dev/null || true)
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
