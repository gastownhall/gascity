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
# then every rig core_rig_names returns (it drops the hq pseudo-rig and
# suspended rigs — any bd read would restart a suspended rig's retired
# proxy and Dolt), each reached through scope_bd.sh's
# `gc bd --city <city> --rig <rig>` front door (the same route the reaper
# and jsonl-export orders use). Listing the rigs needs jq; a city with no
# rigs pays only the rig-list call.
#
# A timer check that fails in one store is reported on stderr naming the
# store; that store's gh check is skipped, the remaining stores are still
# swept, and the run exits non-zero at the end.
#
# Stores gc refuses — a scope whose beads provider is not bd (file- or
# exec-backed), or a rig with no path binding on this site — cannot hold
# bd gates; they are reported as skipped on stdout, not failed.
#
# A run that cannot list the rigs (jq missing, or `gc rig list` failing)
# still sweeps the city store, then reports on stderr that the rig stores
# were not checked and exits non-zero: the sweep never quietly narrows
# back to the city store.
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

# One stderr capture file for the run, removed on exit.
SWEEP_ERR_FILE="$(mktemp)"
trap 'rm -f "$SWEEP_ERR_FILE"' EXIT

# sweep_scope checks the timer and gh gates of the scope made current by
# scope_select. bd reports per-gate errors on stderr but still exits 0, so
# a non-zero timer exit means the store itself failed: name the store on
# stderr, skip its gh check (it would fail the same way, and its '|| true'
# would hide it) and let the sweep continue with the remaining stores. The
# run's exit status is decided once, at the end, from SWEEP_FAILED.
SWEEP_FAILED=0
sweep_scope() {
    local rc=0
    local err
    scope_bd gate check --type=timer --escalate 2>"$SWEEP_ERR_FILE" || rc=$?
    err="$(cat "$SWEEP_ERR_FILE")"
    printf '%s' "$err" >&2
    if [ "$rc" -ne 0 ]; then
        # gc refuses to point bd at two store shapes that cannot hold bd
        # gates: a scope whose beads provider is not bd, and a rig with no
        # path binding on this site. Skips, not failures — failing every
        # 30s for a permanent config shape would only leave a streak.
        case "$err" in
            *"only supported for bd-backed beads providers"*)
                echo "gate-sweep: $SCOPE_LABEL is not a bd bead store; no gates to check there"
                return 0
                ;;
            *"is declared but has no path binding"*)
                echo "gate-sweep: $SCOPE_LABEL has no path binding on this site; no gates to check there"
                return 0
                ;;
        esac
        echo "gate-sweep: timer gate check failed in $SCOPE_LABEL (exit $rc)" >&2
        SWEEP_FAILED=1
        return 0
    fi
    scope_bd gate check --type=gh --escalate || true
}

scope_select city
sweep_scope

# The rig listing needs jq (core_rig_names parses `gc rig list --json`
# with it); the city sweep above does not. Check it here, after the city
# sweep, so a city without jq still gets its own gates evaluated.
if ! command -v jq >/dev/null 2>&1; then
    echo "gate-sweep: jq is required to list rig stores but not found in PATH; rig stores were not checked this run" >&2
    exit 1
fi
if RIG_NAMES="$(core_rig_names)"; then
    while IFS= read -r rig_name; do
        [ -n "$rig_name" ] || continue
        scope_select rig "$rig_name"
        sweep_scope </dev/null
    done <<< "$RIG_NAMES"
else
    echo "gate-sweep: gc rig list failed; rig stores were not checked this run" >&2
    exit 1
fi

exit "$SWEEP_FAILED"
