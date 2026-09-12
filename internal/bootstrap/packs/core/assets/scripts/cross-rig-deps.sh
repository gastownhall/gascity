#!/usr/bin/env bash
# cross-rig-deps — convert satisfied cross-rig blocks to related.
#
# Replaces the deacon patrol cross-rig-deps step. When an issue in one
# rig closes, dependent issues in other rigs stay blocked because
# computeBlockedIDs doesn't resolve across rig boundaries. This script
# converts satisfied cross-rig blocks deps to related, preserving the
# audit trail while removing blocking semantics.
#
# Uses the bead.closed event stream with a fixed lookback window (15
# minutes), then scans each store's indexed depends_on_external column
# once. Cost is O(stores), not O(recently-closed beads). Idempotent —
# converting an already-related dep is a no-op.
#
# Becomes unnecessary when beads supports cross-rig computeBlockedIDs.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "cross-rig-deps"

if ! command -v jq >/dev/null 2>&1; then
    echo "cross-rig-deps: jq is required but not found in PATH" >&2
    exit 1
fi

CITY="${GC_CITY:-.}"
LOOKBACK="${CROSS_RIG_LOOKBACK:-15m}"

# Step 1: Read close transitions once. A quiet run exits before spawning bd.
EVENTS=$(gc events --type bead.closed --since "$LOOKBACK" 2>/dev/null) || exit 0
[ -n "$EVENTS" ] || exit 0

CLOSED_IDS=$(printf '%s\n' "$EVENTS" \
    | jq -r '.payload.bead.id // .payload.id // empty' 2>/dev/null \
    | sort -u) || exit 0
[ -n "$CLOSED_IDS" ] || exit 0
CLOSED_JSON=$(printf '%s\n' "$CLOSED_IDS" | jq -Rsc 'split("\n") | map(select(length > 0))')

# Build the store list once. gc bd is routing sugar, not a federation
# layer, so every store must be queried explicitly. The query volume is
# therefore fixed at one read per store regardless of close throughput.
RIGS_JSON=$(gc rig list --json 2>/dev/null) || exit 0
SCOPES=$(printf '%s' "$RIGS_JSON" \
    | jq -r '(.rigs // [])[]
             | [(if .hq == true then "city" else "rig" end), .name]
             | @tsv' 2>/dev/null) || exit 0
[ -n "$SCOPES" ] || exit 0

run_bd_for_scope() {
    scope_kind="$1"
    scope_name="$2"
    shift 2
    if [ "$scope_kind" = "city" ]; then
        gc bd --city "$CITY" "$@"
    else
        gc bd --rig "$scope_name" "$@"
    fi
}

# Cross-prefix targets are stored verbatim in depends_on_external. bd dep
# list returns hydrated issue records and cannot surface a target that is
# absent from the local store, so query the owning column directly.
EXTERNAL_BLOCKS_SQL="SELECT issue_id, depends_on_external
FROM dependencies
WHERE type = 'blocks' AND depends_on_external IS NOT NULL
UNION ALL
SELECT issue_id, depends_on_external
FROM wisp_dependencies
WHERE type = 'blocks' AND depends_on_external IS NOT NULL"

BATCH_FILE=$(mktemp "${TMPDIR:-/tmp}/cross-rig-deps.XXXXXX")
ERR_FILE=$(mktemp "${TMPDIR:-/tmp}/cross-rig-deps.err.XXXXXX")
trap 'rm -f "$BATCH_FILE" "$ERR_FILE"' EXIT

RESOLVED=0
while IFS="$(printf '\t')" read -r scope_kind scope_name; do
    [ -n "$scope_kind" ] || continue
    if ! ROWS=$(run_bd_for_scope "$scope_kind" "$scope_name" \
            sql --json "$EXTERNAL_BLOCKS_SQL" 2>"$ERR_FILE"); then
        echo "cross-rig-deps: skipping $scope_name — bd sql failed: $(tail -n 1 "$ERR_FILE")" >&2
        continue
    fi
    if [ -z "$ROWS" ] || [ "$ROWS" = "[]" ]; then
        continue
    fi

    MATCHES=$(printf '%s' "$ROWS" \
        | jq -r --argjson closed "$CLOSED_JSON" '
            .[]
            | select(.issue_id != null and .depends_on_external != null)
            | select(.depends_on_external as $target | $closed | index($target))
            | [.issue_id, .depends_on_external]
            | @tsv' 2>/dev/null) || MATCHES=""
    [ -n "$MATCHES" ] || continue

    : > "$BATCH_FILE"
    while IFS="$(printf '\t')" read -r dep_id closed_id; do
        [ -n "$dep_id" ] && [ -n "$closed_id" ] || continue
        printf 'dep remove %s %s\n' "$dep_id" "$closed_id" >> "$BATCH_FILE"
        printf 'dep add %s %s related\n' "$dep_id" "$closed_id" >> "$BATCH_FILE"
    done <<< "$MATCHES"

    STORE_RESOLVED=$(grep -c '^dep remove ' "$BATCH_FILE" 2>/dev/null) || STORE_RESOLVED=0
    [ "$STORE_RESOLVED" -gt 0 ] || continue
    if ! run_bd_for_scope "$scope_kind" "$scope_name" batch \
            -f "$BATCH_FILE" -m "cross-rig-deps sweep"; then
        echo "cross-rig-deps: bd batch failed in $scope_name — no dependencies were converted" >&2
        exit 1
    fi
    RESOLVED=$((RESOLVED + STORE_RESOLVED))
done <<< "$SCOPES"

if [ "$RESOLVED" -gt 0 ]; then
    echo "cross-rig-deps: resolved $RESOLVED cross-rig dependencies"
fi
