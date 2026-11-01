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
# Every bead store is swept: the city's, then each rig's. `gc bd gate
# check` with no --rig reaches the city store only, so a timer or gh gate
# in a rig's store would otherwise stay open past its deadline and wake
# no one. The rigs are scope_bd.sh's core_rig_names, the same selection
# the other maintenance orders use: the hq pseudo-rig (the city store,
# already swept) and suspended rigs are left out. A rig whose beads
# provider is not bd (gc refuses `gc bd` there) has no bd gates and is
# skipped, as reaper.sh and jsonl-export.sh skip it. A timer check that
# fails in one store does not stop the sweep of the stores after it; the
# run exits non-zero at the end, naming the store. So does a rig list
# that cannot be read (or a missing jq), after the city store is swept.
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

rc=0
err_file="$(mktemp)"
trap 'rm -f "$err_file"' EXIT

# sweep_scope <label> [--rig <rig>] checks one store's timer and gh gates.
sweep_scope() {
    local label="$1"
    local timer_ok=1
    shift
    gc bd "$@" gate check --type=timer --escalate 2>"$err_file" || timer_ok=0
    cat "$err_file" >&2
    if [ "$timer_ok" -eq 0 ]; then
        # A rig whose store is not bd-backed is refused by gc before bd runs.
        if [ "$#" -gt 0 ] && grep -qF "only supported for bd-backed beads providers" "$err_file"; then
            echo "gate-sweep: $label is not a bd bead store; no gates to check there"
            return 0
        fi
        echo "gate-sweep: timer gate check failed in $label" >&2
        rc=1
    fi
    gc bd "$@" gate check --type=gh --escalate || true
}

sweep_scope "the city store"

if ! command -v jq >/dev/null 2>&1; then
    echo "gate-sweep: jq is required to list rigs but not found in PATH; rig gates were not checked" >&2
    exit 1
fi
if ! rigs="$(core_rig_names)"; then
    echo "gate-sweep: gc rig list failed; rig gates were not checked" >&2
    exit 1
fi
while IFS= read -r rig; do
    [ -n "$rig" ] || continue
    sweep_scope "rig $rig" --rig "$rig"
done <<EOF
$rigs
EOF

exit "$rc"
