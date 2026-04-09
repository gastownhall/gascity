#!/usr/bin/env bash
# spawn-storm-detect — find beads stuck in a recovery loop.
#
# Scans recent bead.updated events for the "reset to pool" signature
# (status=open, assignee cleared). Counts resets per bead. When any
# bead exceeds the threshold, escalates to mayor via mail.
#
# State files track cumulative reset counts and the last-run timestamp
# across runs. Closed beads are pruned from the ledger automatically.
#
# Runs as an exec order (no LLM, no agent, no wisp).
#
# See lx-qfq5r1 / lx-f2z2ph for the incident that drove the hardening
# pass (bounded queries, single-jq pruning, flock, bd_safe, trap).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-bd-safe.sh
. "$SCRIPT_DIR/lib-bd-safe.sh"
install_trap
acquire_lock

CITY="${GC_CITY:-.}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/maintenance}"
LEDGER="$PACK_STATE_DIR/spawn-storm-counts.json"
LAST_RUN_FILE="$PACK_STATE_DIR/spawn-storm-last-run"
THRESHOLD="${SPAWN_STORM_THRESHOLD:-2}"
MAX_BEADS="${SPAWN_STORM_MAX_BEADS:-500}"

if [ ! -e "$LEDGER" ] && [ -e "$CITY/.gc/spawn-storm-counts.json" ]; then
    LEDGER="$CITY/.gc/spawn-storm-counts.json"
fi
mkdir -p "$(dirname "$LEDGER")"

# Initialize ledger if missing.
if [ ! -f "$LEDGER" ]; then
    echo '{}' > "$LEDGER"
fi

# Record the start time NOW so next run's --updated-after window covers
# the full duration of this run (minor overlap is safe; missing events
# is not).
START_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Load last-run timestamp. Default to a 24h look-back on first run so
# we don't flood Dolt with an unbounded scan of history.
if [ -f "$LAST_RUN_FILE" ]; then
    LAST_RUN_TS=$(cat "$LAST_RUN_FILE")
fi
if [ -z "${LAST_RUN_TS:-}" ]; then
    LAST_RUN_TS=$(date -u -d "-24 hours" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
        || date -u -v-24H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || exit 0
fi

# Step 1: Find beads recently reset to pool.
# Bounded by --updated-after LAST_RUN_TS so we only look at fresh reset
# events, not every open+unassigned bead in the database (the old
# --limit=0 scan drove Dolt to 250% CPU during incident lx-wisp-ece).
OPEN_BEADS=$(bd_safe list --status=open --assignee="" \
    --updated-after "$LAST_RUN_TS" --json --limit="$MAX_BEADS" 2>/dev/null) || OPEN_BEADS="[]"
if [ -z "$OPEN_BEADS" ]; then
    OPEN_BEADS="[]"
fi

# Step 2: Load current ledger.
COUNTS=$(cat "$LEDGER")

# Step 3: For each open unassigned bead with rejection/recovery metadata,
# increment its count and escalate if over threshold. Filter once with
# jq; iterate in the main shell (here-string, not pipe) so COUNTS
# updates persist across iterations.
STORMS=0
STORM_IDS=$(echo "$OPEN_BEADS" | jq -r '.[] | select(.metadata.rejection_reason != null or .metadata.recovered != null) | .id' 2>/dev/null) || STORM_IDS=""
if [ -n "$STORM_IDS" ]; then
    while IFS= read -r bead_id; do
        [ -z "$bead_id" ] && continue

        PREV=$(echo "$COUNTS" | jq -r --arg id "$bead_id" '.[$id] // 0')
        NEW=$((PREV + 1))
        COUNTS=$(echo "$COUNTS" | jq --arg id "$bead_id" --argjson n "$NEW" '.[$id] = $n')

        if [ "$NEW" -ge "$THRESHOLD" ]; then
            TITLE=$(bd_safe show "$bead_id" --json 2>/dev/null | jq -r '.title // "unknown"') || TITLE="unknown"
            gc mail send mayor/ \
                -s "SPAWN_STORM: bead $bead_id reset ${NEW}x" \
                -m "Bead $bead_id ($TITLE) has been reset to pool $NEW times (threshold: $THRESHOLD).
This likely indicates a polecat crash loop on this specific work.

Recommended actions:
- Inspect the bead: bd show $bead_id --json
- Check rejection history: metadata.rejection_reason
- Consider quarantining the bead or investigating the root cause." \
                2>/dev/null || true
            STORMS=$((STORMS + 1))
        fi
    done <<<"$STORM_IDS"
fi

# Step 4: Prune recently-closed beads from the ledger. Bounded by the
# same --updated-after window so we don't scan all closed history.
# Uses a single jq invocation instead of the prior O(n^2) shell loop.
CLOSED_BEADS=$(bd_safe list --status=closed \
    --updated-after "$LAST_RUN_TS" --json --limit="$MAX_BEADS" 2>/dev/null) || CLOSED_BEADS="[]"
if [ -z "$CLOSED_BEADS" ]; then
    CLOSED_BEADS="[]"
fi
if [ "$CLOSED_BEADS" != "[]" ]; then
    COUNTS=$(jq -n --argjson counts "$COUNTS" --argjson closed "$CLOSED_BEADS" \
        '$counts | reduce ($closed | map(.id))[] as $id (.; del(.[$id]))')
fi

# Step 5: Save updated ledger and record run timestamp. The LAST_RUN_TS
# we record is the time we captured BEFORE running any queries, so next
# run's window always overlaps slightly with this one (at-least-once
# semantics — we'd rather double-count than miss a reset event).
echo "$COUNTS" > "$LEDGER"
echo "$START_TS" > "$LAST_RUN_FILE"

if [ "$STORMS" -gt 0 ]; then
    echo "spawn-storm-detect: found $STORMS beads exceeding reset threshold"
fi
