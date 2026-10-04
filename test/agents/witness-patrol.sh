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
source "$(dirname "${BASH_SOURCE[0]}")/lib/bead-id.sh"
cd "$GC_CITY"

while true; do
    # Check inbox for instructions
    inbox=$(gc mail inbox "$GC_AGENT" 2>/dev/null || true)
    if echo "$inbox" | bead_id_rows | grep -q .; then
        echo "$inbox" | bead_id_rows | while read -r line; do
            id=$(echo "$line" | awk '{print $1}')
            gc mail read "$id" 2>/dev/null || true
        done
    fi

    # Scan for orphaned beads (open, no assignee)
    ready=$(bd ready 2>/dev/null || true)
    if echo "$ready" | bead_id_rows | grep -q .; then
        echo "$ready" | bead_id_rows | while read -r line; do
            id=$(echo "$line" | awk '{print $1}')
            # Signal recovery by sending mail to mayor
            gc mail send mayor "Orphaned bead $id detected" 2>/dev/null || true
        done
    fi

    sleep 0.5
done
