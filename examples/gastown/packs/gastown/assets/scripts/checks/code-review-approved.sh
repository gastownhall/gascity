#!/usr/bin/env bash
# Ralph check script for code review loop (personal work formula).
#
# Reads the code review verdict from bead metadata.
# Exit 0 = pass (stop iterating), exit 1 = fail (retry).
#
# Expected metadata key: code_review.verdict
# Values: "done" | "iterate"

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
                    [ .[] | select(.metadata["gc.step_ref"] == $ref and .metadata["gc.root_bead_id"] == $root) | .metadata["code_review.verdict"] ] | first // ""
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

APPLY_REF="mol-personal-work-v2.code-review-loop.run.${ATTEMPT}.apply-code-fixes"
VERDICT=$(load_verdict "$APPLY_REF" "$ROOT_ID")

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
