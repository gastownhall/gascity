#!/bin/bash
# Bash agent: loop with mail check.
# Implements the same flow as prompts/loop-mail.md using gc CLI commands.
# Used in integration tests as a deterministic substitute for an AI agent.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   PATH     — must include gc binary

set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/bead-id.sh"
cd "$GC_CITY"

while true; do
    # Step 1: Check inbox
    inbox=$(gc mail inbox "$GC_AGENT" 2>/dev/null || true)

    # Step 2: Process each unread message
    if echo "$inbox" | bead_id_rows | grep -q .; then
        echo "$inbox" | bead_id_rows | while read -r line; do
            id=$(echo "$line" | awk '{print $1}')

            # Read the message
            msg=$(gc mail read "$id" 2>/dev/null || true)
            from=$(echo "$msg" | grep "^From:" | awk '{print $2}')

            # Reply to sender
            if [ -n "$from" ]; then
                gc mail send "$from" "ack from $GC_AGENT" 2>/dev/null || true
            fi
        done
    fi

    sleep 0.5
done
