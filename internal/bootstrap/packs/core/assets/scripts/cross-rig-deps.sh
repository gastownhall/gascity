#!/usr/bin/env bash
# cross-rig-deps — convert satisfied cross-rig blocks to related.
#
# Replaces the deacon patrol cross-rig-deps step. When an issue in one
# rig closes, dependent issues in other rigs stay blocked because
# computeBlockedIDs doesn't resolve across rig boundaries. This script
# converts satisfied cross-rig blocks deps to related, preserving the
# audit trail while removing blocking semantics.
#
# Uses a fixed lookback window (15 minutes) to find recently closed
# issues. Idempotent — converting an already-related dep is a no-op.
#
# Becomes unnecessary when beads supports cross-rig computeBlockedIDs.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "cross-rig-deps"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/order_outcome.sh"

CITY="${GC_CITY:-.}"
LOOKBACK="${CROSS_RIG_LOOKBACK:-15m}"

# Step 1: Find recently closed issues.
# Use a fixed lookback window rather than tracking patrol time.
SINCE=$(date -u -d "-${LOOKBACK%m} minutes" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || \
        date -u -v-"${LOOKBACK%m}"M +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || exit 0

# Use the query language rather than --closed-after: HTTP-backed stores
# (e.g. maintainer-city) don't serve the listIssues ClosedAfter filter, but
# they do serve bd query. Dolt/local rigs support both; query gives one
# shape that works everywhere. --limit 0 asks for every match: bd before
# v1.3.1 caps an unflagged query at 50 rows even when piped, and the
# cross-rig dependents of any close past the cap would stay blocked with
# nothing logged.
#
# No `|| exit 0` here: a real bd refusal must fail the order with bd's own
# exit code (and its message must reach stderr), not look like "nothing
# closed recently". The one refusal that is not a failure is a city whose
# beads provider is not bd (file- or exec-backed): there is nothing for this
# order to do there, so the run is declared skipped. Only a genuine empty
# result exits 0 otherwise.
QUERY_ERR_FILE=$(mktemp)
QUERY_RC=0
CLOSED=$(gc bd query "status=closed AND closed>\"$SINCE\"" --json --limit 0 2>"$QUERY_ERR_FILE") || QUERY_RC=$?
QUERY_ERR=$(cat "$QUERY_ERR_FILE")
rm -f "$QUERY_ERR_FILE"
if [ "$QUERY_RC" -ne 0 ]; then
    case "$QUERY_ERR" in
        *"only supported for bd-backed beads providers"*)
            echo "cross-rig-deps: city is not a bd bead store; nothing to resolve there"
            order_outcome_scope_skipped city "not a bd bead store"
            order_outcome_set skipped "not a bd bead store"
            order_outcome_write
            exit 0
            ;;
    esac
fi
[ -z "$QUERY_ERR" ] || printf '%s\n' "$QUERY_ERR" >&2
[ "$QUERY_RC" -eq 0 ] || exit "$QUERY_RC"
if [ -z "$CLOSED" ] || [ "$CLOSED" = "[]" ]; then
    exit 0
fi

# Step 2: For each closed issue, check for cross-rig dependents.
# Capture jq output into variables (instead of piping into the loops) so
# producer failures still trip pipefail+set -e fail-loud, and the loop
# bodies run in the parent shell — RESOLVED is incremented in scope and
# survives to the summary echo below. CLOSED is pre-validated as a
# non-empty array above, so CLOSED_IDS is non-empty here.
RESOLVED=0
CLOSED_IDS=$(echo "$CLOSED" | jq -r '.[].id' 2>/dev/null)
# Bead-id prefixes of suspended rigs. A suspended rig is left cold: any bd
# read restarts its retired proxy, so its beads wait for it to resume.
SUSPENDED_PREFIXES=$(gc rig list --json 2>/dev/null \
    | jq -r '(.rigs // [])[] | select(.hq != true and .suspended == true) | .prefix' 2>/dev/null) || SUSPENDED_PREFIXES=""
while IFS= read -r closed_id; do
    if [ -n "$SUSPENDED_PREFIXES" ] && printf '%s\n' "$SUSPENDED_PREFIXES" | grep -qxF "${closed_id%%-*}"; then
        continue
    fi
    # Find beads that have a blocks dep on this closed issue.
    DEPS=$(gc bd dep list "$closed_id" --direction=up --type=blocks --json 2>/dev/null) || continue
    if [ -z "$DEPS" ] || [ "$DEPS" = "[]" ]; then
        continue
    fi

    # Filter for external (cross-rig) deps. The select() filter may yield
    # zero matches, in which case we skip rather than feed an empty
    # here-string into `read` (which would produce one bogus iteration
    # with dep_id="").
    EXTERNAL_DEPS=$(echo "$DEPS" | jq -r '.[] | select(.id | startswith("external:")) | .id' 2>/dev/null)
    if [ -z "$EXTERNAL_DEPS" ]; then
        continue
    fi
    while IFS= read -r dep_id; do
        if [ -n "$SUSPENDED_PREFIXES" ] && printf '%s\n' "$SUSPENDED_PREFIXES" | grep -qxF "${dep_id%%-*}"; then
            continue
        fi
        # Convert blocks → related: remove blocking semantics, keep audit trail.
        gc bd dep remove "$dep_id" "external:$closed_id" 2>/dev/null || true
        gc bd dep add "$dep_id" "external:$closed_id" --type=related 2>/dev/null || true
        RESOLVED=$((RESOLVED + 1))
    done <<< "$EXTERNAL_DEPS"
done <<< "$CLOSED_IDS"

if [ "$RESOLVED" -gt 0 ]; then
    echo "cross-rig-deps: resolved $RESOLVED cross-rig dependencies"
fi
