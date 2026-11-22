#!/usr/bin/env bash
# notify-order-failing — tell a person when an order keeps failing, and clear
# the alert when it recovers.
#
# The controller records order.failing when an order's consecutive failures
# reach its alert threshold (re-raised at most hourly while the streak lasts),
# and order.recovered when a run completes after such a streak. The events
# alone reach only the event log, so this script, run by the
# notify-on-order-failing and notify-on-order-recovered event orders, mails
# them through escalate.sh to $GC_ESCALATION_RECIPIENT (default "human", the
# operator inbox the dashboard shows):
#
#   - a failing order mails once per streak: the incident key is the order,
#     and its fingerprint the streak's first failure, so the controller's
#     hourly re-alerts are not mailed again (escalate.sh reminds once a day
#     while the incident stands) and a new streak mails at once;
#   - a recovery resolves that incident and sends nothing.
#
# An alert about one of these two orders is ignored: a notifier that cannot
# send would otherwise fail, alert about itself and fail again.
#
# Both orders run this one script, which reads both event types from a
# lookback window that must exceed the controller's event-trigger cadence;
# re-reading an event is harmless, its incident is already recorded.
#
# Loud-fail: an escalation that fails, an event read that fails, or output
# that does not decode exits non-zero, which is what makes the controller log
# the order's output.
#
# An escalate.sh override written before incident keys refuses their flags;
# it is sent the alert again without them, so it still alerts, on every run
# that finds the event rather than once per streak.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
LOOKBACK="${GC_NOTIFY_ORDER_FAILING_LOOKBACK:-15m}"

if ! command -v jq >/dev/null 2>&1; then
    echo "notify-order-failing: jq is required but not found in PATH" >&2
    exit 1
fi

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

# escalate_refused_incident_flags RC STDERR is true when an escalate.sh override
# refused the incident flags as a usage error (exit 2, or a message naming the
# flag). The core escalate.sh next to this script takes the flags, so no
# failure of it is read as a refusal.
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

# raise_incident KEY FINGERPRINT SUBJECT MESSAGE escalates MESSAGE as incident
# KEY, which escalate.sh keeps quiet while FINGERPRINT is unchanged.
raise_incident() {
    local rc=0 err
    local alert=(--subject "$3" --message "$4" --severity HIGH)
    # stdout passes through; stderr is held to tell a refusal from a failure.
    { err=$("$ESCALATE_SCRIPT" "${alert[@]}" \
        --incident-key "$1" --fingerprint "$2" 2>&1 >&3 3>&-); } 3>&1 || rc=$?
    if ! escalate_refused_incident_flags "$rc" "$err"; then
        [ -z "$err" ] || printf '%s\n' "$err" >&2
        return "$rc"
    fi
    echo "notify-order-failing: $ESCALATE_SCRIPT predates incident keys (exit $rc on --incident-key); sent the alert without them, so it is not deduplicated" >&2
    "$ESCALATE_SCRIPT" "${alert[@]}"
}

# resolve_incident KEY tells escalate.sh that incident KEY cleared. An override
# that refuses the flag keeps no incidents, so there is nothing to clear.
resolve_incident() {
    local rc=0 err
    err=$("$ESCALATE_SCRIPT" --resolve-incident "$1" 2>&1 >/dev/null) || rc=$?
    if escalate_refused_incident_flags "$rc" "$err"; then
        return 0
    fi
    [ -z "$err" ] || printf '%s\n' "$err" >&2
    return "$rc"
}

# Read both types; a window that holds neither is a clean run. A read that
# fails fails the run before anything is mailed: a failing streak read without
# its recovery would mail a streak that is already over.
EVENTS=""
for type in order.failing order.recovered; do
    if ! out="$(gc events --type "$type" --since "$LOOKBACK")"; then
        echo "notify-order-failing: could not read the $type events of the last $LOOKBACK" >&2
        exit 1
    fi
    EVENTS="$EVENTS$out"$'\n'
done

# One compact line per event, in sequence order. The typed payload sits
# directly under .payload.
if ! ROWS="$(printf '%s\n' "$EVENTS" | jq -c -s '
    [ .[] | select(type == "object" and (.type == "order.failing" or .type == "order.recovered")) ]
    | unique_by(.seq) | sort_by(.seq) | .[]
    | { type: .type, message: (.message // ""),
        order: (.payload.order_name // .subject // ""),
        first_failed: (.payload.first_failed // ""),
        consecutive: (.payload.consecutive // 0),
        last_error: (.payload.last_error // "") }')"; then
    echo "notify-order-failing: could not decode the events gc events printed" >&2
    exit 1
fi
[ -n "$ROWS" ] || exit 0

# A streak that ended inside the window is reported by its recovery alone.
RECOVERED="$(printf '%s\n' "$ROWS" | jq -r 'select(.type == "order.recovered") | "\(.order) \(.first_failed)"')"

FAILED=0
while IFS= read -r row; do
    [ -n "$row" ] || continue
    type="$(printf '%s' "$row" | jq -r '.type')"
    order="$(printf '%s' "$row" | jq -r '.order')"
    first_failed="$(printf '%s' "$row" | jq -r '.first_failed')"
    if [ -z "$order" ] || [ -z "$first_failed" ]; then
        continue
    fi
    # A rig-scoped order is named <order>:rig:<rig>.
    case "${order%%:rig:*}" in
        notify-on-order-failing|notify-on-order-recovered) continue ;;
    esac
    key="order-failing:$order"
    case "$type" in
        order.failing)
            if printf '%s\n' "$RECOVERED" | grep -qxF -- "$order $first_failed"; then
                continue
            fi
            consecutive="$(printf '%s' "$row" | jq -r '.consecutive')"
            last_error="$(printf '%s' "$row" | jq -r '.last_error')"
            body="Order $order has failed $consecutive times in a row since $first_failed.

Last error: $last_error

See: gc events --type order.failed --since 24h
     gc doctor"
            if ! raise_incident "$key" "$first_failed" "Order $order keeps failing" "$body"; then
                echo "notify-order-failing: could not escalate the failing order $order (since $first_failed)" >&2
                FAILED=$((FAILED + 1))
            fi
            ;;
        order.recovered)
            if ! resolve_incident "$key"; then
                echo "notify-order-failing: could not resolve the failing incident of $order" >&2
                FAILED=$((FAILED + 1))
            fi
            ;;
    esac
done <<< "$ROWS"

if [ "$FAILED" -gt 0 ]; then
    exit 1
fi
