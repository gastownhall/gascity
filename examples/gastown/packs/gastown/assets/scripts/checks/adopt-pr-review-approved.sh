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

# json_payload strips any non-JSON prefix lines (e.g., `bd`'s
# `warning: beads.role not configured` diagnostic, which is emitted on
# stdout before the real payload). Without this, jq fails to parse the
# combined stdout and the script exits with empty output and status 1,
# which has produced flaky CI failures for
# TestReviewCheckScriptsPreferNewestVerdictAcrossRalphStep.
json_payload() {
    awk 'found || /^[[:space:]]*[[{]/{ found=1; print }'
}

bd_json() {
    local attempt=0
    local output=""
    while [ "$attempt" -lt 10 ]; do
        if output=$(bd "$@" 2>/dev/null | json_payload) && [ -n "$output" ]; then
            printf '%s\n' "$output"
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 0.2
    done
    return 1
}

load_verdict() {
    local apply_ref="$1"
    local root_id="$2"
    local current=""
    local previous=""
    local stable_run=0
    local attempt=0
    # Total budget: 10 * 200ms = 2s. Keep sampling until two consecutive
    # reads return the same verdict (stable), which guarantees we've
    # observed everything the store will make visible within the budget.
    # Returning early on the first non-empty verdict (the old behavior)
    # created a race against the bead store when the "newest verdict
    # wins" invariant mattered — see
    # TestReviewCheckScriptsPreferNewestVerdictAcrossRalphStep. If no
    # verdict ever materializes, return "iterate" as the safe default.
    while [ "$attempt" -lt 10 ]; do
        current=$(
            bd list --all --json --limit=0 2>/dev/null |
                json_payload |
                jq -r --arg ref "$apply_ref" --arg root "$root_id" '
                    [
                        .[]
                        | select(.metadata["gc.step_ref"] == $ref and .metadata["gc.root_bead_id"] == $root)
                        | {
                            verdict: .metadata["review.verdict"],
                            timestamp: (.updated_at // .created_at // ""),
                            id: (.id // "")
                        }
                        | select(.verdict != null and .verdict != "")
                    ]
                    | sort_by(.timestamp, .id)
                    | .[-1].verdict // ""
                ' 2>/dev/null
        ) || current=""
        if [ -n "$current" ] && [ "$current" = "$previous" ]; then
            stable_run=$((stable_run + 1))
            if [ "$stable_run" -ge 1 ]; then
                printf '%s\n' "$current"
                return 0
            fi
        else
            stable_run=0
        fi
        previous="$current"
        attempt=$((attempt + 1))
        sleep 0.2
    done

    if [ -n "$current" ]; then
        printf '%s\n' "$current"
        return 0
    fi
    printf 'iterate\n'
}

BEAD_ID="${GC_BEAD_ID:-}"
if [ -z "$BEAD_ID" ]; then
    echo "ERROR: GC_BEAD_ID not set" >&2
    exit 1
fi

BEAD_JSON=$(bd_json show "$BEAD_ID" --json)
ATTEMPT=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.attempt"] // "") else (.metadata["gc.attempt"] // "") end' 2>/dev/null || printf '')
ROOT_ID=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.root_bead_id"] // "") else (.metadata["gc.root_bead_id"] // "") end' 2>/dev/null || printf '')
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
