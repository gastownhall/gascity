#!/usr/bin/env bash
# renudge-stale-human-gates — re-mail + re-nudge the addressee of a human gate
# that has stayed OPEN past a staleness threshold, repeating on an interval.
#
# notify-on-human-gate-creation notifies the addressee ONCE, at creation. A
# human gate that is created, notified, and then left unresolved gets no
# further reminder: the creation mail scrolls off, the human forgets, and the
# only gate watcher (`gc bd gate check`) skips human gates entirely. Doctrine
# papers over the gap by hand ("a human gate open past a threshold gets
# re-nudged, repeating on the interval"); this order ships that reflex, and it
# is also the safety net for a creation notify that was undeliverable beyond
# its short lookback window.
#
# Runs as a cooldown sweep. Each run:
#   1. Enumerates OPEN gates across HQ and every rig (`gc bd gate list` is
#      open-only by default), keeping only await_type == "human" gates.
#   2. For each human gate whose age exceeds GC_STALE_GATE_THRESHOLD and whose
#      last re-nudge is older than GC_STALE_GATE_RENUDGE_INTERVAL, re-fetches
#      the gate (the list projection omits assignee/metadata), re-verifies it
#      is still an open human gate, resolves the addressee and re-notifies.
#
# Addressee resolution (first non-empty wins), identical to the creation notify
# so a gate is always re-nudged at the same address it was first notified:
#   1. the gate's assignee
#   2. gc.deferred_assignee metadata (formula/molecule gates strip the
#      assignee here at create time, molecule.go stripDeferredAssignee)
#   3. $GC_ESCALATION_RECIPIENT (default "human")
#
# Notification rides `gc mail send --notify`, which mails the addressee and
# nudges them when they are a real session — and deliberately skips the
# tmux-nudge for the "human" recipient (humans have no session to poke;
# cmd_mail.go guards `to != "human"`).
#
# Loud-fail (gastownhall/gascity#4543): an undeliverable send surfaces to the
# controller log (stderr) and is NOT recorded, so the next sweep retries it. It
# never silently evaporates.
#
# Dedup / cadence: per-gate last-re-nudge state lives in
# $GC_PACK_STATE_DIR/renudge-stale-human-gates-state.json (city- and
# pack-scoped). An entry is refreshed on every successful re-nudge, so a live
# stale gate's entry never ages past the retention window; a resolved gate stops
# being refreshed and is pruned after GC_STALE_GATE_STATE_RETENTION. This
# retention-based prune (rather than pruning to the current open set) keeps the
# cadence memory intact across a transient per-rig enumeration failure, so a
# rig that briefly fails to list does not trigger an early re-nudge storm.
#
# Cross-rig: gates are enumerated per scope (HQ + each non-HQ rig), so the
# owning rig is known without a prefix lookup; the re-fetch is scoped with
# `--rig` (a gc flag, not a bd flag, so it routes through `gc bd`). Mail send is
# city-scoped: recipients (mayor / human / coordinators) are city-level
# identities.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "renudge-stale-human-gates"

# jq is a hard dependency: it decodes the gate list and the re-fetched gate
# record. Without it every re-nudge would be silently skipped. Fail loud.
if ! command -v jq >/dev/null 2>&1; then
    echo "renudge-stale-human-gates: jq is required but not found in PATH" >&2
    exit 1
fi

CITY="${GC_CITY:-.}"
# A human gate must be open at least this long before its FIRST staleness
# re-nudge. Below this the creation notify already covered it; this avoids
# double-notifying a freshly created gate.
THRESHOLD="${GC_STALE_GATE_THRESHOLD:-1h}"
# Minimum time between successive re-nudges of the same gate ("repeating on the
# interval"). A gate is re-nudged at most once per this window.
RENUDGE_INTERVAL="${GC_STALE_GATE_RENUDGE_INTERVAL:-1h}"
# Dedup entries older than this are pruned so the state file stays bounded.
# Must exceed RENUDGE_INTERVAL (a live gate's entry is refreshed each re-nudge,
# so it never ages past this; only a resolved gate's entry reaches it).
RETENTION="${GC_STALE_GATE_STATE_RETENTION:-24h}"
# Human channel for gates with no resolvable assignee. escalate.sh and the
# creation notify use the same default, keeping the "notify the human" address
# consistent across all three.
ESCALATION_RECIPIENT="${GC_ESCALATION_RECIPIENT:-human}"

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
STATE_FILE="$PACK_STATE_DIR/renudge-stale-human-gates-state.json"
mkdir -p "$PACK_STATE_DIR"

# Convert a simple Go-style duration (Ns/Nm/Nh/Nd) to whole seconds.
duration_to_seconds() {
    case "$1" in
        *d) echo $(( ${1%d} * 86400 )) ;;
        *h) echo $(( ${1%h} * 3600 )) ;;
        *m) echo $(( ${1%m} * 60 )) ;;
        *s) echo "${1%s}" ;;
        *)  echo "$1" ;;
    esac
}

# Parse an ISO-8601 UTC timestamp (e.g. 2026-07-22T13:54:16Z) to epoch seconds.
# Empty on failure so callers can skip an unparseable gate rather than misage it.
# Portable across GNU and BSD/macOS date, matching wisp-compact.sh: GNU `date -d`
# first, then BSD `date -ju -f` (forcing UTC to match GNU), with a no-Z layout
# for older timestamps. Without the BSD fallbacks every gate would be skipped on
# macOS (BSD date rejects -d), silently disabling the whole sweep.
iso_to_epoch() {
    [ -n "$1" ] || { echo ""; return 0; }
    date -u -d "$1" +%s 2>/dev/null || \
        date -ju -f "%Y-%m-%dT%H:%M:%SZ" "$1" +%s 2>/dev/null || \
        date -ju -f "%Y-%m-%dT%H:%M:%S" "$1" +%s 2>/dev/null || \
        echo ""
}

THRESHOLD_S="$(duration_to_seconds "$THRESHOLD")"
RENUDGE_INTERVAL_S="$(duration_to_seconds "$RENUDGE_INTERVAL")"
NOW_EPOCH="$(date -u +%s)"
NOW_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Build the list of scopes to sweep: HQ (empty scope, bare gc bd) plus every
# non-HQ rig. `gc bd gate list` without --rig is HQ-scoped from the city cwd,
# so per-rig gates are invisible to a bare query — walk each rig explicitly.
# The HQ entry is excluded (gc rig list reports the city root as an hq=true
# pseudo-rig that `gc --rig <cityName>` cannot resolve), matching orphan-sweep.
SCOPES_FILE="$(mktemp "$PACK_STATE_DIR/.renudge-scopes.XXXXXX")"
trap 'rm -f "$SCOPES_FILE"' EXIT
printf '\n' > "$SCOPES_FILE" # HQ scope: an empty line
RIGS_JSON="$(gc rig list --json 2>/dev/null || true)"
if [ -n "$RIGS_JSON" ]; then
    printf '%s' "$RIGS_JSON" \
        | jq -r '(.rigs // [])[] | select(.hq != true) | .name' 2>/dev/null \
        >> "$SCOPES_FILE" || true
fi

# Load dedup state (object mapping "<gate-id>" -> ISO timestamp of last
# re-nudge). A missing or corrupt file resets to an empty object.
STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
echo "$STATE" | jq -e 'type == "object"' >/dev/null 2>&1 || STATE='{}'

RENUDGED=0
FAILED=0
while IFS= read -r scope; do
    RIG_ARG1=""
    RIG_ARG2=""
    if [ -n "$scope" ]; then
        RIG_ARG1="--rig"
        RIG_ARG2="$scope"
    fi

    # List OPEN gates in this scope (open-only by default). --limit 0 =
    # unlimited so a busy rig past the default 50 is not silently truncated.
    # Best-effort: a read failure (API down, unreachable rig) must not crash
    # the controller's order loop — skip this scope and continue.
    GATES_JSON="$(gc bd gate list ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --limit 0 --json 2>/dev/null)" || continue
    [ -n "$GATES_JSON" ] && [ "$GATES_JSON" != "null" ] || continue

    # Keep only human gates, emit "<id>\t<created_at>". Non-human gates (timer,
    # gh, bead, and the legacy await_type=null workflow gates) are dropped here.
    HUMAN_GATES="$(printf '%s' "$GATES_JSON" \
        | jq -r '(if type == "array" then . else [.] end)[]
                 | select(.await_type == "human" and .status == "open")
                 | "\(.id)\t\(.created_at // "")"' 2>/dev/null)" || HUMAN_GATES=""
    [ -n "$HUMAN_GATES" ] || continue

    while IFS="$(printf '\t')" read -r gate_id created_at; do
        [ -n "$gate_id" ] || continue

        # Age gate: only gates open past the staleness threshold.
        created_epoch="$(iso_to_epoch "$created_at")"
        [ -n "$created_epoch" ] || continue
        age=$(( NOW_EPOCH - created_epoch ))
        [ "$age" -ge "$THRESHOLD_S" ] || continue

        # Cadence gate: skip if re-nudged within the interval. A missing entry
        # (never re-nudged) is eligible immediately once past the threshold.
        last_iso="$(echo "$STATE" | jq -r --arg k "$gate_id" '.[$k] // ""' 2>/dev/null)"
        if [ -n "$last_iso" ]; then
            last_epoch="$(iso_to_epoch "$last_iso")"
            if [ -n "$last_epoch" ] && [ $(( NOW_EPOCH - last_epoch )) -lt "$RENUDGE_INTERVAL_S" ]; then
                continue
            fi
        fi

        # Re-fetch: the list projection omits assignee/metadata, and re-reading
        # closes the tiny window where the gate resolved since the list. Confirm
        # it is still an open human gate before sending.
        GATE_JSON="$(gc bd show "$gate_id" ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --json 2>/dev/null \
            | jq -c 'if type == "array" then .[0] else . end' 2>/dev/null)" || continue
        [ -n "$GATE_JSON" ] && [ "$GATE_JSON" != "null" ] || continue
        AWAIT_TYPE="$(printf '%s' "$GATE_JSON" | jq -r '.await_type // ""' 2>/dev/null)"
        STATUS="$(printf '%s' "$GATE_JSON" | jq -r '.status // ""' 2>/dev/null)"
        [ "$AWAIT_TYPE" = "human" ] || continue
        [ "$STATUS" = "open" ] || continue

        # Addressee: assignee -> gc.deferred_assignee -> escalation recipient.
        # Both null and empty-string count as "unset" (a stripped assignee can
        # land as "" rather than null).
        ADDRESSEE="$(printf '%s' "$GATE_JSON" | jq -r \
            '[.assignee, .metadata."gc.deferred_assignee"]
             | map(select(. != null and . != "")) | (.[0] // "")' 2>/dev/null)"
        [ -n "$ADDRESSEE" ] || ADDRESSEE="$ESCALATION_RECIPIENT"

        TITLE="$(printf '%s' "$GATE_JSON" | jq -r '.title // ""' 2>/dev/null)"
        DESC="$(printf '%s' "$GATE_JSON" | jq -r '.description // ""' 2>/dev/null)"
        age_h=$(( age / 3600 ))
        age_m=$(( (age % 3600) / 60 ))

        SUBJECT="Reminder — human gate still open: $gate_id"
        BODY="Human gate $gate_id has been open and unresolved for ${age_h}h${age_m}m and still awaits you."
        [ -n "$TITLE" ] && BODY="$BODY
Title: $TITLE"
        [ -n "$DESC" ] && BODY="$BODY
$DESC"
        BODY="$BODY
Resolve with: gc bd gate resolve $gate_id"

        # Loud-fail: record the re-nudge only on a delivered send, so an
        # undeliverable one surfaces and retries next sweep.
        if gc mail send "$ADDRESSEE" -s "$SUBJECT" -m "$BODY" --notify >/dev/null 2>&1; then
            STATE="$(echo "$STATE" | jq --arg k "$gate_id" --arg now "$NOW_ISO" '.[$k] = $now')"
            RENUDGED=$((RENUDGED + 1))
        else
            echo "renudge-stale-human-gates: FAILED to re-notify addressee '$ADDRESSEE' of stale human gate $gate_id (will retry next sweep)" >&2
            FAILED=$((FAILED + 1))
        fi
    done <<INNER
$HUMAN_GATES
INNER
done < "$SCOPES_FILE"

# Prune entries older than RETENTION so the state file stays bounded.
RETENTION_S="$(duration_to_seconds "$RETENTION")"
STATE="$(echo "$STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select((now - (.value | fromdateiso8601)) <= $keep))')" || true

# Atomic write: temp file in the same dir, then rename.
TMP="$(mktemp "$PACK_STATE_DIR/.renudge-stale-human-gates-state.XXXXXX")"
printf '%s\n' "$STATE" > "$TMP"
mv -f "$TMP" "$STATE_FILE"

if [ "$RENUDGED" -gt 0 ]; then
    echo "renudge-stale-human-gates: re-notified $RENUDGED stale human gate addressee(s)"
fi

# Loud-fail: state has been written (successful re-nudges are deduped), so a
# non-zero exit now surfaces the per-gate failure lines above to the controller
# log without losing the recorded successes. exit 0 would swallow them (#4543).
if [ "$FAILED" -gt 0 ]; then
    echo "renudge-stale-human-gates: $FAILED stale human gate addressee(s) failed to re-notify (see above; will retry next sweep)" >&2
    exit 1
fi
