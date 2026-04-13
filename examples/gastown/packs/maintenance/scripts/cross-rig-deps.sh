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
# Per-iteration `bd dep remove` / `bd dep add` calls are collected into
# a single `bd batch` invocation so all mutations commit in one dolt
# transaction (see beads#6). This eliminates the btrfs write thrashing
# caused by opening one dolt sql-server connection per operation.
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

# Step 2: For each closed issue, discover its cross-rig dependents and
# emit the remove+add pair into a batch stream. `bd dep list` is a read
# op not supported by `bd batch`, so dependency discovery stays outside
# the batched section; only the mutations are batched.
BATCH_FILE=$(mktemp)
trap 'rm -f "$BATCH_FILE"' EXIT

echo "$CLOSED" | jq -r '.[].id' 2>/dev/null | while IFS= read -r closed_id; do
    # Find beads that have a blocks dep on this closed issue.
    DEPS=$(bd dep list "$closed_id" --direction=up --type=blocks --json 2>/dev/null) || continue
    if [ -z "$DEPS" ] || [ "$DEPS" = "[]" ]; then
        continue
    fi

    # Filter for external (cross-rig) deps and emit batch commands.
    # Convert blocks → related: remove blocking semantics, keep audit trail.
    echo "$DEPS" | jq -r --arg closed "$closed_id" \
        '.[] | select(.id | startswith("external:")) | .id
         | "dep remove \(.) external:\($closed)\ndep add \(.) external:\($closed) related"' \
        2>/dev/null >> "$BATCH_FILE" || true
done

# Count the dep remove lines — each represents one resolved cross-rig dep.
# grep -c exits 1 on zero matches; suppress that so `set -e` is happy.
RESOLVED=$(grep -c '^dep remove ' "$BATCH_FILE" 2>/dev/null) || RESOLVED=0

if [ "$RESOLVED" -gt 0 ]; then
    # Apply all mutations as a single transaction. On failure, bd batch
    # rolls back every op and exits non-zero; `set -e` propagates it.
    if ! bd batch -f "$BATCH_FILE" -m "cross-rig-deps sweep"; then
        echo "cross-rig-deps: bd batch failed — no dependencies were converted" >&2
        exit 1
    fi
    echo "cross-rig-deps: resolved $RESOLVED cross-rig dependencies"
fi
