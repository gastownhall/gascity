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
# first sweep after resume. Stores served by one Dolt server database (a rig
# bound to the city store) are swept once, by the first of them; bd names no
# database for an embedded store, which is always swept. A timer check that
# fails in one store is reported on stderr naming the store; that store's gh
# gates and the other stores are still checked, and the run exits non-zero
# with a last stderr line naming every failed store. A store gc will not scope
# for bd is skipped with a notice and does not fail the run. A run that cannot
# list the rigs (gc rig list fails, or jq is missing) fails after the city
# store is swept.
#
# Cost: one `gc rig list --json` (parsed with jq) on every run, even in a city
# with no rigs, plus three `gc bd` calls per swept store (the database probe,
# then the timer and the gh check). On the local fallback route the listing
# probes each rig's running status, which this script does not use. On its 30s
# cooldown the order touches every non-suspended store more often than a
# proxied scope's idle timeout, against the backstop invariant in
# engdocs/design/beads-proxied-local-default.md; activity-gated backstops are
# Pillar 3 of engdocs/design/idle-controller-call-rate.md.
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

FAILED_SCOPES=""
SWEPT_DBS=" "

# sweep_scope checks the timer and gh gates of the scope scope_select made
# current. It first asks bd which database backs the scope, and a database
# already swept this run is not swept again: bd escalates an escalated gh gate
# on every check, so a second pass would send each escalation twice. A store
# gc will not point bd at (a non-bd beads provider, or a rig with no path
# binding on this site) holds no bd gates, so it is named as skipped rather
# than failed. Any other failed probe leaves the database unknown (bd serves
# `sql` against a Dolt server, not an embedded store, and the store may be
# unreachable), and the store is swept anyway: its gate checks say whether bd
# can reach it. bd exits non-zero when a single gate could not be checked or
# closed, after it has applied the others, so a failed timer check is recorded
# and the store's gh gates are still checked.
sweep_scope() {
    local rc=0
    if scope_resolve_db; then
        case "$SWEPT_DBS" in
            *" $SCOPE_DB "*) return 0 ;;
        esac
        SWEPT_DBS="$SWEPT_DBS$SCOPE_DB "
    elif [ "$SCOPE_NOT_BD" -eq 1 ]; then
        echo "gate-sweep: $SCOPE_LABEL is not a bd bead store; no gates to check there"
        return 0
    else
        case "$SCOPE_LAST_ERROR" in
            *"is declared but has no path binding"*)
                echo "gate-sweep: $SCOPE_LABEL has no path binding on this site; no gates to check there"
                return 0
                ;;
        esac
    fi
    scope_bd gate check --type=timer --escalate || rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "gate-sweep: timer gate check failed in $SCOPE_LABEL (exit $rc)" >&2
        FAILED_SCOPES="$FAILED_SCOPES $SCOPE_LABEL"
    fi
    scope_bd gate check --type=gh --escalate || true
}

# finish ends the run with status $1, or 1 when a timer check failed. The
# controller keeps only the tail of a failing run's output, so the failed
# stores are named again on the last stderr line.
finish() {
    if [ -n "$FAILED_SCOPES" ]; then
        echo "gate-sweep: timer gate check failed in:$FAILED_SCOPES" >&2
        exit 1
    fi
    exit "$1"
}

scope_select city
sweep_scope

# The city sweep needs no jq, so jq is checked only once it is swept. A run
# that cannot list the rigs fails rather than quietly narrowing to the city.
if ! command -v jq >/dev/null 2>&1; then
    echo "gate-sweep: jq is required to list rig stores but not found in PATH; rig stores were not checked this run" >&2
    finish 1
fi
if ! RIG_NAMES=$(core_rig_names); then
    echo "gate-sweep: gc rig list failed; rig stores were not checked this run" >&2
    finish 1
fi
while IFS= read -r rig_name; do
    [ -n "$rig_name" ] || continue
    scope_select rig "$rig_name"
    # Nothing a sweep spawns may read the remaining rig names.
    sweep_scope </dev/null
done <<<"$RIG_NAMES"

finish 0
