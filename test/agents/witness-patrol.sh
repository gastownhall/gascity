#!/bin/bash
# Bash agent: witness patrol.
# Simulates the witness role: scans for orphaned beads (open beads
# with no assignee), reclaims them by assigning back to pool.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   PATH     — must include gc and bd binaries

set -euo pipefail
GC_BIN="${GC_BIN:-gc}"
cd "$GC_CITY"

while true; do
    # Check inbox for instructions
    inbox=$("$GC_BIN" mail inbox "$GC_AGENT" 2>/dev/null || true)
    if echo "$inbox" | grep -q "^gc-"; then
        echo "$inbox" | grep "^gc-" | while read -r line; do
            id=$(echo "$line" | awk '{print $1}')
            "$GC_BIN" mail read "$id" 2>/dev/null || true
        done
    fi

    # Scan for orphaned beads (open, no assignee)
    ready=$(bd ready 2>/dev/null || true)
    if echo "$ready" | grep -q "^gc-"; then
        echo "$ready" | grep "^gc-" | while read -r line; do
            id=$(echo "$line" | awk '{print $1}')
            # Signal recovery by sending mail to mayor
            "$GC_BIN" mail send mayor "Orphaned bead $id detected" 2>/dev/null || true
        done
    fi

    sleep 0.5
done
