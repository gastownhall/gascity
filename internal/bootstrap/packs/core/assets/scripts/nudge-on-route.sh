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
# This order is exactly that workaround, shipped. It fires on bead.updated
# and reads both bead.created and bead.updated events; whenever a bead
# carries a gc.routed_to target it runs
# `gc session nudge <target> "<message>"`. Routing is often stamped in the
# CREATE payload (`gc order run`, formula pours, poured step beads), so the
# only lifecycle event such a bead emits before it is claimed is
# bead.created (#4382); it is picked up on the next fire, which on a live
# city is seconds to a couple of minutes away.
#
# Each event is evaluated once, and each pair nudged at most once. Two state
# files live in $GC_PACK_STATE_DIR, so they are city- and pack-scoped and
# multi-city installs never cross-pollinate:
#   - nudge-on-route-seq: the highest event seq the previous run evaluated.
#     Each run selects only events above it and advances it after every
#     successful read, nudge failures included.
#   - nudge-on-route-state.json: the (bead, routed_to) pairs already nudged.
# New pairs are grouped by target, so each target is nudged once per run.
#
# Two windows. The controller fires this order on its own cadence, with no
# promise about the gap between fires. Each run reads the fast window
# (GC_NUDGE_ON_ROUTE_LOOKBACK, 2m); when its seqs do not reach back to the
# mark, the run re-reads the catch-up window (GC_NUDGE_ON_ROUTE_MAX_LOOKBACK,
# 1h), which is also the first run's read. A span that even the catch-up
# read misses is named on stderr rather than dropped silently.
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
# Fast window, read on every run that has a usable seq mark. It only needs to
# cover the usual gap between two fires: when its seqs do not reach back to
# the mark, the gap is detected and caught up from MAX_LOOKBACK.
LOOKBACK="${GC_NUDGE_ON_ROUTE_LOOKBACK:-2m}"
# Catch-up window: the read used when there is no usable seq mark (first
# run, garbage, or a mark above the log head) or the fast read missed
# events. The real cut is the persisted high-water seq; this only bounds the
# read. GC_NUDGE_ON_ROUTE_WINDOW is the earlier name of this setting and is
# still honored.
MAX_LOOKBACK="${GC_NUDGE_ON_ROUTE_MAX_LOOKBACK:-${GC_NUDGE_ON_ROUTE_WINDOW:-1h}}"
# Dedup entries older than this are pruned so the state file stays bounded.
# Must be at least MAX_LOOKBACK, so a catch-up read never re-nudges a pair
# pruned since it was nudged, and an active routing — which keeps
# re-emitting bead.updated as the reconciler refreshes it — is never
# pruned and re-nudged. Accepts a simple Ns / Nm / Nh duration.
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

RETENTION_S="$(duration_to_seconds "$RETENTION")"
MAX_LOOKBACK_S="$(duration_to_seconds "$MAX_LOOKBACK")"
# A short retention costs at most a duplicate nudge, so warn and run.
if [ "$RETENTION_S" -lt "$MAX_LOOKBACK_S" ]; then
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

# read_events reads `gc events --since <window>` into EVENTS and sets
# LOW_SEQ/HEAD_SEQ to its lowest and highest parseable seq (empty when it
# holds none). Best-effort: a failed read (API down) exits 0 with the mark
# untouched, so it never crashes the controller's order loop. The read is
# unfiltered: `gc events --type` takes a single exact type and both
# bead.created and bead.updated are needed. Each line is parsed on its own
# (`fromjson?`), so one malformed line — the local fallback reads
# events.jsonl, which can hold a torn write — never drops the rest.
read_events() {
    EVENTS="$(gc events --since "$1" 2>/dev/null)" || exit 0
    _seqs="$(printf '%s\n' "$EVENTS" | jq -R -r 'fromjson? | .seq? | numbers | floor' 2>/dev/null | sort -n)" || _seqs=""
    LOW_SEQ="$(printf '%s\n' "$_seqs" | head -1)"
    HEAD_SEQ="$(printf '%s\n' "$_seqs" | tail -1)"
}

# Highest seq the previous run evaluated; anything but digits is no mark.
LAST_SEQ="$(cat "$SEQ_FILE" 2>/dev/null || true)"
case "$LAST_SEQ" in ''|*[!0-9]*) LAST_SEQ=0 ;; *) LAST_SEQ=$((10#$LAST_SEQ)) ;; esac

# City event seqs are contiguous across all types, so the fast read covers
# everything since the mark exactly when its lowest seq is at most mark+1 and
# its head has not gone below the mark. A seq the reader omitted can only
# force an extra catch-up read, never false coverage.
FAST_OK=0
if [ "$LAST_SEQ" -gt 0 ]; then
    read_events "$LOOKBACK"
    if [ -n "$HEAD_SEQ" ] && [ "$LOW_SEQ" -le $((LAST_SEQ + 1)) ] && [ "$HEAD_SEQ" -ge "$LAST_SEQ" ]; then
        FAST_OK=1
    fi
fi
if [ "$FAST_OK" -eq 0 ]; then
    read_events "$MAX_LOOKBACK"
    [ -n "$HEAD_SEQ" ] || exit 0
    # Event log reset/rebuilt below our mark: start over rather than wedge.
    if [ "$HEAD_SEQ" -lt "$LAST_SEQ" ]; then
        echo "nudge-on-route: event seq went backwards ($HEAD_SEQ < $LAST_SEQ); resetting mark" >&2
        LAST_SEQ=0
    fi
    # The catch-up read has no wider fallback: when it starts above the mark
    # (a gap longer than MAX_LOOKBACK, or an events walk cut short by its
    # time budget) the span below it is lost. Say so, then evaluate what was
    # read and advance as usual; holding the mark would livelock under
    # sustained load, since every run would hit the same limit.
    if [ "$LAST_SEQ" -gt 0 ] && [ "$LOW_SEQ" -gt $((LAST_SEQ + 1)) ]; then
        echo "nudge-on-route: catch-up read starts at seq $LOW_SEQ above mark $LAST_SEQ; $((LOW_SEQ - LAST_SEQ - 1)) older event(s) not evaluated" >&2
    fi
fi

# Reduce to unique "<bead_id>\t<routed_to>" pairs from events newer than the
# previous run's high-water seq. Only events that actually carry a non-empty
# gc.routed_to target are considered. The bead payload is read as
# (.payload.bead // .payload): the API envelope wraps the bead under
# .payload.bead while the `gc events` local fallback emits the bead fields
# directly under .payload (#5968) — same normalization as the sibling
# notify-on-human-gate-creation.sh.
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
TAB="$(printf '\t')"
# Pairs not yet in the dedup state, one "<routed_to>\t<bead_id>" per line.
NEW_PAIRS=""
while IFS="$TAB" read -r bead_id routed_to; do
    if [ -z "$bead_id" ] || [ -z "$routed_to" ]; then continue; fi
    key="$bead_id|$routed_to"
    if echo "$STATE" | jq -e --arg k "$key" 'has($k)' >/dev/null 2>&1; then
        # Already nudged. Refresh last-seen so this still-active routing is
        # not pruned and re-nudged while it keeps re-emitting.
        STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
        continue
    fi
    NEW_PAIRS="$NEW_PAIRS$routed_to$TAB$bead_id
"
done <<EOF
$PAIRS
EOF

# One nudge per target per run. A pour that stamps routing on N steps yields
# N new pairs for one target in one batch, and each `gc session nudge`
# interrupts the session to tell it the same thing: check for assigned work.
# On success every new pair for the target is recorded; on failure none is,
# so the next event that re-emits one of them retries it.
TARGETS="$(printf '%s' "$NEW_PAIRS" | cut -f1 | sort -u)"
while IFS= read -r target; do
    [ -n "$target" ] || continue
    nudge_routed_target "$target" </dev/null || continue
    while IFS="$TAB" read -r routed_to bead_id; do
        [ "$routed_to" = "$target" ] || continue
        STATE="$(echo "$STATE" | jq --arg k "$bead_id|$routed_to" --arg now "$NOW" '.[$k] = $now')"
        NUDGED=$((NUDGED + 1))
    done <<NEW
$NEW_PAIRS
NEW
done <<TARGETS
$TARGETS
TARGETS

# Prune entries older than RETENTION so the state file stays bounded.
STATE="$(echo "$STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select((now - (.value | fromdateiso8601)) <= $keep))')" || true

# Atomic write: temp file in the same dir, then rename.
TMP="$(mktemp "$PACK_STATE_DIR/.nudge-on-route-state.XXXXXX")"
printf '%s\n' "$STATE" > "$TMP"
mv -f "$TMP" "$STATE_FILE"
# The mark is written after the dedup state, so a crash between the two
# re-evaluates the batch and the dedup state absorbs the repeats.
advance_seq

if [ "$NUDGED" -gt 0 ]; then
    echo "nudge-on-route: nudged $NUDGED newly-routed bead(s)"
fi
