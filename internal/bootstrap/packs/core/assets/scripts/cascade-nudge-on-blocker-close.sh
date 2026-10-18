#!/usr/bin/env bash
# cascade-nudge-on-blocker-close — notify dependents when a blocker closes.
#
# When a blocker bead closes (linked via `gc bd dep <dependent> --blocks
# <blocker>`), the owner of each dependent has no event-driven signal that
# work can resume: they poll, get nudged by hand, or miss the unblock.
#
# This order subscribes to bead.closed events. For each closed bead it
# resolves the dependents linked by a `blocks` dependency and nudges every
# open or deferred dependent that no other open blocker still holds: at the
# assignee when one holds the bead, else at its gc.routed_to target (the
# graph.v2 claim shape leaves the assignee empty until a session claims the
# step). A routed target may be a pool base, which nudge_routed_target
# resolves to its active members. Idempotent: a given
# (blocker, dependent) pair is nudged at most once. Dedup state lives in
# $GC_PACK_STATE_DIR/cascade-nudge-on-blocker-close-state.json, so it is
# both city- and pack-scoped — multi-city installs never cross-pollinate.
#
# Event contract note: the close transition emits `bead.closed`, not
# `bead.updated` (a closed bead only emits bead.updated on a later
# metadata edit), so subscribing to bead.closed fires on the transition
# this order cares about. Delivery is at least once: one close can reach
# the bus twice (from the closing process and from the controller's
# cache), and the blocker list and the per-pair dedup absorb the repeat.
#
# Cross-rig blocker chains within a city are supported via a prefix->rig
# lookup so `gc bd dep list` and `gc session nudge` are scoped to the rig that
# owns each bead. The dep lookup routes through `gc bd` (not bare `bd`) so the
# wrapper runs bd in the owning rig's directory; `--rig` is a gc flag, not a
# bd flag. Cross-city cascade is out of scope.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "cascade-nudge-on-blocker-close"

# jq is a hard dependency: it decodes the event stream and the dependency
# records. Without it every cascade would be silently skipped. Fail loud.
if ! command -v jq >/dev/null 2>&1; then
    echo "cascade-nudge-on-blocker-close: jq is required but not found in PATH" >&2
    exit 1
fi

CITY="${GC_CITY:-.}"
# Event lookback window. Must exceed the controller's event-trigger eval
# cadence so no close event is missed between runs.
LOOKBACK="${GC_CASCADE_NUDGE_LOOKBACK:-5m}"
# Dedup entries older than this are pruned so the state file stays bounded.
# Must exceed LOOKBACK. Accepts a simple Ns / Nm / Nh duration.
RETENTION="${GC_CASCADE_NUDGE_RETENTION:-1h}"

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
STATE_FILE="$PACK_STATE_DIR/cascade-nudge-on-blocker-close-state.json"
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

# Build a prefix->rig lookup once. `gc rig list` is queried per bead via
# prefix_rig below (no associative arrays, for bash 3.2 portability).
# Best-effort: a single-rig city resolves nothing here and simply runs the
# bd/gc calls in their default scope.
RIGS_JSON="$(gc rig list --json 2>/dev/null || true)"

# Resolve a bead id's rig into RIG_ARG1/RIG_ARG2 ("--rig" "<name>"), or
# leave them empty when the prefix is unknown. Callers expand them with
# ${RIG_ARG1:+...} so an empty result adds no arguments under `set -u`.
set_rig_args() {
    RIG_ARG1=""
    RIG_ARG2=""
    RIG_SUSPENDED=""
    [ -n "$RIGS_JSON" ] || return 0
    _prefix="${1%%-*}"
    [ -n "$_prefix" ] && [ "$_prefix" != "$1" ] || return 0
    # Exclude the HQ entry: `gc rig list` reports the city root as a rig with
    # an hq=true flag and its own prefix, but it is not a declared rig binding,
    # so `gc --rig <cityName>` cannot resolve it. Leaving RIG_ARG empty for HQ
    # beads falls back to default scope, which is where they live.
    _rig="$(printf '%s' "$RIGS_JSON" \
        | jq -r --arg p "$_prefix" '(.rigs // [])[] | select(.prefix == $p and (.hq != true)) | .name' 2>/dev/null \
        | head -1)"
    if [ -n "$_rig" ]; then
        RIG_ARG1="--rig"
        RIG_ARG2="$_rig"
    fi
    # A suspended rig is left cold: any bd read restarts its retired proxy.
    # RIG_SUSPENDED tells the caller to leave the bead until the rig resumes.
    if [ -n "$_rig" ] && printf '%s' "$RIGS_JSON" \
        | jq -e --arg r "$_rig" '(.rigs // [])[] | select(.name == $r and .suspended == true)' >/dev/null 2>&1; then
        RIG_SUSPENDED=1
    fi
}

# Nudge the session(s) named by a routed_to target. A multi-session pool
# routes to the pool BASE (NormalizePoolRouteTarget collapses slot -> base),
# which is the members' `template`, not a session name — `gc session nudge`
# resolves a single session and cannot target a pool base. So enumerate the
# pool's active members by template and nudge each; a target with no members
# (a single-session agent, or an explicit slot name) is nudged directly.
# Returns 0 if at least one nudge succeeded, non-zero otherwise. Mirrors
# nudge_routed_target in nudge-on-route.sh with a per-call message. No --rig:
# gc.routed_to is city-qualified and may name a rig other than the dependent
# bead's, and the proven helper lists and nudges in default scope.
nudge_routed_target() {
    local target="$1"
    local message="$2"
    local members any member
    members="$(gc session list --json --state active --template "$target" 2>/dev/null \
        | jq -r '(.sessions // [])[] | .name // .id' 2>/dev/null)" || members=""
    if [ -n "$members" ]; then
        any=1
        while IFS= read -r member; do
            [ -n "$member" ] || continue
            if gc session nudge "$member" "$message" >/dev/null 2>&1; then
                any=0
            fi
        done <<MEMBERS
$members
MEMBERS
        return "$any"
    fi
    gc session nudge "$target" "$message" >/dev/null 2>&1
}

# still_blocked reports whether dependent $1 has a `blocks` blocker other
# than $2, the blocker whose close is being handled, that still blocks it (bd
# treats a closed or pinned blocker as satisfied). Only a close that may have
# unblocked the dependent is worth a nudge: while another blocker holds it,
# its assignee has nothing to do, and a nudge to an asleep seat parked on it
# would wake that seat for nothing (it wakes on its own once the dependent is
# ready). A read that fails, or that cannot be parsed, proves nothing: the
# nudge goes out, as it always has. Uses the dependent's rig args.
still_blocked() {
    _down="$(gc bd dep list "$1" ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} \
            --direction=down --type=blocks --json 2>/dev/null)" || return 1
    _open="$(printf '%s' "$_down" \
        | jq -r --arg b "$2" '[.[] | select(.id != $b and .status != "closed" and .status != "pinned")] | length' 2>/dev/null)" || return 1
    [ -n "$_open" ] && [ "$_open" -gt 0 ]
}

# Pull recent bead.closed events. Best-effort: a read failure must not
# crash the controller's order loop.
EVENTS="$(gc events --type bead.closed --since "$LOOKBACK" 2>/dev/null)" || exit 0
[ -n "$EVENTS" ] || exit 0

BLOCKERS="$(printf '%s\n' "$EVENTS" \
    | jq -r '.payload.bead.id // empty' 2>/dev/null | sort -u)" || BLOCKERS=""
[ -n "$BLOCKERS" ] || exit 0

# Load dedup state (object mapping "<blocker>|<dependent>" -> ISO timestamp).
STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
echo "$STATE" | jq -e 'type == "object"' >/dev/null 2>&1 || STATE='{}'

NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
NUDGED=0
while IFS= read -r blocker; do
    [ -n "$blocker" ] || continue

    set_rig_args "$blocker"
    [ -z "$RIG_SUSPENDED" ] || continue
    DEPS="$(gc bd dep list "$blocker" ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} \
            --direction=up --type=blocks --json 2>/dev/null)" || continue
    if [ -z "$DEPS" ] || [ "$DEPS" = "[]" ]; then continue; fi

    # Dependents that are still open/deferred and have somewhere to send the
    # wake-up: an assigned dependent is nudged at its assignee; an unassigned
    # one is nudged at its gc.routed_to target (the graph.v2 claim shape
    # keeps the assignee empty until a session claims). The kind column
    # dispatches the two deliveries and keeps every field non-empty, so the
    # tab-separated read cannot collapse a column; a malformed row (metadata
    # a string, gc.routed_to a number) is dropped alone, not the blocker's
    # whole row set.
    ROWS="$(printf '%s' "$DEPS" \
        | jq -r '.[]
                 | select(.status == "open" or .status == "deferred")
                 | ((.assignee | strings) // "") as $assignee
                 | ((.metadata | objects | .["gc.routed_to"] | strings) // "") as $route
                 | if $assignee != "" then [.id, "assignee", $assignee]
                   elif $route != "" then [.id, "route", $route]
                   else empty end
                 | @tsv' 2>/dev/null)" || ROWS=""
    [ -n "$ROWS" ] || continue

    while IFS="$(printf '\t')" read -r dep_id dep_kind target; do
        if [ -z "$dep_id" ] || [ -z "$dep_kind" ] || [ -z "$target" ]; then continue; fi
        key="$blocker|$dep_id"
        if echo "$STATE" | jq -e --arg k "$key" 'has($k)' >/dev/null 2>&1; then
            STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
            continue
        fi
        set_rig_args "$dep_id"
        [ -z "$RIG_SUSPENDED" ] || continue
        # A still-blocked dependent's pair is recorded as handled, so the
        # lookback's later runs do not read its blockers again: the close of
        # its last blocker is its own (blocker, dependent) pair, and nudges.
        if still_blocked "$dep_id" "$blocker"; then
            STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
            continue
        fi
        if [ "$dep_kind" = "assignee" ]; then
            msg="blocker $blocker closed — your dependent $dep_id may be unblocked"
            if gc session nudge ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} "$target" "$msg" >/dev/null 2>&1; then
                STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
                NUDGED=$((NUDGED + 1))
            fi
        else
            msg="blocker $blocker closed — routed dependent $dep_id may be ready to claim"
            if nudge_routed_target "$target" "$msg"; then
                STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
                NUDGED=$((NUDGED + 1))
            fi
        fi
    done <<EOF
$ROWS
EOF
done <<EOF
$BLOCKERS
EOF

# Prune entries older than RETENTION so the state file stays bounded.
RETENTION_S="$(duration_to_seconds "$RETENTION")"
STATE="$(echo "$STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select((now - (.value | fromdateiso8601)) <= $keep))')" || true

# Atomic write: temp file in the same dir, then rename.
TMP="$(mktemp "$PACK_STATE_DIR/.cascade-nudge-on-blocker-close-state.XXXXXX")"
printf '%s\n' "$STATE" > "$TMP"
mv -f "$TMP" "$STATE_FILE"

if [ "$NUDGED" -gt 0 ]; then
    echo "cascade-nudge-on-blocker-close: nudged $NUDGED dependent(s)"
fi
