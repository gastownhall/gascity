#!/usr/bin/env bash
# gate-sweep — evaluate and close pending gates.
#
# Replaces the deacon patrol check-gates step. All gate evaluation is
# deterministic: timer gates are timestamp comparison, condition gates
# are exit code checks, GitHub gates are API status queries.
#
# Runs as an exec order (no LLM, no agent, no wisp).
#
# See lx-qfq5r1 / lx-f2z2ph for the hardening pass — flock, bd_safe, trap.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-bd-safe.sh
. "$SCRIPT_DIR/lib-bd-safe.sh"
install_trap
acquire_lock

CITY="${GC_CITY:-.}"

# Step 1: Close elapsed timer gates.
# bd gate check evaluates all open gate beads, closes those past their
# timeout, and prints a summary. --escalate sends mail for expired gates.
bd_safe gate check --type=timer --escalate 2>/dev/null || true

# Step 2: Evaluate condition gates.
# For each open condition gate, run its check command. Close if exit 0.
CONDITION_GATES=$(bd_safe gate list --type=condition --status=open --json 2>/dev/null) || CONDITION_GATES=""
if [ -n "$CONDITION_GATES" ] && [ "$CONDITION_GATES" != "[]" ]; then
    COND_LINES=$(echo "$CONDITION_GATES" | jq -r '.[] | "\(.id)\t\(.metadata.check)"' 2>/dev/null) || COND_LINES=""
    if [ -n "$COND_LINES" ]; then
        while IFS=$'\t' read -r gate_id check_cmd; do
            if [ -n "$check_cmd" ] && eval "$check_cmd" >/dev/null 2>&1; then
                bd_safe gate close "$gate_id" --reason "condition satisfied" 2>/dev/null || true
            fi
        done <<<"$COND_LINES"
    fi
fi

# Step 3: Evaluate GitHub gates (gh:run, gh:pr).
# For each open GitHub gate, check the workflow/PR status and close if done.
GH_GATES=$(bd_safe gate list --type=gh --status=open --json 2>/dev/null) || GH_GATES=""
if [ -n "$GH_GATES" ] && [ "$GH_GATES" != "[]" ]; then
    GH_LINES=$(echo "$GH_GATES" | jq -r '.[] | "\(.id)\t\(.metadata.await_type)\t\(.metadata.ref)"' 2>/dev/null) || GH_LINES=""
    if [ -n "$GH_LINES" ]; then
        while IFS=$'\t' read -r gate_id await_type ref; do
            case "$await_type" in
                gh:run)
                    STATUS=$(gh run view "$ref" --json status -q .status 2>/dev/null) || continue
                    if [ "$STATUS" = "completed" ]; then
                        CONCLUSION=$(gh run view "$ref" --json conclusion -q .conclusion 2>/dev/null)
                        bd_safe gate close "$gate_id" --reason "workflow $CONCLUSION" 2>/dev/null || true
                    fi
                    ;;
                gh:pr)
                    STATE=$(gh pr view "$ref" --json state -q .state 2>/dev/null) || continue
                    if [ "$STATE" = "MERGED" ] || [ "$STATE" = "CLOSED" ]; then
                        bd_safe gate close "$gate_id" --reason "PR $STATE" 2>/dev/null || true
                    fi
                    ;;
            esac
        done <<<"$GH_LINES"
    fi
fi
