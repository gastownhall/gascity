#!/usr/bin/env bash
# Ralph check script for code review loop (personal work formula).
#
# Reads the code review verdict from bead metadata.
# Exit 0 = pass (stop iterating), exit 1 = fail (retry).
#
# Expected metadata key: code_review.verdict
# Values: "done" | "iterate"

set -euo pipefail

BEAD_ID="${GC_BEAD_ID:-}"
if [ -z "$BEAD_ID" ]; then
    echo "ERROR: GC_BEAD_ID not set" >&2
    exit 1
fi

VERDICT=$(bd show "$BEAD_ID" --json 2>/dev/null | jq -r '.metadata["code_review.verdict"] // "iterate"')

case "$VERDICT" in
    done|approved|pass)
        echo "Code review approved — stopping iteration"
        exit 0
        ;;
    iterate|fail|retry)
        echo "Code review needs iteration — retrying"
        exit 1
        ;;
    *)
        echo "Unknown verdict: $VERDICT — treating as iterate" >&2
        exit 1
        ;;
esac
