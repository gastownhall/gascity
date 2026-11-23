#!/usr/bin/env bash
# notify-drain-ack-loop — tell a person when one work bead keeps
# drain-acking with a started claim kept, and give them the park recipe.
#
# A misrouted work bead can livelock the dispatcher
# (gastownhall/gascity#6940): a pool session claims the bead, correctly
# refuses to execute or close it, escalates, and drain-acks still
# assigned — on repeat, forever, with nothing dispatch reads ever
# changing. The reconcilers record session.drain_acked_with_assigned_work
# with reason started_claim_kept for exactly that shape, so this script,
# run by the notify-on-drain-ack-loop event order, counts those cycles per
# bead inside a window and, past the cap, mails through escalate.sh to
# $GC_ESCALATION_RECIPIENT (default "human", the operator inbox the
# dashboard shows):
#
#   - one bead mails once: the incident key is drain-ack-loop:<bead> and
#     its fingerprint the bead, so a window the order re-reads while the
#     loop lasts is not mailed again (escalate.sh reminds once a day while
#     it stands);
#   - events for the same (bead, session) less than 60s apart count as one
#     cycle: a real cycle spans a sleep, a wake and an agent turn;
#   - a bead no longer in_progress, no longer assigned to a session the
#     events name, or on a dispatch hold is not mailed: the loop is over
#     or the bead is already parked.
#
# The mail gives the park recipe (hold:mayor), which makes the bead stop
# looking dispatch-eligible without a human awake to act; the seat's next
# own drain-ack then releases the claim. The script itself never writes
# the bead.
#
# Loud-fail: an event read or a bead read that fails, output that does not
# decode, or an invalid window or threshold exits non-zero, which is what
# makes the controller log the order's output, and nothing is mailed. The
# next event that still finds the loop runs the order again.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
WINDOW="${GC_DRAIN_ACK_LOOP_WINDOW:-1h}"
THRESHOLD="${GC_DRAIN_ACK_LOOP_THRESHOLD:-3}"

if ! command -v jq >/dev/null 2>&1; then
    echo "notify-drain-ack-loop: jq is required but not found in PATH" >&2
    exit 1
fi

# A cycle spans a sleep, a wake and an agent turn; closer events for one
# (bead, session) are re-reads of the same cycle.
CYCLE_SECONDS=60

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

# escalate_refused_incident_flags RC STDERR is true when an escalate.sh
# override refused the incident flags as a usage error, as one written
# before them (the 1.5.0 contract) does: with exit 2, the usage-error status
# (the 1.5.0 escalate.sh, argparse, Go's flag package), or with a message
# that names the flag (getopt-style parsers exit 1). The core escalate.sh
# next to this script takes the flags, so no failure of it is read as a
# refusal.
escalate_refused_incident_flags() {
    [ "$1" -ne 0 ] || return 1
    if [ "$ESCALATE_SCRIPT" -ef "$SCRIPT_DIR/escalate.sh" ]; then
        return 1
    fi
    [ "$1" -ne 2 ] || return 0
    case "$2" in
        *incident-key*|*--fingerprint*|*resolve-incident*) return 0 ;;
    esac
    return 1
}

# raise_incident KEY FINGERPRINT SUBJECT MESSAGE [SEVERITY] escalates
# MESSAGE as incident KEY, which escalate.sh keeps quiet while FINGERPRINT
# is unchanged. An override that refuses the incident flags is sent the
# alert again without them, so it keeps alerting, undeduplicated, rather
# than losing the alert.
raise_incident() {
    local rc=0 err
    local alert=(--subject "$3" --message "$4")
    [ -z "${5:-}" ] || alert+=(--severity "$5")
    # stdout passes through; stderr is held to tell a refusal from a failure.
    { err=$("$ESCALATE_SCRIPT" "${alert[@]}" \
        --incident-key "$1" --fingerprint "$2" 2>&1 >&3 3>&-); } 3>&1 || rc=$?
    if ! escalate_refused_incident_flags "$rc" "$err"; then
        [ -z "$err" ] || printf '%s\n' "$err" >&2
        return "$rc"
    fi
    echo "notify-drain-ack-loop: $ESCALATE_SCRIPT predates incident keys (exit $rc on --incident-key); sent the alert without them, so it is not deduplicated" >&2
    "$ESCALATE_SCRIPT" "${alert[@]}"
}

# gc events --since takes a Go duration; anything else is a configuration
# error the operator must hear about, not an empty window.
if ! printf '%s' "$WINDOW" | grep -Eq '^[0-9]+(\.[0-9]+)?(ms|s|m|h)$'; then
    echo "notify-drain-ack-loop: GC_DRAIN_ACK_LOOP_WINDOW must be a duration like 1h or 30m, got \"$WINDOW\"" >&2
    exit 1
fi
if ! printf '%s' "$THRESHOLD" | grep -Eq '^[1-9][0-9]*$'; then
    echo "notify-drain-ack-loop: GC_DRAIN_ACK_LOOP_THRESHOLD must be a whole number of at least 1, got \"$THRESHOLD\"" >&2
    exit 1
fi

if ! EVENTS="$(gc events --type session.drain_acked_with_assigned_work --since "$WINDOW")"; then
    echo "notify-drain-ack-loop: could not read the session.drain_acked_with_assigned_work events of the last $WINDOW" >&2
    exit 1
fi

# One compact line per event, in sequence order: seq, ts (as epoch seconds,
# so the cycle dedupe needs no date portability), then the fields the count
# needs. The typed payload sits directly under .payload. A ts that does not
# decode fails the whole read: a window that cannot be ordered cannot be
# counted.
if ! ROWS="$(printf '%s\n' "$EVENTS" | jq -c -s '
    [ .[] | select(type == "object"
        and .type == "session.drain_acked_with_assigned_work"
        and (.payload.reason // "") == "started_claim_kept") ]
    | unique_by(.seq) | sort_by(.seq) | .[]
    | { seq: .seq, at: (.ts | sub("\\.[0-9]+"; "") | fromdateiso8601),
        session: (.payload.session_id // ""),
        bead: (.payload.bead_id // ""),
        template: (.payload.template // "") }')"; then
    echo "notify-drain-ack-loop: could not decode the events gc events printed" >&2
    exit 1
fi

# Count cycles per bead: an event for a (bead, session) within 60s of the
# one before it is the same cycle. counts holds each bead's cycle count;
# sessions_of and templates_of remember what the mail names.
declare -A last_seen=() counts=() sessions_of=() templates_of=()
while IFS= read -r row; do
    [ -n "$row" ] || continue
    at="$(printf '%s' "$row" | jq -r '.at')"
    session="$(printf '%s' "$row" | jq -r '.session')"
    bead="$(printf '%s' "$row" | jq -r '.bead')"
    template="$(printf '%s' "$row" | jq -r '.template')"
    [ -n "$bead" ] || continue
    case "$at" in ''|*[!0-9]*)
        echo "notify-drain-ack-loop: could not read the timestamp of event $row" >&2
        exit 1 ;;
    esac
    key="$bead|$session"
    if [ -n "${last_seen[$key]:-}" ] && [ "$((at - last_seen[$key]))" -lt "$CYCLE_SECONDS" ]; then
        continue
    fi
    last_seen[$key]="$at"
    counts[$bead]=$(( ${counts[$bead]:-0} + 1 ))
    case " ${sessions_of[$bead]:-} " in
        *" $session "*) ;;
        *) sessions_of[$bead]="${sessions_of[$bead]:-} $session" ;;
    esac
    if [ -n "$template" ] && [ -z "${templates_of[$bead]:-}" ]; then
        templates_of[$bead]="$template"
    fi
done <<< "$ROWS"

FAILED=0
for bead in "${!counts[@]}"; do
    count="${counts[$bead]}"
    [ "$count" -ge "$THRESHOLD" ] || continue

    if ! BEAD_JSON="$(gc bd show "$bead" --json)"; then
        echo "notify-drain-ack-loop: could not read bead $bead" >&2
        exit 1
    fi
    if ! BEAD="$(printf '%s' "$BEAD_JSON" | jq -c 'if (type == "array" and length == 1) then .[0] elif type == "object" then . else error("not one bead") end')"; then
        echo "notify-drain-ack-loop: could not decode what gc bd show printed for $bead" >&2
        exit 1
    fi
    status="$(printf '%s' "$BEAD" | jq -r '.status // ""')"
    assignee="$(printf '%s' "$BEAD" | jq -r '.assignee // ""')"
    title="$(printf '%s' "$BEAD" | jq -r '.title // ""')"
    held="$(printf '%s' "$BEAD" | jq -r '[(.labels // [])[] | ascii_downcase | ltrimstr(" ") | rtrimstr(" ")] | any(. == "hold:mayor" or . == "hold:external")')"

    # The loop is over, or already parked: in_progress, still assigned to a
    # session the events name, and on no dispatch hold.
    [ "$status" = "in_progress" ] || continue
    [ "$held" = "false" ] || continue
    case " ${sessions_of[$bead]:-} " in
        *" $assignee "*) ;;
        *) continue ;;
    esac

    sessions="$(printf '%s' "${sessions_of[$bead]}" | sed 's/^ //')"
    template="${templates_of[$bead]:-}"
    body="Bead $bead ($title) drain-acked with a started claim kept $count times in the last $WINDOW, across session(s): $sessions${template:+ (template $template)}. Sessions claim it, refuse to execute or close it, escalate, and drain-ack still assigned — nothing dispatch reads changes between cycles.

Park the bead so it stops looking dispatch-eligible:
    gc bd set-state $bead hold=mayor --reason \"drain-ack loop\"

If it was misrouted, also clear its routing:
    gc bd update $bead --set-metadata gc.routed_to=

Once held, the seat running gc runtime drain-ack releases the claim (open, unassigned, held).
See: gc events --type session.drain_acked_with_assigned_work --since $WINDOW"
    if ! raise_incident "drain-ack-loop:$bead" "$bead" \
        "Bead $bead is livelocked in a drain-ack loop ($count started claims kept in $WINDOW)" \
        "$body" HIGH; then
        echo "notify-drain-ack-loop: could not escalate the drain-ack loop of $bead" >&2
        FAILED=$((FAILED + 1))
    fi
done

if [ "$FAILED" -gt 0 ]; then
    exit 1
fi
