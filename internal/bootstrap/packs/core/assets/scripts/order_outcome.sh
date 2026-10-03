#!/usr/bin/env bash
# order_outcome.sh — typed skipped/partial outcome for exec orders.
#
# An exec order that exits 0 is recorded as order.completed. When part of the
# work could not run (a scope bd could not reach, a safety gate that held a
# step back) the order says so here, and the controller turns the declaration
# into a typed order.skipped event. The controller passes the path in
# GC_ORDER_OUTCOME_FILE; when it is unset (a manual run) the helpers only
# accumulate and order_outcome_write is a no-op.
#
# Contract (decoded strictly by cmd/gc/order_outcome.go):
#   {"outcome":"skipped"|"partial","reason":"<text>",
#    "scopes":[{"scope":"<label>","reason":"<text>"}]}
#
# "skipped": nothing the order exists to do ran. "partial": some of it ran.

ORDER_OUTCOME_SCOPES_JSON="[]"
ORDER_OUTCOME_KIND=""
ORDER_OUTCOME_REASON=""

# order_outcome_scope_skipped <scope-label> <reason> records one scope or step
# that did not run.
order_outcome_scope_skipped() {
    ORDER_OUTCOME_SCOPES_JSON=$(printf '%s' "$ORDER_OUTCOME_SCOPES_JSON" |
        jq -c --arg scope "$1" --arg reason "$2" '. + [{scope: $scope, reason: $reason}]')
}

# order_outcome_set <skipped|partial> <reason> sets the overall outcome.
# "skipped" wins over "partial": once nothing ran, a later partial note does
# not downgrade it.
order_outcome_set() {
    if [ "$ORDER_OUTCOME_KIND" = "skipped" ] && [ "$1" = "partial" ]; then
        return 0
    fi
    ORDER_OUTCOME_KIND="$1"
    ORDER_OUTCOME_REASON="$2"
}

# order_outcome_write writes the declaration, if any, to GC_ORDER_OUTCOME_FILE.
# A recorded scope without an explicit outcome makes the run "partial".
order_outcome_write() {
    local target="${GC_ORDER_OUTCOME_FILE:-}"
    if [ -z "$ORDER_OUTCOME_KIND" ] && [ "$ORDER_OUTCOME_SCOPES_JSON" != "[]" ]; then
        ORDER_OUTCOME_KIND="partial"
        ORDER_OUTCOME_REASON="some scopes or steps did not run"
    fi
    [ -n "$ORDER_OUTCOME_KIND" ] || return 0
    [ -n "$target" ] || return 0
    jq -n -c \
        --arg outcome "$ORDER_OUTCOME_KIND" \
        --arg reason "$ORDER_OUTCOME_REASON" \
        --argjson scopes "$ORDER_OUTCOME_SCOPES_JSON" \
        '{outcome: $outcome, reason: $reason, scopes: $scopes}' >"$target"
}
