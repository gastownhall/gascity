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
# stores are still swept and the run exits non-zero. A store gc will not
# scope for bd is skipped with a notice and does not fail the run.
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
ERR_FILE=$(mktemp)
trap 'rm -f "$ERR_FILE"' EXIT

# sweep_scope checks the timer and gh gates of the scope scope_select made
# current. A store gc will not point bd at (a non-bd beads provider, or a rig
# with no path binding on this site) holds no bd gates, so it is named as
# skipped rather than failed. bd reports per-gate errors and still exits 0, so
# any other failing timer check means the store itself failed: it is reported,
# the store's gh check is skipped (it would fail the same way behind its
# `|| true`), and the run fails after the remaining stores are swept.
sweep_scope() {
    local rc=0
    scope_bd gate check --type=timer --escalate 2>"$ERR_FILE" || rc=$?
    cat "$ERR_FILE" >&2
    if [ "$rc" -ne 0 ]; then
        case "$(cat "$ERR_FILE")" in
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
