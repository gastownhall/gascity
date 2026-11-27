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
# Activity gate. A run sweeps only the stores that may hold an open timer or gh
# gate: a store whose last sweep found one, or one whose bead id prefix a gate
# bead.created/bead.updated event named since the last run (read from
# `gc events`, the controller's API, which opens no store). A store whose timer
# and gh checks both report no open gate leaves the watch set. A run with
# nothing watched and no gate event costs three `gc events` reads and touches
# no store at all. Every store is swept (the full sweep below) on the first run,
# whenever the event log cannot be read, when the controller restarted inside
# the lookback, when a gate event names a prefix no rig owns, and at least every
# GC_GATE_SWEEP_FULL_INTERVAL (default 1h) as a backstop for a gate the event
# log never saw. The watch set lives in $GC_PACK_STATE_DIR/gate-sweep-state.
#
# Cost of a full sweep: one `gc rig list --json` (parsed with jq), plus three
# `gc bd` calls per swept store (the database probe, then the timer and the gh
# check). On the local fallback route the listing probes each rig's running
# status, which this script does not use. Before the activity gate the full
# sweep ran on every 30s cooldown and touched every non-suspended store more
# often than a proxied scope's idle timeout, against the backstop invariant in
# engdocs/design/beads-proxied-local-default.md; the gate is the activity-gated
# backstop of Pillar 3 of engdocs/design/idle-controller-call-rate.md.
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
    local timer_out gh_out
    SCOPE_EMPTY=0
    if scope_resolve_db; then
        case "$SWEPT_DBS" in
            *" $SCOPE_DB "*)
                SCOPE_EMPTY=1
                return 0
                ;;
        esac
        SWEPT_DBS="$SWEPT_DBS$SCOPE_DB "
    elif [ "$SCOPE_NOT_BD" -eq 1 ]; then
        echo "gate-sweep: $SCOPE_LABEL is not a bd bead store; no gates to check there"
        SCOPE_EMPTY=1
        return 0
    else
        case "$SCOPE_LAST_ERROR" in
            *"is declared but has no path binding"*)
                echo "gate-sweep: $SCOPE_LABEL has no path binding on this site; no gates to check there"
                SCOPE_EMPTY=1
                return 0
                ;;
        esac
    fi
    timer_out=$(scope_bd gate check --type=timer --escalate) || rc=$?
    [ -z "$timer_out" ] || printf '%s\n' "$timer_out"
    if [ "$rc" -ne 0 ]; then
        echo "gate-sweep: timer gate check failed in $SCOPE_LABEL (exit $rc)" >&2
        FAILED_SCOPES="$FAILED_SCOPES $SCOPE_LABEL"
    fi
    gh_out=$(scope_bd gate check --type=gh --escalate) || true
    [ -z "$gh_out" ] || printf '%s\n' "$gh_out"
    # bd prints "No open gates of type '<type>' found." when it has nothing to
    # check. Only that answer from both checks, from a timer check that
    # succeeded, takes the store off the watch set; any other output keeps it.
    if [ "$rc" -eq 0 ]; then
        case "$timer_out" in
            *"No open gates"*)
                case "$gh_out" in
                    *"No open gates"*) SCOPE_EMPTY=1 ;;
                esac
                ;;
        esac
    fi
}

# finish ends the run with status $1, or 1 when a timer check failed. The
# controller keeps only the tail of a failing run's output, so the failed
# stores are named again on the last stderr line. The watch set is saved first,
# whatever the status.
finish() {
    write_state
    if [ -n "$FAILED_SCOPES" ]; then
        echo "gate-sweep: timer gate check failed in:$FAILED_SCOPES" >&2
        exit 1
    fi
    exit "$1"
}

# --- activity gate state -----------------------------------------------------

STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY_ABS/.gc/runtime}/packs/core}"
STATE_FILE="$STATE_DIR/gate-sweep-state"
NOW=$(date +%s)
LAST_RUN=""
LAST_FULL=""
# WATCH holds store labels ("city", "rig:<name>"), space-separated with a
# leading and trailing space so a label is matched as *" $label "*.
WATCH=" "
FULL=1
FULL_COMPLETE=0
FULL_REASON="no sweep state yet"

duration_to_seconds() {
    case "$1" in
        *h) echo $(( ${1%h} * 3600 )) ;;
        *m) echo $(( ${1%m} * 60 )) ;;
        *s) echo "${1%s}" ;;
        *)  echo "$1" ;;
    esac
}
FULL_INTERVAL_S=$(duration_to_seconds "${GC_GATE_SWEEP_FULL_INTERVAL:-1h}")
case "$FULL_INTERVAL_S" in '' | *[!0-9]*) FULL_INTERVAL_S=3600 ;; esac

watch_add() {
    case "$WATCH" in *" $1 "*) ;; *) WATCH="$WATCH$1 " ;; esac
}
watch_del() {
    WATCH="${WATCH/ $1 / }"
}

read_state() {
    local key val
    [ -f "$STATE_FILE" ] || return 0
    while read -r key val; do
        case "$key" in
            last_run) LAST_RUN="$val" ;;
            last_full) LAST_FULL="$val" ;;
            watch) [ -z "$val" ] || watch_add "$val" ;;
        esac
    done <"$STATE_FILE"
    case "$LAST_RUN" in *[!0-9]*) LAST_RUN="" ;; esac
    case "$LAST_FULL" in *[!0-9]*) LAST_FULL="" ;; esac
}

write_state() {
    local tmp label
    mkdir -p "$STATE_DIR" 2>/dev/null || return 0
    tmp=$(mktemp "$STATE_DIR/.gate-sweep-state.XXXXXX" 2>/dev/null) || return 0
    {
        echo "last_run $NOW"
        # Only a full sweep that reached every store resets the backstop.
        if [ "$FULL_COMPLETE" -eq 1 ]; then echo "last_full $NOW"; else echo "last_full $LAST_FULL"; fi
        for label in $WATCH; do echo "watch $label"; done
    } >"$tmp"
    mv -f "$tmp" "$STATE_FILE"
}

# store_label_for_prefix prints the store label owning bead id prefix $1 from
# $RIGS_JSON: "city" for the hq entry, "rig:<name>" for a rig, nothing when no
# entry has that prefix.
store_label_for_prefix() {
    printf '%s' "$RIGS_JSON" | jq -r --arg p "$1" '
        [(.rigs // [])[] | select(.prefix == $p)][0] // empty
        | if (.hq // false) then "city" else "rig:" + .name end' 2>/dev/null
}

# gate_activity reads the events since the last run, with a minute of overlap,
# and adds the store of every gate created or updated in them to WATCH. It
# returns 1, with FULL_REASON set, when only a full sweep is safe.
gate_activity() {
    local since="$1s"
    local out ids="" prefixes prefix label
    if ! out=$(gc events --type controller.started --since "$since" 2>/dev/null); then
        FULL_REASON="the event log could not be read"
        return 1
    fi
    if [ -n "$out" ]; then
        FULL_REASON="the controller restarted since the last run"
        return 1
    fi
    for type in bead.created bead.updated; do
        if ! out=$(gc events --type "$type" --since "$since" 2>/dev/null); then
            FULL_REASON="the event log could not be read"
            return 1
        fi
        [ -n "$out" ] || continue
        # The API wraps the bead under .payload.bead; the local fallback
        # copies the bus payload, where it sits directly under .payload.
        if ! out=$(printf '%s\n' "$out" | jq -r '(.payload.bead // .payload) | select(.issue_type == "gate") | .id // empty' 2>/dev/null); then
            FULL_REASON="the $type events could not be decoded"
            return 1
        fi
        ids="$ids$out
"
    done
    prefixes=$(printf '%s' "$ids" | sed -n 's/^\([^-][^-]*\)-.*/\1/p' | sort -u)
    [ -n "$prefixes" ] || return 0
    if ! RIGS_JSON=$(cd "$CITY_ABS" && gc rig list --json 2>/dev/null); then
        FULL_REASON="gc rig list failed"
        return 1
    fi
    while IFS= read -r prefix; do
        [ -n "$prefix" ] || continue
        label=$(store_label_for_prefix "$prefix")
        if [ -z "$label" ]; then
            FULL_REASON="a gate event names prefix $prefix, which no store owns"
            return 1
        fi
        watch_add "$label"
    done <<<"$prefixes"
}

# sweep_and_mark sweeps the current scope and records whether it stays watched.
sweep_and_mark() {
    sweep_scope
    if [ "$SCOPE_EMPTY" -eq 1 ]; then
        watch_del "$SCOPE_LABEL"
    else
        watch_add "$SCOPE_LABEL"
    fi
}

RIGS_JSON=""
if command -v jq >/dev/null 2>&1; then
    read_state
    if [ -n "$LAST_RUN" ] && [ -n "$LAST_FULL" ]; then
        if [ $((NOW - LAST_FULL)) -ge "$FULL_INTERVAL_S" ] || [ "$NOW" -lt "$LAST_FULL" ] || [ "$NOW" -lt "$LAST_RUN" ]; then
            FULL_REASON="the full-sweep backstop is due"
        elif gate_activity $((NOW - LAST_RUN + 60)); then
            FULL=0
        fi
    fi
fi

if [ "$FULL" -eq 0 ]; then
    if [ "$WATCH" = " " ]; then
        # Nothing may hold a gate: no store is touched.
        finish 0
    fi
    if [ -z "$RIGS_JSON" ] && ! RIGS_JSON=$(cd "$CITY_ABS" && gc rig list --json 2>/dev/null); then
        echo "gate-sweep: gc rig list failed; watched stores were not checked this run" >&2
        finish 1
    fi
    ACTIVE_RIGS=$(printf '%s\n' "$RIGS_JSON" | jq -r '.rigs[]? | select((.hq // false) == false and (.suspended // false) == false) | .name // empty' 2>/dev/null) || ACTIVE_RIGS=""
    ALL_RIGS=$(printf '%s\n' "$RIGS_JSON" | jq -r '.rigs[]? | select((.hq // false) == false) | .name // empty' 2>/dev/null) || ALL_RIGS=""
    for label in $WATCH; do
        case "$label" in
            city)
                scope_select city
                sweep_and_mark </dev/null
                ;;
            rig:*)
                if printf '%s\n' "$ACTIVE_RIGS" | grep -qxF -- "${label#rig:}"; then
                    scope_select rig "${label#rig:}"
                    sweep_and_mark </dev/null
                elif ! printf '%s\n' "$ALL_RIGS" | grep -qxF -- "${label#rig:}"; then
                    # The rig is gone from the config; a suspended rig stays
                    # watched until it resumes.
                    watch_del "$label"
                fi
                ;;
        esac
    done
    finish 0
fi

# Full sweep: every store.
echo "gate-sweep: sweeping every store ($FULL_REASON)"
scope_select city
sweep_and_mark

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
    sweep_and_mark </dev/null
done <<<"$RIG_NAMES"

FULL_COMPLETE=1
finish 0
