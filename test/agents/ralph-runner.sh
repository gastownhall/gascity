#!/bin/bash
# Bash agent: Ralph demo runner.
# Waits for an assigned open bead, writes a demo file into the city root,
# closes the run bead, and exits.

set -euo pipefail
cd "$GC_CITY"

while true; do
    assigned=$(bd list --assignee="$GC_AGENT" --status=open --json 2>/dev/null || true)
    bead_id=$(echo "$assigned" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | sed -n '1p')
    if [ -n "$bead_id" ]; then
        printf 'hello from ralph\n' > "$GC_CITY/ralph-demo.txt"
        bd close "$bead_id"
        exit 0
    fi
    sleep 0.2
done
