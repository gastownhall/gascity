#!/usr/bin/env bash
# cascade-nudge-on-blocker-close — notify dependents when a blocker closes.
#
# When a blocker bead closes (linked via `gc bd dep <dependent> --blocks
# <blocker>`), the owner of each dependent has no event-driven signal that
# work can resume: they poll, get nudged by hand, or miss the unblock.
#
# This order subscribes to bead.closed events. For each closed bead it
# resolves the dependents linked by a `blocks` dependency and nudges the
# assignee of every open or deferred dependent. Idempotent: a given
# (blocker, dependent) pair is nudged at most once. Dedup state lives in
# $GC_PACK_STATE_DIR/cascade-nudge-on-blocker-close-state.json, so it is
# both city- and pack-scoped — multi-city installs never cross-pollinate.
#
# Event contract note: the close transition emits `bead.closed`, not
# `bead.updated` (a closed bead only emits bead.updated on a later
# metadata edit). Subscribing to bead.closed fires once, exactly on the
# transition this order cares about.
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
    DEPS="$(gc bd dep list "$blocker" ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} \
            --direction=up --type=blocks --json 2>/dev/null)" || continue
    if [ -z "$DEPS" ] || [ "$DEPS" = "[]" ]; then continue; fi

    # Dependents that are still open/deferred and have an assignee to nudge.
    ROWS="$(printf '%s' "$DEPS" \
        | jq -r '.[]
                 | select((.status == "open" or .status == "deferred")
                          and (.assignee != null and .assignee != ""))
                 | [.id, .assignee] | @tsv' 2>/dev/null)" || ROWS=""
    [ -n "$ROWS" ] || continue

    while IFS="$(printf '\t')" read -r dep_id assignee; do
        if [ -z "$dep_id" ] || [ -z "$assignee" ]; then continue; fi
        key="$blocker|$dep_id"
        if echo "$STATE" | jq -e --arg k "$key" 'has($k)' >/dev/null 2>&1; then
            STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
            continue
        fi
        set_rig_args "$dep_id"
        msg="blocker $blocker closed — your dependent $dep_id may be unblocked"
        if gc session nudge ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} "$assignee" "$msg" >/dev/null 2>&1; then
            STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
            NUDGED=$((NUDGED + 1))
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
