#!/usr/bin/env bash
# wisp-compact — TTL-based cleanup of expired ephemeral beads.
#
# Wisps are short-lived work items (heartbeats, pings, patrols) that
# accumulate and bloat the database. This script applies retention policy:
# - Closed wisps past TTL → deleted (Dolt AS OF preserves history)
# - Non-closed wisps past TTL → promoted to permanent (stuck detection)
# - Wisps with comments or "keep" label → promoted (proven value)
#
# TTL by wisp_type label:
#   heartbeat, ping: 6h
#   patrol, gc_report: 24h
#   recovery, error, escalation: 7d
#   default (untyped): 24h
#
# NOTE on `bd batch` (beads#6): this script loops `bd update`, `bd
# comment`, and `bd delete` per wisp. `bd update` is in the batch
# grammar, but `bd comment` and `bd delete` are not. Per-bead branching
# (promote vs delete) also makes a single batch stream impractical.
# Nothing here can be meaningfully folded into a batch transaction.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

CITY="${GC_CITY:-.}"

# Get all ephemeral beads.
ALL=$(bd list --json --all -n 0 2>/dev/null) || exit 0
EPHEMERALS=$(echo "$ALL" | jq '[.[] | select(.ephemeral == true)]' 2>/dev/null) || exit 0

if [ -z "$EPHEMERALS" ] || [ "$EPHEMERALS" = "[]" ]; then
    exit 0
fi

NOW=$(date +%s)
PROMOTED=0
DELETED=0
SKIPPED=0

# Process each ephemeral bead.
echo "$EPHEMERALS" | jq -c '.[]' 2>/dev/null | while IFS= read -r bead; do
    id=$(echo "$bead" | jq -r '.id')
    status=$(echo "$bead" | jq -r '.status')
    updated_at=$(echo "$bead" | jq -r '.updated_at // .created_at')
    comment_count=$(echo "$bead" | jq -r '.comment_count // 0')
    labels=$(echo "$bead" | jq -r '.labels // [] | .[]' 2>/dev/null)

    # Determine TTL from wisp_type label.
    TTL_SECONDS=$((24 * 3600))  # default: 24h
    for label in $labels; do
        case "$label" in
            wisp_type:heartbeat|wisp_type:ping) TTL_SECONDS=$((6 * 3600)) ;;
            wisp_type:patrol|wisp_type:gc_report) TTL_SECONDS=$((24 * 3600)) ;;
            wisp_type:recovery|wisp_type:error|wisp_type:escalation) TTL_SECONDS=$((7 * 24 * 3600)) ;;
            keep) TTL_SECONDS=0 ;;  # force promote
        esac
    done

    # Calculate age.
    BEAD_TS=$(date -d "$updated_at" +%s 2>/dev/null || date -j -f "%Y-%m-%dT%H:%M:%S" "$updated_at" +%s 2>/dev/null) || continue
    AGE=$((NOW - BEAD_TS))

    # Skip if within TTL (unless force-promote via keep label).
    if [ "$TTL_SECONDS" -gt 0 ] && [ "$AGE" -lt "$TTL_SECONDS" ]; then
        SKIPPED=$((SKIPPED + 1))
        continue
    fi

    # Promote if has comments, keep label, or non-closed.
    if [ "$comment_count" -gt 0 ] || echo "$labels" | grep -q '^keep$' || [ "$status" != "closed" ]; then
        REASON="proven value"
        [ "$status" != "closed" ] && REASON="open past TTL (stuck detection)"
        bd update "$id" --persistent 2>/dev/null || true
        bd comment "$id" "Promoted from wisp: $REASON" 2>/dev/null || true
        PROMOTED=$((PROMOTED + 1))
        continue
    fi

    # Closed + past TTL + no special attributes → delete.
    bd delete "$id" --force 2>/dev/null || true
    DELETED=$((DELETED + 1))
done

TOTAL=$((PROMOTED + DELETED))
if [ "$TOTAL" -gt 0 ]; then
    echo "wisp-compact: promoted=$PROMOTED deleted=$DELETED skipped=$SKIPPED"
fi
