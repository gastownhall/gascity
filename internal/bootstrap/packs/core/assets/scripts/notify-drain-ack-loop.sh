#!/usr/bin/env bash
# notify-drain-ack-loop — tell a person when one work bead keeps cycling
# through drain-ack with its started claim kept.
#
# The controller records session.drain_acked_with_assigned_work each time a
# session's drain-ack stop keeps a work bead assigned. Its payload's reason
# says the stop's shape; started_claim_kept is a stop that kept an
# in_progress claim the session's started records name, on no dispatch hold.
# One such stop is ordinary (a turn cap, a handoff); the same bead kept that
# way stop after stop is a loop nothing in the city ends by itself: a seat
# wakes for its claim, cannot finish it, and stops again. This script, run by
# the notify-on-drain-ack-loop event order, mails it through escalate.sh to
# $GC_ESCALATION_RECIPIENT (default "human"):
#
#   - it counts each bead's started_claim_kept cycles inside the window
#     ($GC_DRAIN_ACK_LOOP_WINDOW, default 1h); events of one (bead, session)
#     less than a minute apart are one cycle, as a real cycle spans a sleep, a
#     wake and an agent turn;
#   - a bead with at least $GC_DRAIN_ACK_LOOP_THRESHOLD cycles (default 3) is
#     read again and escalated only while it is still in_progress, assigned to
#     a session those events name, and on no dispatch hold;
#   - the incident key and fingerprint are the bead, so a loop mails once and
#     escalate.sh reminds once a day while it lasts.
#
# The mail gives the park command; this script never writes the bead. Parking
# is the recipient's decision.
#
# An unusable setting, or an events or bead read that fails or does not
# decode, exits non-zero with the cause on stderr and mails nothing.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
WINDOW="${GC_DRAIN_ACK_LOOP_WINDOW:-1h}"
THRESHOLD="${GC_DRAIN_ACK_LOOP_THRESHOLD:-3}"
EVENT_TYPE="session.drain_acked_with_assigned_work"
LOOP_REASON="started_claim_kept"
# Events of one (bead, session) closer than this are one cycle.
CYCLE_MIN_SECONDS=60

fail() {
    echo "notify-drain-ack-loop: $*" >&2
    exit 1
}

# A setting it cannot use fails the order rather than counting wrong.
[[ "$WINDOW" =~ ^([0-9]+[hms])+$ ]] ||
    fail "GC_DRAIN_ACK_LOOP_WINDOW=$WINDOW is not a duration like 1h or 30m"
[[ "$THRESHOLD" =~ ^[1-9][0-9]*$ ]] ||
    fail "GC_DRAIN_ACK_LOOP_THRESHOLD=$THRESHOLD is not a positive whole number"

command -v jq >/dev/null 2>&1 || fail "jq is required but not found in PATH"

resolve_escalate_script() {
    local candidate
    local pack
    local system_packs="${GC_SYSTEM_PACKS_DIR:-$CITY/.gc/system/packs}"

    if [ -n "${GC_ESCALATE_SCRIPT:-}" ]; then
        printf '%s\n' "$GC_ESCALATE_SCRIPT"
        return
    fi
    for pack in ${GC_ESCALATE_SEARCH_PACKS:-gastown maintenance bd core}; do
        candidate="$system_packs/$pack/assets/scripts/escalate.sh"
        if [ -x "$candidate" ]; then
            printf '%s\n' "$candidate"
            return
        fi
    done
    printf '%s\n' "$SCRIPT_DIR/escalate.sh"
}

ESCALATE_SCRIPT="$(resolve_escalate_script)"

EVENTS="$(gc events --type "$EVENT_TYPE" --since "$WINDOW")" ||
    fail "reading $EVENT_TYPE events since $WINDOW failed"

# One compact line per looping bead: its ID, its cycle count, the sessions and
# templates its counted events name. ts is RFC 3339 with an optional fraction
# and a Z or numeric offset.
LOOPS="$(printf '%s\n' "$EVENTS" | jq -c -s \
    --arg type "$EVENT_TYPE" --arg reason "$LOOP_REASON" \
    --argjson min "$CYCLE_MIN_SECONDS" --argjson threshold "$THRESHOLD" '
    def epoch: sub("\\.[0-9]+"; "")
        | if endswith("Z") then fromdateiso8601
          else (.[0:19] + "Z" | fromdateiso8601)
               - ((if .[19:20] == "-" then -1 else 1 end)
                  * ((.[20:22] | tonumber) * 3600 + (.[23:25] | tonumber) * 60))
          end;
    [ .[] | select(type == "object" and .type == $type and .payload.reason == $reason
                   and (.payload.bead_id // "") != "" and (.payload.session_id // "") != "")
      | { seq: .seq, at: (.ts | epoch), bead: .payload.bead_id, session: .payload.session_id,
          template: (.payload.template // .subject // "") } ]
    | unique_by(.seq)
    | group_by(.bead)[]
    | . as $events
    | ([ group_by(.session)[]
         | sort_by(.at)
         | reduce .[] as $e ({n: 0, last: null};
             if .last == null or ($e.at - .last) >= $min then {n: (.n + 1), last: $e.at} else . end)
         | .n ] | add) as $cycles
    | select($cycles >= $threshold)
    | { bead: $events[0].bead, cycles: $cycles,
        sessions: ($events | map(.session) | unique),
        templates: ($events | map(.template) | map(select(. != "")) | unique) }')" ||
    fail "decoding $EVENT_TYPE events failed"
[ -n "$LOOPS" ] || exit 0

# Every bead is read before anything is sent, so a failed read mails nothing.
ALERTS=()
while IFS= read -r loop; do
    [ -n "$loop" ] || continue
    bead="$(printf '%s' "$loop" | jq -r '.bead')"
    shown="$(gc bd show "$bead" --json)" || fail "reading bead $bead failed"
    verdict="$(printf '%s' "$shown" | jq -e -c --arg id "$bead" --argjson loop "$loop" '
        (if type == "array" then .[0] else . end) as $b
        | if ($b | type) != "object" or $b.id != $id then error("no bead \($id)") else . end
        | { title: ($b.title // ""),
            parkable: ($b.status == "in_progress"
                       and (($loop.sessions | index($b.assignee // "")) != null)
                       and ((($b.labels // []) | map(select(. == "hold:mayor" or . == "hold:external")) | length) == 0)) }')" ||
        fail "decoding bead $bead failed"
    if [ "$(printf '%s' "$verdict" | jq -r '.parkable')" = true ]; then
        ALERTS+=("$(jq -c -n --argjson loop "$loop" --argjson v "$verdict" '$loop + {title: $v.title}')")
    fi
done <<< "$LOOPS"

FAILED=0
for alert in ${ALERTS[@]+"${ALERTS[@]}"}; do
    bead="$(printf '%s' "$alert" | jq -r '.bead')"
    title="$(printf '%s' "$alert" | jq -r '.title')"
    cycles="$(printf '%s' "$alert" | jq -r '.cycles')"
    sessions="$(printf '%s' "$alert" | jq -r '.sessions | join(", ")')"
    templates="$(printf '%s' "$alert" | jq -r 'if (.templates | length) == 0 then "unknown" else .templates | join(", ") end')"
    body="Bead $bead ($title) went through $cycles drain-ack cycles in the last $WINDOW with its started claim kept assigned.

Session(s): $sessions
Template: $templates

Each cycle a session stopped while keeping this claim and was woken for it
again. Nothing in the city ends that by itself.

To park it:
  bd set-state $bead hold=mayor --reason \"drain-ack loop\"
The seat's next own drain-ack then releases the claim (open, unassigned,
held). If the bead was routed to the wrong agent, also clear its gc.routed_to
metadata.

See: gc events --type $EVENT_TYPE --since $WINDOW"
    if ! "$ESCALATE_SCRIPT" --subject "Bead $bead keeps drain-acking with its claim kept" \
        --message "$body" --severity HIGH \
        --incident-key "drain-ack-loop:$bead" --fingerprint "$bead"; then
        echo "notify-drain-ack-loop: could not escalate the drain-ack loop of $bead" >&2
        FAILED=$((FAILED + 1))
    fi
done

if [ "$FAILED" -gt 0 ]; then
    exit 1
fi
