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

CITY="${GC_CITY:-.}"
LOOKBACK="${CROSS_RIG_LOOKBACK:-15m}"

# Step 1: Find recently closed issues.
# Use a fixed lookback window rather than tracking patrol time.
SINCE=$(date -u -d "-${LOOKBACK%m} minutes" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || \
        date -u -v-"${LOOKBACK%m}"M +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || exit 0

CLOSED=$(bd list --status=closed --closed-after="$SINCE" --json 2>/dev/null) || exit 0
if [ -z "$CLOSED" ] || [ "$CLOSED" = "[]" ]; then
    exit 0
fi

# Step 2: For each closed issue, check for cross-rig dependents.
RESOLVED=0
echo "$CLOSED" | jq -r '.[].id' 2>/dev/null | while IFS= read -r closed_id; do
    # Find beads that have a blocks dep on this closed issue.
    DEPS=$(bd dep list "$closed_id" --direction=up --type=blocks --json 2>/dev/null) || continue
    if [ -z "$DEPS" ] || [ "$DEPS" = "[]" ]; then
        continue
    fi

    # Filter for external (cross-rig) deps.
    echo "$DEPS" | jq -r '.[] | select(.id | startswith("external:")) | .id' 2>/dev/null | while IFS= read -r dep_id; do
        # Convert blocks → related: remove blocking semantics, keep audit trail.
        bd dep remove "$dep_id" "external:$closed_id" 2>/dev/null || true
        bd dep add "$dep_id" "external:$closed_id" --type=related 2>/dev/null || true
        RESOLVED=$((RESOLVED + 1))
    done
done

if [ "$RESOLVED" -gt 0 ]; then
    echo "cross-rig-deps: resolved $RESOLVED cross-rig dependencies"
fi
