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
# A bead in a rig with federation = "rig" (a bead store other cities also
# attach) is nudged only when `gc agent is-foreign --rig-namespace <rig>`
# vouches for its route. This is defence in depth: the guarded rig store is
# meant to drop other cities' events before they get here. In a city gc
# reports as having no guarded rig (GC_GUARDED_RIGS set and empty, see
# rig_namespace_city_may_guard) there is nothing to check and the rig list is
# never read. Otherwise the rig list that says which rigs are guarded is read
# once per run, and only when a pair not nudged yet remains. A run that cannot
# read it nudges no new pair (fail closed) and fails, so the controller records
# order.failed with the reason; the pairs are not recorded, so a later run in
# the lookback tries them again.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "nudge-on-route"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/rig_namespace_gate.sh"

# jq is a hard dependency: without it the event stream cannot be decoded
# and every nudge would be silently skipped. Fail loud at startup.
if ! command -v jq >/dev/null 2>&1; then
    echo "nudge-on-route: jq is required but not found in PATH" >&2
    exit 1
fi

CITY="${GC_CITY:-.}"
# Event lookback window. Must exceed the controller's event-trigger eval
# cadence so no routing event is missed between runs.
LOOKBACK="${GC_NUDGE_ON_ROUTE_LOOKBACK:-2m}"
# Dedup entries older than this are pruned so the state file stays bounded.
# Must comfortably exceed LOOKBACK so an active routing — which keeps
# re-emitting bead.updated as the reconciler refreshes it — is never
# pruned and re-nudged. Accepts a simple Ns / Nm / Nh duration.
RETENTION="${GC_NUDGE_ON_ROUTE_RETENTION:-1h}"
NUDGE_MESSAGE="${GC_NUDGE_ON_ROUTE_MESSAGE:-check for assigned work}"

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
STATE_FILE="$PACK_STATE_DIR/nudge-on-route-state.json"
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

# Pull recent bead.updated events. Best-effort: a read failure (API down)
# must not crash the controller's order loop.
EVENTS="$(gc events --type bead.updated --since "$LOOKBACK" 2>/dev/null)" || exit 0
[ -n "$EVENTS" ] || exit 0

# Reduce to unique "<bead_id>\t<routed_to>" pairs. Only events that actually
# carry a non-empty gc.routed_to target are considered.
#
# The bead.updated payload is flat — .payload.id and .payload.metadata — not
# the nested .payload.bead.{id,metadata} an earlier schema exposed. The nested
# paths match nothing against the current event shape, so every routed bead was
# silently dropped and no nudge was ever delivered.
PAIRS="$(printf '%s\n' "$EVENTS" \
    | jq -r 'select(.payload.metadata."gc.routed_to" != null
                    and .payload.metadata."gc.routed_to" != "")
             | [.payload.id, .payload.metadata."gc.routed_to"] | @tsv' 2>/dev/null \
    | sort -u)" || PAIRS=""
[ -n "$PAIRS" ] || exit 0

# Load dedup state (object mapping "<bead>|<routed_to>" -> ISO timestamp).
# A missing or corrupt file resets to an empty object rather than failing.
STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
echo "$STATE" | jq -e 'type == "object"' >/dev/null 2>&1 || STATE='{}'

NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
NUDGED=0
SKIPPED_FOREIGN=0
# Pairs not nudged because this run could not read the rig list.
SKIPPED_NO_RIG_LIST=0
RIG_LIST_READ=""
RIGS_JSON=""
GUARDED_RIGS=""
CITY_MAY_GUARD=""
if rig_namespace_city_may_guard; then
    CITY_MAY_GUARD=1
fi

# read_rig_list reads `gc rig list --json` into RIGS_JSON and GUARDED_RIGS, at
# most once per run. RIGS_JSON stays empty when the list cannot be read
# (rig_namespace_rig_list_readable). stdin is closed so gc cannot swallow the
# caller's next loop line.
read_rig_list() {
    [ -z "$RIG_LIST_READ" ] || return 0
    RIG_LIST_READ=1
    RIGS_JSON="$(gc rig list --json </dev/null 2>/dev/null)" || RIGS_JSON=""
    if rig_namespace_rig_list_readable "$RIGS_JSON"; then
        GUARDED_RIGS="$(rig_namespace_guarded_rigs "$RIGS_JSON")"
    else
        RIGS_JSON=""
    fi
}

while IFS="$(printf '\t')" read -r bead_id routed_to; do
    if [ -z "$bead_id" ] || [ -z "$routed_to" ]; then continue; fi
    key="$bead_id|$routed_to"
    if echo "$STATE" | jq -e --arg k "$key" 'has($k)' >/dev/null 2>&1; then
        # Already nudged. Refresh last-seen so this still-active routing is
        # not pruned and re-nudged while it keeps re-emitting.
        STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
        continue
    fi
    if [ -n "$CITY_MAY_GUARD" ]; then
        read_rig_list
        if [ -z "$RIGS_JSON" ]; then
            SKIPPED_NO_RIG_LIST=$((SKIPPED_NO_RIG_LIST + 1))
            continue
        fi
        # A bead.updated event names the bead, not its store, so the bead's
        # prefix is the only way to find its rig here.
        if [ -n "$GUARDED_RIGS" ] && ! rig_namespace_admits_row "$RIGS_JSON" "" "$bead_id" "$routed_to"; then
            SKIPPED_FOREIGN=$((SKIPPED_FOREIGN + 1))
            continue
        fi
    fi
    if nudge_routed_target "$routed_to"; then
        STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
        NUDGED=$((NUDGED + 1))
    fi
done <<EOF
$PAIRS
EOF

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
if [ "$SKIPPED_FOREIGN" -gt 0 ]; then
    echo "nudge-on-route: skipped $SKIPPED_FOREIGN routed bead(s) in guarded rigs whose route this city cannot vouch for"
fi
# Fail closed, loudly: the state is written, and a non-zero exit makes the
# controller record order.failed with this line, which the dashboard's
# attention list and gc doctor report.
if [ "$SKIPPED_NO_RIG_LIST" -gt 0 ]; then
    echo "nudge-on-route: gc rig list failed in a city that may have a guarded rig (federation = \"rig\"); $SKIPPED_NO_RIG_LIST routed bead(s) not nudged this run" >&2
    exit 1
fi
