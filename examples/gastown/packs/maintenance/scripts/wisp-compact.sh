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
# Runs as an exec order (no LLM, no agent, no wisp).
#
# See lx-qfq5r1 / lx-f2z2ph for the hardening pass — bounded query
# via --label-pattern + --updated-before, flock, bd_safe, trap.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-bd-safe.sh
. "$SCRIPT_DIR/lib-bd-safe.sh"
install_trap
acquire_lock

CITY="${GC_CITY:-.}"

# Candidate query cutoff: the minimum TTL in the table above (6h) so
# the query captures every wisp that could possibly be past its TTL.
# Per-bead logic below applies the specific TTL per wisp_type.
# Override via env var if you want a wider/narrower candidate window.
CUTOFF="${WISP_COMPACT_CUTOFF:-6h}"
MAX_BEADS="${WISP_COMPACT_MAX_BEADS:-1000}"

# Parse the cutoff into hours (supports Nh and Nd only — keep simple).
case "$CUTOFF" in
    *h) CUTOFF_HOURS="${CUTOFF%h}" ;;
    *d) CUTOFF_HOURS=$((${CUTOFF%d} * 24)) ;;
    *)  CUTOFF_HOURS=6 ;;
esac

CUTOFF_TS=$(date -u -d "-${CUTOFF_HOURS} hours" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || date -u -v-"${CUTOFF_HOURS}"H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || exit 0

# Query candidate wisps: label matches wisp_type:* and last-updated is
# older than the candidate cutoff. Replaces the prior --all -n 0 scan
# that hammered Dolt during lx-wisp-ece.
WISPS=$(bd_safe list --label-pattern "wisp_type:*" \
    --updated-before "$CUTOFF_TS" --json --limit="$MAX_BEADS" 2>/dev/null) || exit 0

if [ -z "$WISPS" ] || [ "$WISPS" = "[]" ]; then
    exit 0
fi

NOW=$(date +%s)
PROMOTED=0
DELETED=0
SKIPPED=0

# Process each candidate in the main shell (here-string, not pipe) so
# the summary counters below reflect real totals.
WISP_LINES=$(echo "$WISPS" | jq -c '.[]' 2>/dev/null) || WISP_LINES=""
if [ -n "$WISP_LINES" ]; then
    while IFS= read -r bead; do
        [ -z "$bead" ] && continue

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
            bd_safe update "$id" --persistent 2>/dev/null || true
            bd_safe comment "$id" "Promoted from wisp: $REASON" 2>/dev/null || true
            PROMOTED=$((PROMOTED + 1))
            continue
        fi

        # Closed + past TTL + no special attributes → delete.
        bd_safe delete "$id" --force 2>/dev/null || true
        DELETED=$((DELETED + 1))
    done <<<"$WISP_LINES"
fi

TOTAL=$((PROMOTED + DELETED))
if [ "$TOTAL" -gt 0 ]; then
    echo "wisp-compact: promoted=$PROMOTED deleted=$DELETED skipped=$SKIPPED"
fi
