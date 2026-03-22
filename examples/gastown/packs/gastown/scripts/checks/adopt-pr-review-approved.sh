#!/usr/bin/env bash
# Ralph check script for adopt-pr review loop.
#
# Reads the review verdict from the apply-fixes step's bead metadata.
# Exit 0 = pass (stop iterating), exit 1 = fail (retry with next attempt).
#
# Expected metadata key: review.verdict
# Values: "done" (approved) | "iterate" (needs another round)
#
# The apply-fixes step sets this after applying synthesis findings:
#   bd meta set $BEAD_ID review.verdict=done
#   bd meta set $BEAD_ID review.verdict=iterate

set -euo pipefail

BEAD_ID="${GC_BEAD_ID:-}"
if [ -z "$BEAD_ID" ]; then
    echo "ERROR: GC_BEAD_ID not set" >&2
    exit 1
fi

# Read the verdict from the run attempt's parent bead metadata
VERDICT=$(bd show "$BEAD_ID" --json 2>/dev/null | jq -r '.metadata["review.verdict"] // "iterate"')

case "$VERDICT" in
    done|approved|pass)
        echo "Review approved — stopping iteration"
        exit 0
        ;;
    iterate|fail|retry)
        echo "Review needs iteration — retrying"
        exit 1
        ;;
    *)
        echo "Unknown verdict: $VERDICT — treating as iterate" >&2
        exit 1
        ;;
esac
