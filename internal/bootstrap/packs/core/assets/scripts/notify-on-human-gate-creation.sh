#!/usr/bin/env bash
# notify-on-human-gate-creation — mail + nudge the addressee when a human
# gate bead is created.
#
# Creating a `type=human` gate produces ZERO notification: `gc bd gate create`
# builds the gate bead, adds the blocks edge, commits, prints to stdout, and
# returns. Nobody tells the human who must resolve it. The only gate watcher
# (`gc bd gate check`) skips human gates entirely (they "need manual
# resolution"). Doctrine papers over the gap by hand ("on the predecessor
# close, mail+nudge the addressee"); this order ships that reflex.
#
# It subscribes to bead.created events. For each newly-created bead whose
# issue_type is `gate` it re-fetches the bead (`gc bd show --json`) — the
# event payload does not carry the gate type — and, when the gate is an OPEN
# `human` gate, resolves the addressee and notifies them once. The type is
# bd's await_type for a gate bd created, else the gc.gate_type metadata a
# formula [steps.gate] stamps; a gate typed neither way is never mailed.
# Idempotent:
# a given gate is notified at most once. Dedup state lives in
# $GC_PACK_STATE_DIR/notify-on-human-gate-creation-state.json, so it is both
# city- and pack-scoped — multi-city installs never cross-pollinate.
#
# Addressee resolution (first non-empty wins):
#   1. the gate's assignee
#   2. gc.deferred_assignee metadata (formula/molecule gates strip the
#      assignee here at create time, molecule.go stripDeferredAssignee)
#   3. $GC_ESCALATION_RECIPIENT (default "human")
#
# Notification rides `gc mail send --notify`, which mails the addressee and
# nudges them when they are a real session — and deliberately skips the
# tmux-nudge for the "human" recipient (humans have no session to poke;
# cmd_mail.go guards `to != "human"`). That is the one wrinkle a naive
# "nudge the assignee" would trip on.
#
# Loud-fail (gastownhall/gascity#4543): an undeliverable send is NOT recorded
# as done, and the script exits NON-ZERO when any send failed. The exit code is
# load-bearing — the controller captures an exec order's combined output but
# only logs it on a non-zero exit (order_dispatch.go), so a fire-and-forget
# exit 0 would swallow the failure. It never silently evaporates. Retry: the
# controller persists the bead.created cursor before the run, so this order
# retries a failed gate only opportunistically (another bead.created within the
# lookback window re-queries the same window); the companion staleness sweep is
# the guaranteed backstop — it re-notifies any human gate still open past the
# threshold, so a persistently-undeliverable gate is not lost.
#
# Cross-rig gate beads within a city are supported via a prefix->rig lookup
# so `gc bd show` is scoped to the rig that owns each gate. The read routes
# through `gc bd` (not bare `bd`) so the wrapper runs bd in the owning rig's
# directory; `--rig` is a gc flag, not a bd flag. Mail send is city-scoped:
# recipients (mayor / human / coordinators) are city-level identities.
#
# In a rig with federation = "rig" (a bead store other cities also attach), a
# gate whose own addressee this city cannot vouch for (`gc agent is-foreign
# --rig-namespace <rig>`) is left alone, and so is a gate that names no
# addressee when its creation stamp (gc.owner_namespace) names another
# namespace, as the stale-gate re-nudge judges it. This is defence in depth:
# the guarded rig store is meant to drop other cities' events before they get
# here. In a city gc reports as having no guarded rig (GC_GUARDED_RIGS set
# and empty, see rig_namespace_city_may_guard) none of that is checked, and
# the rig list stays the best-effort scope lookup it has always been.
# Otherwise a run that cannot read `gc rig list` cannot tell a guarded rig
# apart, so it notifies no new gate (fail closed) and fails, so the controller
# records order.failed with the reason; the gates are not recorded, so a later
# run in the lookback, or the stale-gate re-nudge, notifies them.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "notify-on-human-gate-creation"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/rig_namespace_gate.sh"

# jq is a hard dependency: it decodes the event stream and the gate bead
# record. Without it every notification would be silently skipped. Fail loud.
if ! command -v jq >/dev/null 2>&1; then
    echo "notify-on-human-gate-creation: jq is required but not found in PATH" >&2
    exit 1
fi

CITY="${GC_CITY:-.}"
# Event lookback window. Must exceed the controller's event-trigger eval
# cadence so no bead.created event is missed between runs.
LOOKBACK="${GC_NOTIFY_GATE_LOOKBACK:-5m}"
# Dedup entries older than this are pruned so the state file stays bounded.
# Must exceed LOOKBACK. Accepts a simple Ns / Nm / Nh duration.
RETENTION="${GC_NOTIFY_GATE_RETENTION:-1h}"
# Human channel for gates with no resolvable assignee. escalate.sh uses the
# same default, keeping the "notify the human" address consistent.
ESCALATION_RECIPIENT="${GC_ESCALATION_RECIPIENT:-human}"

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
STATE_FILE="$PACK_STATE_DIR/notify-on-human-gate-creation-state.json"
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

# Build a prefix->rig lookup once. A single-rig city resolves nothing here and
# simply runs the bd/gc calls in their default scope. A list that cannot be
# read is left empty. In a city that may have a guarded rig the run then
# notifies no new gate (see the gate loop); in one without, the bd/gc calls run
# in their default scope.
CITY_MAY_GUARD=""
if rig_namespace_city_may_guard; then
    CITY_MAY_GUARD=1
fi
RIGS_JSON="$(gc rig list --json 2>/dev/null)" || RIGS_JSON=""
rig_namespace_rig_list_readable "$RIGS_JSON" || RIGS_JSON=""

# Resolve a bead id's rig into RIG_ARG1/RIG_ARG2 ("--rig" "<name>"), or leave
# them empty when the prefix is unknown. Callers expand them with
# ${RIG_ARG1:+...} so an empty result adds no arguments under `set -u`. The HQ
# entry is excluded: `gc rig list` reports the city root as an hq=true
# pseudo-rig that `gc --rig <cityName>` cannot resolve, so HQ beads fall back
# to default scope, which is where they live.
set_rig_args() {
    RIG_ARG1=""
    RIG_ARG2=""
    RIG_SUSPENDED=""
    [ -n "$RIGS_JSON" ] || return 0
    _prefix="${1%%-*}"
    [ -n "$_prefix" ] && [ "$_prefix" != "$1" ] || return 0
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

# Pull recent bead.created events. Best-effort: a read failure (API down)
# must not crash the controller's order loop.
EVENTS="$(gc events --type bead.created --since "$LOOKBACK" 2>/dev/null)" || exit 0
[ -n "$EVENTS" ] || exit 0

# Reduce to the unique bead ids whose issue_type is `gate`. Non-gate creations
# (the overwhelming majority) are dropped here, before any per-bead re-fetch.
# Normalize the payload shape: the API envelope wraps the bead under
# .payload.bead, but the `gc events` local fallback (used when the API is down)
# copies the raw bus payload verbatim, where the bead fields sit directly under
# .payload. `(.payload.bead // .payload)` reads both, so a gate is never missed
# in fallback mode (which is exactly when notifications matter most).
GATE_IDS="$(printf '%s\n' "$EVENTS" \
    | jq -r '(.payload.bead // .payload) as $b
             | select($b.issue_type == "gate")
             | $b.id // empty' 2>/dev/null \
    | sort -u)" || GATE_IDS=""
[ -n "$GATE_IDS" ] || exit 0

# Load dedup state (object mapping "<gate-id>" -> ISO timestamp). A missing or
# corrupt file resets to an empty object rather than failing.
STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
echo "$STATE" | jq -e 'type == "object"' >/dev/null 2>&1 || STATE='{}'

NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
NOTIFIED=0
FAILED=0
SKIPPED_FOREIGN=0
# Gates not notified because this run could not read the rig list.
SKIPPED_NO_RIG_LIST=0
while IFS= read -r gate_id; do
    [ -n "$gate_id" ] || continue

    # Already notified? Refresh last-seen so this entry is not pruned and
    # re-notified while the creation event is still inside the lookback window.
    if echo "$STATE" | jq -e --arg k "$gate_id" 'has($k)' >/dev/null 2>&1; then
        STATE="$(echo "$STATE" | jq --arg k "$gate_id" --arg now "$NOW" '.[$k] = $now')"
        continue
    fi
    if [ -n "$CITY_MAY_GUARD" ] && [ -z "$RIGS_JSON" ]; then
        SKIPPED_NO_RIG_LIST=$((SKIPPED_NO_RIG_LIST + 1))
        continue
    fi

    set_rig_args "$gate_id"
    [ -z "$RIG_SUSPENDED" ] || continue
    # Re-fetch authoritative gate details: the bead.created payload omits
    # the gate type, so the event alone cannot tell a human gate from a timer/gh
    # gate. `gc bd show --json` returns an array; normalize to the first row.
    GATE_JSON="$(gc bd show "$gate_id" ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --json 2>/dev/null \
        | jq -c 'if type == "array" then .[0] else . end' 2>/dev/null)" || continue
    [ -n "$GATE_JSON" ] && [ "$GATE_JSON" != "null" ] || continue

    # The gate's type: bd's own gates carry it in await_type; a gate a formula
    # [steps.gate] synthesized carries it in gc.gate_type, because gc's stores
    # have no await_type column to write.
    GATE_TYPE="$(printf '%s' "$GATE_JSON" | jq -r \
        'if (.await_type // "") != "" then .await_type else (.metadata."gc.gate_type" // "") end' 2>/dev/null)"
    STATUS="$(printf '%s' "$GATE_JSON" | jq -r '.status // ""' 2>/dev/null)"
    # Only OPEN human gates. A gate resolved as fast as it was created needs no
    # nudge; non-human gates have their own (auto) watchers.
    [ "$GATE_TYPE" = "human" ] || continue
    [ "$STATUS" = "open" ] || continue

    # Resolve the addressee: assignee -> gc.deferred_assignee -> escalation
    # recipient. Both null and empty-string are treated as "unset" (a stripped
    # assignee can land as "" rather than null), so an automated gate that
    # names its resolver only in gc.deferred_assignee is routed there, not to
    # the human fallback. Ad-hoc gates set neither and fall through to human.
    ADDRESSEE="$(printf '%s' "$GATE_JSON" | jq -r \
        '[.assignee, .metadata."gc.deferred_assignee"]
         | map(select(. != null and . != "")) | (.[0] // "")' 2>/dev/null)"
    # The rig-namespace checks, only in a city that may have a guarded rig.
    if [ -n "$CITY_MAY_GUARD" ]; then
        if [ -n "$ADDRESSEE" ]; then
            # The gate was read from RIG_ARG2's store; that rig's namespace judges
            # its addressee (and the prefix's rig too, if it differs).
            if ! rig_namespace_admits_row "$RIGS_JSON" "$RIG_ARG2" "$gate_id" "$ADDRESSEE"; then
                SKIPPED_FOREIGN=$((SKIPPED_FOREIGN + 1))
                continue
            fi
        else
            # A guarded rig's gate that names no addressee is judged by its
            # creation stamp, as the store guard judges such a row: one another
            # city's guard created (gc.owner_namespace names another namespace) is
            # that city's, and its escalation is not this city's to send. An
            # unstamped gate keeps the escalation fallback.
            owner_stamp="$(printf '%s' "$GATE_JSON" | jq -r '.metadata."gc.owner_namespace" // "" | tostring' 2>/dev/null)" || owner_stamp="?"
            if rig_namespace_row_stamp_names_other "$RIGS_JSON" "$RIG_ARG2" "$gate_id" "$owner_stamp"; then
                SKIPPED_FOREIGN=$((SKIPPED_FOREIGN + 1))
                continue
            fi
        fi
    fi
    [ -n "$ADDRESSEE" ] || ADDRESSEE="$ESCALATION_RECIPIENT"

    TITLE="$(printf '%s' "$GATE_JSON" | jq -r '.title // ""' 2>/dev/null)"
    DESC="$(printf '%s' "$GATE_JSON" | jq -r '.description // ""' 2>/dev/null)"

    SUBJECT="Human gate awaiting you: $gate_id"
    BODY="A human gate ($gate_id) was created and awaits your resolution."
    [ -n "$TITLE" ] && BODY="$BODY
Title: $TITLE"
    [ -n "$DESC" ] && BODY="$BODY
$DESC"
    # `gc bd close` rather than `gc bd gate resolve` (equivalent for a gate):
    # a split city refuses the latter for a gate its class binding holds.
    BODY="$BODY
Resolve with: gc bd close $gate_id"

    # `gc mail send --notify` mails the addressee and nudges them when they are
    # a real session; it skips the nudge for the "human" recipient natively.
    # Loud-fail: on an undeliverable send, surface it and do NOT record the
    # gate as notified, so the next sweep retries.
    if gc mail send "$ADDRESSEE" -s "$SUBJECT" -m "$BODY" --notify >/dev/null 2>&1; then
        STATE="$(echo "$STATE" | jq --arg k "$gate_id" --arg now "$NOW" '.[$k] = $now')"
        NOTIFIED=$((NOTIFIED + 1))
    else
        echo "notify-on-human-gate-creation: FAILED to notify addressee '$ADDRESSEE' of human gate $gate_id (will retry next sweep)" >&2
        FAILED=$((FAILED + 1))
    fi
done <<EOF
$GATE_IDS
EOF

# Prune entries older than RETENTION so the state file stays bounded.
RETENTION_S="$(duration_to_seconds "$RETENTION")"
STATE="$(echo "$STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select((now - (.value | fromdateiso8601)) <= $keep))')" || true

# Atomic write: temp file in the same dir, then rename.
TMP="$(mktemp "$PACK_STATE_DIR/.notify-on-human-gate-creation-state.XXXXXX")"
printf '%s\n' "$STATE" > "$TMP"
mv -f "$TMP" "$STATE_FILE"

if [ "$NOTIFIED" -gt 0 ]; then
    echo "notify-on-human-gate-creation: notified $NOTIFIED human gate addressee(s)"
fi
if [ "$SKIPPED_FOREIGN" -gt 0 ]; then
    echo "notify-on-human-gate-creation: left $SKIPPED_FOREIGN gate(s) in guarded rigs that are not this city's (an addressee it cannot vouch for, or another city's creation stamp)"
fi

# Loud-fail: state has been written (successes are deduped), so a non-zero exit
# now surfaces the per-gate failure lines above to the controller log without
# losing the recorded successes. exit 0 would swallow them (#4543). A run that
# could not read the rig list in a city that may have a guarded rig fails the
# same way, so the controller records order.failed, which the dashboard's
# attention list and gc doctor report.
if [ "$SKIPPED_NO_RIG_LIST" -gt 0 ]; then
    echo "notify-on-human-gate-creation: gc rig list failed in a city that may have a guarded rig (federation = \"rig\"); $SKIPPED_NO_RIG_LIST created gate(s) not checked or notified this run" >&2
fi
if [ "$FAILED" -gt 0 ]; then
    echo "notify-on-human-gate-creation: $FAILED human gate addressee(s) failed to notify (see above; will retry)" >&2
fi
if [ "$FAILED" -gt 0 ] || [ "$SKIPPED_NO_RIG_LIST" -gt 0 ]; then
    exit 1
fi
