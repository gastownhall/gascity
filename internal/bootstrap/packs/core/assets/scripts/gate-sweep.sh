#!/usr/bin/env bash
# gate-sweep — evaluate and close pending gates.
#
# Runs as an exec order (no LLM, no agent, no wisp). bd dispatches per
# type. The `|| true` on the gh-gate line is load-bearing: bd shells
# out to `gh` for gh:run / gh:pr gates, and fresh cities without
# `gh auth` would otherwise fail this order on every 30s cooldown.
# bd's combined output reaches the controller log only on non-zero
# exit (see the `if err != nil` branch of `dispatchOne` in
# cmd/gc/order_dispatch.go), so suppressing gh-gate errors also
# hides real bd errors on that line — diagnose by hand.
#
# Timer-gate evaluation is local-only (no `gh` shell-out, no auth
# requirement) so its failures should propagate to the controller log.
# `|| true` would silently mask real bd regressions in timer-gate
# evaluation — see #1734 for the rationale.
#
# Bead-type gates are skipped: in beads v1.0.2, checkBeadGate is
# hard-coded to fail because cross-rig routing was removed upstream.
# Restore `gc bd gate check --type=bead --escalate` when beads adds it back.
#
# Every bead store is swept, not just the city's: the city store first,
# then every rig `gc rig list --json` reports, each reached through
# scope_bd.sh's `gc bd --city <city> --rig <rig>` front door (the same
# route the reaper and jsonl-export orders use). Listing the rigs needs
# jq; a city with no rigs pays only the rig-list call.
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "gate-sweep"

# CITY_ABS is read by scope_bd.sh.
CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
# shellcheck disable=SC2034
CITY_ABS="$(cd "$CITY" 2>/dev/null && pwd -P || printf '%s\n' "$CITY")"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/scope_bd.sh"

# sweep_scope checks the timer and gh gates of the scope made current by
# scope_select.
sweep_scope() {
    scope_bd gate check --type=timer --escalate
    scope_bd gate check --type=gh --escalate || true
}

scope_select city
sweep_scope

RIG_NAMES="$(cd "$CITY_ABS" && gc rig list --json | jq -r '.rigs[]? | .name // empty')"
while IFS= read -r rig_name; do
    [ -n "$rig_name" ] || continue
    scope_select rig "$rig_name"
    sweep_scope </dev/null
done <<< "$RIG_NAMES"
