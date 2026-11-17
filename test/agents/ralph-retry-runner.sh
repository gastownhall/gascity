#!/bin/bash
# Bash agent: Ralph retry demo runner.
# Writes a failing artifact on attempt 1 and a passing artifact on attempt 2.

set -euo pipefail
cd "$GC_CITY"

while true; do
    assigned=$(bd list --assignee="$GC_AGENT" --status=open --json 2>/dev/null || true)
    bead_id=$(echo "$assigned" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | sed -n '1p')
    if [ -n "$bead_id" ]; then
        bead_json=$(bd show --json "$bead_id")
        attempt=$(echo "$bead_json" | sed -n 's/.*"gc.attempt"[[:space:]]*:[[:space:]]*"\([0-9][0-9]*\)".*/\1/p' | sed -n '1p')
        if [ -z "$attempt" ]; then
            attempt=1
        fi
        if [ "$attempt" = "1" ]; then
            printf 'fail\n' > "$GC_CITY/ralph-retry-demo.txt"
        else
            printf 'pass\n' > "$GC_CITY/ralph-retry-demo.txt"
        fi
        bd close "$bead_id"
        exit 0
    fi
    sleep 0.2
done
