#!/usr/bin/env bash
# gate-sweep — evaluate and close pending gates.
#
# Replaces the deacon patrol check-gates step. All gate evaluation is
# deterministic: timer gates are timestamp comparison, condition gates
# are exit code checks, GitHub gates are API status queries.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

CITY="${GC_CITY:-.}"

# Step 1: Close elapsed timer gates.
# bd gate check evaluates all open gate beads, closes those past their
# timeout, and prints a summary. --escalate sends mail for expired gates.
bd gate check --type=timer --escalate 2>/dev/null || true

# Step 2: Evaluate condition gates.
# For each open condition gate, run its check command. Close if exit 0.
CONDITION_GATES=$(bd gate list --type=condition --status=open --json 2>/dev/null) || true
if [ -n "$CONDITION_GATES" ] && [ "$CONDITION_GATES" != "[]" ]; then
    echo "$CONDITION_GATES" | jq -r '.[] | "\(.id)\t\(.metadata.check)"' 2>/dev/null | while IFS=$'\t' read -r gate_id check_cmd; do
        if [ -n "$check_cmd" ] && eval "$check_cmd" >/dev/null 2>&1; then
            bd gate close "$gate_id" --reason "condition satisfied" 2>/dev/null || true
        fi
    done
fi

# Step 3: Evaluate GitHub gates (gh:run, gh:pr).
# For each open GitHub gate, check the workflow/PR status and close if done.
GH_GATES=$(bd gate list --type=gh --status=open --json 2>/dev/null) || true
if [ -n "$GH_GATES" ] && [ "$GH_GATES" != "[]" ]; then
    echo "$GH_GATES" | jq -r '.[] | "\(.id)\t\(.metadata.await_type)\t\(.metadata.ref)"' 2>/dev/null | while IFS=$'\t' read -r gate_id await_type ref; do
        case "$await_type" in
            gh:run)
                STATUS=$(gh run view "$ref" --json status -q .status 2>/dev/null) || continue
                if [ "$STATUS" = "completed" ]; then
                    CONCLUSION=$(gh run view "$ref" --json conclusion -q .conclusion 2>/dev/null)
                    bd gate close "$gate_id" --reason "workflow $CONCLUSION" 2>/dev/null || true
                fi
                ;;
            gh:pr)
                STATE=$(gh pr view "$ref" --json state -q .state 2>/dev/null) || continue
                if [ "$STATE" = "MERGED" ] || [ "$STATE" = "CLOSED" ]; then
                    bd gate close "$gate_id" --reason "PR $STATE" 2>/dev/null || true
                fi
                ;;
        esac
    done
fi
