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

load_verdict() {
    local apply_ref="$1"
    local root_id="$2"
    local verdict=""
    local attempt=0

    while [ "$attempt" -lt 5 ]; do
        verdict=$(
            bd list --all --json --limit=0 2>/dev/null |
                jq -r --arg ref "$apply_ref" --arg root "$root_id" '
                    [ .[] | select(.metadata["gc.step_ref"] == $ref and .metadata["gc.root_bead_id"] == $root) | .metadata["review.verdict"] ] | first // ""
                ' 2>/dev/null
        ) || verdict=""
        if [ -n "$verdict" ]; then
            printf '%s\n' "$verdict"
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 0.2
    done

    printf 'iterate\n'
}

BEAD_ID="${GC_BEAD_ID:-}"
if [ -z "$BEAD_ID" ]; then
    echo "ERROR: GC_BEAD_ID not set" >&2
    exit 1
fi

BEAD_JSON=$(bd show "$BEAD_ID" --json 2>/dev/null)
ATTEMPT=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.attempt"] // "") else (.metadata["gc.attempt"] // "") end')
ROOT_ID=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.root_bead_id"] // "") else (.metadata["gc.root_bead_id"] // "") end')
if [ -z "$ATTEMPT" ] || [ -z "$ROOT_ID" ]; then
    echo "ERROR: missing gc.attempt or gc.root_bead_id on $BEAD_ID" >&2
    exit 1
fi

APPLY_REF="mol-adopt-pr-v2.review-loop.run.${ATTEMPT}.apply-fixes"
VERDICT=$(load_verdict "$APPLY_REF" "$ROOT_ID")

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
