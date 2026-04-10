#!/usr/bin/env bash
# spawn-storm-detect — find beads stuck in a recovery loop.
#
# Scans recent bead.updated events for the "reset to pool" signature
# (status=open, assignee cleared). Counts resets per bead. When any
# bead exceeds the threshold, escalates to mayor via mail.
#
# State file tracks cumulative reset counts across runs. Closed beads
# are pruned from the ledger automatically.
#
# Runs as an exec order (no LLM, no agent, no wisp).
#
# NOTE on `bd batch` (beads#6): this script's loops call `bd show` and
# `gc mail send` per iteration — neither is supported by `bd batch`
# (which only handles close/update/create/dep add/dep remove). The
# ledger-prune loop does no `bd` calls at all, just local jq. There is
# therefore nothing here to fold into a batch transaction.
set -euo pipefail

CITY="${GC_CITY:-.}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/maintenance}"
LEDGER="$PACK_STATE_DIR/spawn-storm-counts.json"
THRESHOLD="${SPAWN_STORM_THRESHOLD:-2}"

if [ ! -e "$LEDGER" ] && [ -e "$CITY/.gc/spawn-storm-counts.json" ]; then
    LEDGER="$CITY/.gc/spawn-storm-counts.json"
fi
mkdir -p "$(dirname "$LEDGER")"

# Initialize ledger if missing.
if [ ! -f "$LEDGER" ]; then
    echo '{}' > "$LEDGER"
fi

# Step 1: Find beads that were recently reset to pool.
# Look for open beads that have been updated (recovery resets them to open + unassigned).
OPEN_BEADS=$(bd list --status=open --assignee="" --json --limit=0 2>/dev/null) || exit 0
if [ -z "$OPEN_BEADS" ] || [ "$OPEN_BEADS" = "[]" ]; then
    exit 0
fi

# Step 2: Load current ledger.
COUNTS=$(cat "$LEDGER")

# Step 3: For each open unassigned bead, check if it has rejection metadata
# (indicates it was returned from refinery or recovered by witness).
STORMS=0
echo "$OPEN_BEADS" | jq -r '.[] | select(.metadata.rejection_reason != null or .metadata.recovered != null) | .id' 2>/dev/null | while IFS= read -r bead_id; do
    [ -z "$bead_id" ] && continue

    # Increment count for this bead.
    PREV=$(echo "$COUNTS" | jq -r --arg id "$bead_id" '.[$id] // 0')
    NEW=$((PREV + 1))
    COUNTS=$(echo "$COUNTS" | jq --arg id "$bead_id" --argjson n "$NEW" '.[$id] = $n')

    if [ "$NEW" -ge "$THRESHOLD" ]; then
        TITLE=$(bd show "$bead_id" --json 2>/dev/null | jq -r '.title // "unknown"')
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
done

# Step 4: Prune closed beads from ledger.
CLOSED_IDS=$(bd list --status=closed --json --limit=0 2>/dev/null | jq -r '.[].id' 2>/dev/null) || true
for cid in $CLOSED_IDS; do
    COUNTS=$(echo "$COUNTS" | jq --arg id "$cid" 'del(.[$id])' 2>/dev/null) || true
done

# Step 5: Save updated ledger.
echo "$COUNTS" > "$LEDGER"

if [ "$STORMS" -gt 0 ]; then
    echo "spawn-storm-detect: found $STORMS beads exceeding reset threshold"
fi
