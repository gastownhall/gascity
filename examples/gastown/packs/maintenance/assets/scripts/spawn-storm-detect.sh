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
RESET_IDS=$(echo "$OPEN_BEADS" | jq -r '.[] | select(.metadata.rejection_reason != null or .metadata.recovered != null) | .id' 2>/dev/null)
while IFS= read -r bead_id; do
    [ -z "$bead_id" ] && continue

    # Increment count for this bead.
    PREV=$(echo "$COUNTS" | jq -r --arg id "$bead_id" '.[$id] // 0')
    NEW=$((PREV + 1))
    COUNTS=$(echo "$COUNTS" | jq --arg id "$bead_id" --argjson n "$NEW" '.[$id] = $n')

    if [ "$NEW" -ge "$THRESHOLD" ]; then
        TITLE_JSON=$(bd show "$bead_id" --json 2>/dev/null || true)
        TITLE=$(echo "$TITLE_JSON" | jq -r 'if type == "array" then (.[0].title // "unknown") else "unknown" end' 2>/dev/null || echo "unknown")
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
done <<< "$RESET_IDS"

# Step 4: Prune closed beads from ledger.
# Only check beads actually tracked in the ledger (avoids expensive full scan
# of all closed beads via bd list --status=closed --limit=0).
TRACKED_IDS=$(echo "$COUNTS" | jq -r 'keys[]' 2>/dev/null) || true
while IFS= read -r tid; do
    [ -z "$tid" ] && continue
    if BEAD_OUTPUT=$(bd show "$tid" --json 2>&1); then
        BEAD_STATUS=$(echo "$BEAD_OUTPUT" | jq -r 'if type == "array" then (.[0].status // "deleted") elif type == "object" and ((.error // "") | test("not found|no issue found"; "i")) then "deleted" else "unknown" end' 2>/dev/null || echo "unknown")
    elif echo "$BEAD_OUTPUT" | grep -qiE 'not found|no issue found'; then
        BEAD_STATUS="deleted"
    else
        BEAD_STATUS="unknown"
    fi
    if [ "$BEAD_STATUS" = "closed" ] || [ "$BEAD_STATUS" = "deleted" ]; then
        COUNTS=$(echo "$COUNTS" | jq --arg id "$tid" 'del(.[$id])' 2>/dev/null) || true
    fi
done <<< "$TRACKED_IDS"

# Step 5: Save updated ledger.
echo "$COUNTS" > "$LEDGER"

if [ "$STORMS" -gt 0 ]; then
    echo "spawn-storm-detect: found $STORMS beads exceeding reset threshold"
fi
