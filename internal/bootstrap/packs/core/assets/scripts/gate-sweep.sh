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
# Gates live in every bead store, so the sweep checks the city store and then
# each rig store core_rig_names (scope_bd.sh) lists. The hq pseudo-rig is the
# city store, and suspended rigs are left out because any bd read restarts a
# suspended rig's retired proxy and Dolt; their expired timers resolve on the
# first sweep after resume. Listing the rigs costs one `gc rig list --json`
# (parsed with jq) on every run, even in a city with no rigs. A timer check
# that fails in one store is reported on stderr naming the store; the other
# stores are still swept and the run exits non-zero.
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "gate-sweep"

CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
# CITY_ABS is read by scope_bd.sh.
# shellcheck disable=SC2034
CITY_ABS="$(cd "$CITY" 2>/dev/null && pwd -P || printf '%s\n' "$CITY")"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/scope_bd.sh"

FAILED=0

# sweep_scope checks the timer and gh gates of the scope scope_select made
# current. bd reports per-gate errors and still exits 0, so a failing timer
# check means the store itself failed: it is reported, the store's gh check is
# skipped (it would fail the same way behind its `|| true`), and the run fails
# after the remaining stores are swept.
sweep_scope() {
    local rc=0
    scope_bd gate check --type=timer --escalate || rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "gate-sweep: timer gate check failed in $SCOPE_LABEL (exit $rc)" >&2
        FAILED=1
        return 0
    fi
    scope_bd gate check --type=gh --escalate || true
}

scope_select city
sweep_scope

RIG_NAMES=$(core_rig_names)
while IFS= read -r rig_name; do
    [ -n "$rig_name" ] || continue
    scope_select rig "$rig_name"
    # Nothing a sweep spawns may read the remaining rig names.
    sweep_scope </dev/null
done <<<"$RIG_NAMES"

exit "$FAILED"
