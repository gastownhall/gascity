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
# Two event sources, one window. Routing is often stamped in the CREATE
# payload (`gc order run`, formula pours, poured step beads), so the only
# lifecycle event such a bead ever emits before it is claimed is
# bead.created (#4382); bead.updated alone misses every one of them. And
# the controller fires this order on its own dispatch cadence — under
# max_dispatches_per_tick that can be many minutes apart — while a fixed
# wall-clock lookback only sees the last couple of minutes, so events
# between two runs were dropped. Each run therefore processes every
# bead.created/bead.updated event with seq > the high-water seq recorded by
# the previous run (bounded by WINDOW), and records the new high-water mark.
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
# Upper bound on how far back one run reads. The real cut is the persisted
# high-water seq; this only bounds the read (and is the lookback on the very
# first run, when no seq has been recorded yet).
WINDOW="${GC_NUDGE_ON_ROUTE_WINDOW:-${GC_NUDGE_ON_ROUTE_LOOKBACK:-1h}}"
# Dedup entries older than this are pruned so the state file stays bounded.
# Must comfortably exceed WINDOW so an active routing — which keeps
# re-emitting bead.updated as the reconciler refreshes it — is never
# pruned and re-nudged. Accepts a simple Ns / Nm / Nh duration.
RETENTION="${GC_NUDGE_ON_ROUTE_RETENTION:-1h}"
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
# the controller's order loop. One unfiltered read: `gc events --type` takes
# a single exact type and both bead.created and bead.updated are needed.
EVENTS="$(gc events --since "$WINDOW" 2>/dev/null)" || exit 0
[ -n "$EVENTS" ] || exit 0

LAST_SEQ="$(cat "$SEQ_FILE" 2>/dev/null || true)"
case "$LAST_SEQ" in ''|*[!0-9]*) LAST_SEQ=0 ;; esac

# Reduce to unique "<bead_id>\t<routed_to>" pairs from events newer than the
# previous run's high-water seq. Only events that actually carry a non-empty
# gc.routed_to target are considered. The bead payload is read as
# (.payload.bead // .payload): the API envelope wraps the bead under
# .payload.bead while the `gc events` local fallback emits the bead fields
# directly under .payload (#5968) — same normalization as the sibling
# notify-on-human-gate-creation.sh.
PAIRS="$(printf '%s\n' "$EVENTS" \
    | jq -r --argjson last "$LAST_SEQ" '
        select((.seq // 0) > $last
               and (.type == "bead.created" or .type == "bead.updated"))
        | (.payload.bead // .payload) as $b
        | select($b.metadata."gc.routed_to" != null
                 and $b.metadata."gc.routed_to" != "")
        | [$b.id, $b.metadata."gc.routed_to"] | @tsv' 2>/dev/null \
    | sort -u)" || PAIRS=""

# Advance the high-water mark to the newest event seen, regardless of type,
# so the next run starts exactly where this one stopped. Unconditional: a
# nudge that fails is usually a pool with no live member yet (its spawn
# primes it), so holding the mark back for it would stall every later
# routing behind it; a routing that still matters re-emits bead.updated as
# the reconciler refreshes it and is picked up then.
HEAD_SEQ="$(printf '%s\n' "$EVENTS" | jq -r '.seq // 0' 2>/dev/null | sort -n | tail -1)" || HEAD_SEQ=""
case "$HEAD_SEQ" in ''|*[!0-9]*) HEAD_SEQ=0 ;; esac
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
advance_seq

# Prune entries older than RETENTION so the state file stays bounded.
RETENTION_S="$(duration_to_seconds "$RETENTION")"
STATE="$(echo "$STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select((now - (.value | fromdateiso8601)) <= $keep))')" || true

# Atomic write: temp file in the same dir, then rename.
TMP="$(mktemp "$PACK_STATE_DIR/.nudge-on-route-state.XXXXXX")"
printf '%s\n' "$STATE" > "$TMP"
mv -f "$TMP" "$STATE_FILE"

if [ "$NUDGED" -gt 0 ]; then
    echo "nudge-on-route: nudged $NUDGED newly-routed bead(s)"
fi
