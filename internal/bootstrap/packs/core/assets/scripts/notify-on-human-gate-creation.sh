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
# event payload does not carry await_type — and, when the gate is an OPEN
# `human` gate, resolves the addressee and notifies them once. Idempotent:
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
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "notify-on-human-gate-creation"

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

# Build a prefix->rig lookup once. Best-effort: a single-rig city resolves
# nothing here and simply runs the bd/gc calls in their default scope.
RIGS_JSON="$(gc rig list --json 2>/dev/null || true)"

# Resolve a bead id's rig into RIG_ARG1/RIG_ARG2 ("--rig" "<name>"), or leave
# them empty when the prefix is unknown. Callers expand them with
# ${RIG_ARG1:+...} so an empty result adds no arguments under `set -u`. The HQ
# entry is excluded: `gc rig list` reports the city root as an hq=true
# pseudo-rig that `gc --rig <cityName>` cannot resolve, so HQ beads fall back
# to default scope, which is where they live.
set_rig_args() {
    RIG_ARG1=""
    RIG_ARG2=""
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
while IFS= read -r gate_id; do
    [ -n "$gate_id" ] || continue

    # Already notified? Refresh last-seen so this entry is not pruned and
    # re-notified while the creation event is still inside the lookback window.
    if echo "$STATE" | jq -e --arg k "$gate_id" 'has($k)' >/dev/null 2>&1; then
        STATE="$(echo "$STATE" | jq --arg k "$gate_id" --arg now "$NOW" '.[$k] = $now')"
        continue
    fi

    set_rig_args "$gate_id"
    # Re-fetch authoritative gate details: the bead.created payload omits
    # await_type, so the event alone cannot tell a human gate from a timer/gh
    # gate. `gc bd show --json` returns an array; normalize to the first row.
    GATE_JSON="$(gc bd show "$gate_id" ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --json 2>/dev/null \
        | jq -c 'if type == "array" then .[0] else . end' 2>/dev/null)" || continue
    [ -n "$GATE_JSON" ] && [ "$GATE_JSON" != "null" ] || continue

    AWAIT_TYPE="$(printf '%s' "$GATE_JSON" | jq -r '.await_type // ""' 2>/dev/null)"
    STATUS="$(printf '%s' "$GATE_JSON" | jq -r '.status // ""' 2>/dev/null)"
    # Only OPEN human gates. A gate resolved as fast as it was created needs no
    # nudge; non-human gates have their own (auto) watchers.
    [ "$AWAIT_TYPE" = "human" ] || continue
    [ "$STATUS" = "open" ] || continue

    # Resolve the addressee: assignee -> gc.deferred_assignee -> escalation
    # recipient. Both null and empty-string are treated as "unset" (a stripped
    # assignee can land as "" rather than null), so an automated gate that
    # names its resolver only in gc.deferred_assignee is routed there, not to
    # the human fallback. Ad-hoc gates set neither and fall through to human.
    ADDRESSEE="$(printf '%s' "$GATE_JSON" | jq -r \
        '[.assignee, .metadata."gc.deferred_assignee"]
         | map(select(. != null and . != "")) | (.[0] // "")' 2>/dev/null)"
    [ -n "$ADDRESSEE" ] || ADDRESSEE="$ESCALATION_RECIPIENT"

    TITLE="$(printf '%s' "$GATE_JSON" | jq -r '.title // ""' 2>/dev/null)"
    DESC="$(printf '%s' "$GATE_JSON" | jq -r '.description // ""' 2>/dev/null)"

    SUBJECT="Human gate awaiting you: $gate_id"
    BODY="A human gate ($gate_id) was created and awaits your resolution."
    [ -n "$TITLE" ] && BODY="$BODY
Title: $TITLE"
    [ -n "$DESC" ] && BODY="$BODY
$DESC"
    BODY="$BODY
Resolve with: gc bd gate resolve $gate_id"

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

# Loud-fail: state has been written (successes are deduped), so a non-zero exit
# now surfaces the per-gate failure lines above to the controller log without
# losing the recorded successes. exit 0 would swallow them (#4543).
if [ "$FAILED" -gt 0 ]; then
    echo "notify-on-human-gate-creation: $FAILED human gate addressee(s) failed to notify (see above; will retry)" >&2
    exit 1
fi
