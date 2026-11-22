#!/usr/bin/env bash
# nudge-on-route — wake the target session the moment a bead is routed to it.
#
# `gc sling` does not nudge warm-idle workers (issue #1129, closed by
# design: cities that reuse warm workers were told to "introduce orders
# that trigger on new beads being created and manually nudge the workers
# in the warm set"). Without that nudge, a bead whose metadata.gc.routed_to
# is newly set or changed sits unclaimed against any worker that is not
# currently in an active turn cycle.
#
# This order is exactly that workaround, shipped. It subscribes to
# bead.updated events; whenever a bead carries a gc.routed_to target it
# runs `gc session nudge <target> "<message>"`. Idempotent: a given
# (bead, routed_to) pair is nudged at most once. Dedup state lives in
# $GC_PACK_STATE_DIR/nudge-on-route-state.json, so it is both city- and
# pack-scoped — multi-city installs never cross-pollinate.
#
# Two event sources, two windows. Routing is often stamped in the CREATE
# payload (`gc order run`, formula pours, poured step beads), so the only
# lifecycle event such a bead ever emits before it is claimed is
# bead.created (#4382); bead.updated alone misses every one of them. And
# the controller fires this order on its own dispatch cadence — under
# max_dispatches_per_tick that can be many minutes apart — while a short
# wall-clock lookback only sees the last couple of minutes. Each run
# therefore processes every bead.created/bead.updated event with seq > the
# high-water seq recorded by the previous run: the fast window is read
# first, the catch-up window bounds the first run and any read the fast
# window cannot be trusted to cover, and the new high-water mark is
# recorded.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "nudge-on-route"

# jq is a hard dependency: without it the event stream cannot be decoded
# and every nudge would be silently skipped. Fail loud at startup.
if ! command -v jq >/dev/null 2>&1; then
    echo "nudge-on-route: jq is required but not found in PATH" >&2
    exit 1
fi

CITY="${GC_CITY:-.}"
# Fast window, read on every run that has a persisted mark. In steady state
# the previous run's high-water seq sits just below the log head, so this
# short read is enough and the run costs one gc events call.
LOOKBACK="${GC_NUDGE_ON_ROUTE_WINDOW:-${GC_NUDGE_ON_ROUTE_LOOKBACK:-2m}}"
# Catch-up window: the lookback on the very first run (no mark yet), and
# the re-read whenever the fast window cannot be trusted to reach back to
# the mark. Bounds how far back any run reads; the real cut is always the
# persisted high-water seq.
MAX_LOOKBACK="${GC_NUDGE_ON_ROUTE_MAX_LOOKBACK:-1h}"
# Dedup entries older than this are pruned so the state file stays bounded.
# Must comfortably exceed MAX_LOOKBACK so an active routing — which keeps
# re-emitting bead.updated as the reconciler refreshes it — is never
# pruned and re-nudged, even by a catch-up read. Accepts a simple
# Ns / Nm / Nh duration.
RETENTION="${GC_NUDGE_ON_ROUTE_RETENTION:-2h}"
NUDGE_MESSAGE="${GC_NUDGE_ON_ROUTE_MESSAGE:-check for assigned work}"

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
STATE_FILE="$PACK_STATE_DIR/nudge-on-route-state.json"
# Highest event seq processed by the previous run (a bare integer).
SEQ_FILE="$PACK_STATE_DIR/nudge-on-route-seq"
mkdir -p "$PACK_STATE_DIR"

# Convert a simple Go-style duration (Ns/Nm/Nh) to whole seconds.
duration_to_seconds() {
    case "$1" in
        *h) echo $(( ${1%h} * 3600 )) ;;
        *m) echo $(( ${1%m} * 60 )) ;;
        *s) echo "${1%s}" ;;
        *)  echo "$1" ;;
    esac
}

# A retention shorter than the catch-up window lets a catch-up read re-nudge
# a pair it still sees. Warn but keep going: the combination costs at most a
# duplicate nudge, not a missed routing.
RETENTION_S="$(duration_to_seconds "$RETENTION")"
MAX_LOOKBACK_S="$(duration_to_seconds "$MAX_LOOKBACK")"
if [ "$RETENTION_S" -lt "$MAX_LOOKBACK_S" ] 2>/dev/null; then
    echo "nudge-on-route: GC_NUDGE_ON_ROUTE_RETENTION ($RETENTION) is shorter than GC_NUDGE_ON_ROUTE_MAX_LOOKBACK ($MAX_LOOKBACK); a catch-up read can re-nudge pruned pairs" >&2
fi

# Nudge the session(s) named by a routed_to target. A multi-session pool
# routes to the pool BASE (NormalizePoolRouteTarget collapses slot -> base),
# which is the members' `template`, not a session name — `gc session nudge`
# resolves a single session and cannot target a pool base. So enumerate the
# pool's active members by template and nudge each; a target with no members
# (a single-session agent, or an explicit slot name) is nudged directly.
# Returns 0 if at least one nudge succeeded, non-zero otherwise.
nudge_routed_target() {
    _target="$1"
    _members="$(gc session list --json --state active --template "$_target" 2>/dev/null \
        | jq -r '(.sessions // [])[] | .name // .id' 2>/dev/null)" || _members=""
    if [ -n "$_members" ]; then
        _any=1
        while IFS= read -r _m; do
            [ -n "$_m" ] || continue
            if gc session nudge "$_m" "$NUDGE_MESSAGE" >/dev/null 2>&1; then
                _any=0
            fi
        done <<MEMBERS
$_members
MEMBERS
        return "$_any"
    fi
    gc session nudge "$_target" "$NUDGE_MESSAGE" >/dev/null 2>&1
}

# Pull recent events. Best-effort: a read failure (API down) must not crash
# the controller's order loop. One unfiltered read per window: `gc events
# --type` takes a single exact type and both bead.created and bead.updated
# are needed. City seqs are assigned latest+1 under the recorder's mutex, so
# they are contiguous across types: a read whose lowest seq sits at or below
# MARK+1 reached back to the previous run. The fast window is used only when
# that holds and its head covers the mark; anything else — no usable mark,
# an empty or unparseable fast read, a gap older than the fast window, or a
# log head below the mark (the log was reset or rebuilt) — re-reads the
# catch-up window and uses only that read.
LAST_SEQ="$(cat "$SEQ_FILE" 2>/dev/null || true)"
case "$LAST_SEQ" in ''|*[!0-9]*) LAST_SEQ=0 ;; esac

read_events() {
    EVENTS="$(gc events --since "$1" 2>/dev/null)" || exit 0
}

# Lowest (LOW_SEQ) and newest (HEAD_SEQ) event seq in $EVENTS; 0 when none
# parse. HEAD_SEQ is the next high-water mark. Parsed line by line so one
# malformed line (a torn events.jsonl write) never drops the rest.
seq_bounds() {
    _b="$(printf '%s\n' "$EVENTS" \
        | jq -R -r 'fromjson? | try (.seq // empty) catch empty' 2>/dev/null \
        | sort -n)" || _b=""
    LOW_SEQ="$(printf '%s\n' "$_b" | sed -n '1p')"
    HEAD_SEQ="$(printf '%s\n' "$_b" | sed -n '$p')"
    case "$LOW_SEQ" in ''|*[!0-9]*) LOW_SEQ=0 ;; esac
    case "$HEAD_SEQ" in ''|*[!0-9]*) HEAD_SEQ=0 ;; esac
}

# Widen to the catch-up window. Nothing usable even there: leave the mark
# untouched and end the run (today's best-effort contract).
catch_up() {
    read_events "$MAX_LOOKBACK"
    seq_bounds
    [ "$HEAD_SEQ" -gt 0 ] || exit 0
    if [ "$LAST_SEQ" -gt 0 ] && [ "$HEAD_SEQ" -lt "$LAST_SEQ" ]; then
        echo "nudge-on-route: event seq went backwards ($HEAD_SEQ < $LAST_SEQ); resetting mark" >&2
        LAST_SEQ=0
    fi
    # The catch-up read has no wider fallback. When even it starts above the
    # mark — the gap was longer than MAX_LOOKBACK, or the events walk ran
    # out of its budget and returned only the newest pages — name the
    # skipped span and evaluate what was read: holding the mark instead
    # would livelock every later run on the same gap.
    if [ "$LAST_SEQ" -gt 0 ] && [ "$LOW_SEQ" -gt $((LAST_SEQ + 1)) ]; then
        echo "nudge-on-route: catch-up read starts at seq $LOW_SEQ above mark $LAST_SEQ; $((LOW_SEQ - LAST_SEQ - 1)) older event(s) not evaluated" >&2
    fi
}

if [ "$LAST_SEQ" -gt 0 ]; then
    read_events "$LOOKBACK"
    seq_bounds
    if [ "$HEAD_SEQ" -gt 0 ] && [ "$LOW_SEQ" -le $((LAST_SEQ + 1)) ] && [ "$HEAD_SEQ" -ge "$LAST_SEQ" ]; then
        :
    else
        catch_up
    fi
else
    catch_up
fi

# Reduce to unique "<bead_id>\t<routed_to>" pairs from events newer than the
# previous run's high-water seq. Only events that actually carry a non-empty
# gc.routed_to target are considered. The bead payload is read as
# (.payload.bead // .payload): the API envelope wraps the bead under
# .payload.bead while the `gc events` local fallback emits the bead fields
# directly under .payload (#5968) — same normalization as the sibling
# notify-on-human-gate-creation.sh. Parsed line by line so one malformed
# line never drops the rest of the batch.
PAIRS="$(printf '%s\n' "$EVENTS" \
    | jq -R -r --argjson last "$LAST_SEQ" '
        fromjson? | try (
            select((.seq // 0) > $last
                   and (.type == "bead.created" or .type == "bead.updated"))
            | (.payload.bead // .payload) as $b
            | select($b.metadata."gc.routed_to" != null
                     and $b.metadata."gc.routed_to" != "")
            | [$b.id, $b.metadata."gc.routed_to"] | @tsv
        ) catch empty' 2>/dev/null \
    | sort -u)" || PAIRS=""

# Advance the high-water mark to the newest event seen, regardless of type,
# so the next run starts exactly where this one stopped. Unconditional: a
# nudge that fails is usually a pool with no live member yet (its spawn
# primes it), so holding the mark back for it would stall every later
# routing behind it; a routing that still matters re-emits bead.updated as
# the reconciler refreshes it and is picked up then.
advance_seq() {
    [ "$HEAD_SEQ" -gt "$LAST_SEQ" ] || return 0
    _tmp="$(mktemp "$PACK_STATE_DIR/.nudge-on-route-seq.XXXXXX")"
    printf '%s\n' "$HEAD_SEQ" > "$_tmp"
    mv -f "$_tmp" "$SEQ_FILE"
}
if [ -z "$PAIRS" ]; then
    advance_seq
    exit 0
fi

# Load dedup state (object mapping "<bead>|<routed_to>" -> ISO timestamp).
# A missing or corrupt file resets to an empty object rather than failing.
STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
echo "$STATE" | jq -e 'type == "object"' >/dev/null 2>&1 || STATE='{}'

NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
NUDGED=0
while IFS="$(printf '\t')" read -r bead_id routed_to; do
    if [ -z "$bead_id" ] || [ -z "$routed_to" ]; then continue; fi
    key="$bead_id|$routed_to"
    if echo "$STATE" | jq -e --arg k "$key" 'has($k)' >/dev/null 2>&1; then
        # Already nudged. Refresh last-seen so this still-active routing is
        # not pruned and re-nudged while it keeps re-emitting.
        STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
        continue
    fi
    if nudge_routed_target "$routed_to"; then
        STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
        NUDGED=$((NUDGED + 1))
    fi
done <<EOF
$PAIRS
EOF

# Prune entries older than RETENTION so the state file stays bounded.
STATE="$(echo "$STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select((now - (.value | fromdateiso8601)) <= $keep))')" || true

# Atomic write: temp file in the same dir, then rename. The dedup state is
# written before the seq mark on purpose: a crash between the two
# re-evaluates the batch, and dedup absorbs the repeats, while the reverse
# order would skip events whose pairs were never recorded.
TMP="$(mktemp "$PACK_STATE_DIR/.nudge-on-route-state.XXXXXX")"
printf '%s\n' "$STATE" > "$TMP"
mv -f "$TMP" "$STATE_FILE"
advance_seq

if [ "$NUDGED" -gt 0 ]; then
    echo "nudge-on-route: nudged $NUDGED newly-routed bead(s)"
fi
