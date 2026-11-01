#!/usr/bin/env bash
# notify-claims-stall — tell a person when an agent template's seats stop
# claiming their work, and when they claim it again.
#
# The controller's claims-stall guard (cmd/gc/claim_stall_guard.go) records
# agent.claims_stalled when several seats of one template in a row fail to
# claim the routed work they were spawned for (a provider out of usage, an
# expired account, a CLI that never reads its prompt), and
# agent.claims_resumed when a seat claims again. On 2026-10-31 such a stall
# ran ten hours with nobody told. The events alone reach only the event log,
# so this script, run by the notify-on-claims-stalled and
# notify-on-claims-resumed event orders, mails them through escalate.sh to
# $GC_ESCALATION_RECIPIENT (default "human", the operator inbox the dashboard
# shows):
#
#   - a stall mails once per episode: the incident key is the episode the
#     guard names, and its fingerprint the episode too, so the probe alerts
#     the guard raises while the stall lasts are not mailed again (escalate.sh
#     reminds once a day while it stands);
#   - a resumption resolves that incident and mails a short note, once.
#
# Both orders run this one script, which reads both event types from a
# lookback window that must exceed the controller's event-trigger cadence;
# re-reading an event is harmless, its incident is already recorded.
#
# Loud-fail: a send that fails exits non-zero, which is what makes the
# controller log the order's output. It is sent again by the next run that
# still finds the event inside the lookback: the next matching event (a probe
# alert, the resumption) runs the order. A resumption whose incident does not
# clear still sends its note, then fails the run the same way. An event read
# that fails, or output that does not decode, fails the run and mails
# nothing: a resumption may raise no later event to run the order again.
#
# An escalate.sh override written before incident keys refuses their flags;
# it is sent each alert again without them, so it still alerts, on every run
# that finds the event rather than once per episode.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
LOOKBACK="${GC_NOTIFY_CLAIMS_STALL_LOOKBACK:-15m}"

if ! command -v jq >/dev/null 2>&1; then
    echo "notify-claims-stall: jq is required but not found in PATH" >&2
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
# refused the incident flags as a usage error, as one written before them (the
# 1.5.0 contract) does: with exit 2, the usage-error status (the 1.5.0
# escalate.sh, argparse, Go's flag package), or with a message that names the
# flag (getopt-style parsers exit 1). The core escalate.sh next to this script
# takes the flags, so no failure of it is read as a refusal.
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

# raise_incident KEY FINGERPRINT SUBJECT MESSAGE [SEVERITY] escalates MESSAGE
# as incident KEY, which escalate.sh keeps quiet while FINGERPRINT is
# unchanged. An override that refuses the incident flags is sent the alert
# again without them, so it keeps alerting, undeduplicated, rather than losing
# the alert.
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
    echo "notify-claims-stall: $ESCALATE_SCRIPT predates incident keys (exit $rc on --incident-key); sent the alert without them, so it is not deduplicated" >&2
    "$ESCALATE_SCRIPT" "${alert[@]}"
}

# resolve_incident KEY tells escalate.sh that incident KEY cleared. An override
# that refuses the flag keeps no incidents, so there is nothing to clear.
resolve_incident() {
    local rc=0 err
    err=$("$ESCALATE_SCRIPT" --resolve-incident "$1" 2>&1 >/dev/null) || rc=$?
    if escalate_refused_incident_flags "$rc" "$err"; then
        echo "notify-claims-stall: $ESCALATE_SCRIPT predates incident keys (exit $rc on --resolve-incident); its alerts are not deduplicated" >&2
        return 0
    fi
    [ -z "$err" ] || printf '%s\n' "$err" >&2
    return "$rc"
}

# Read both types; a window that holds neither is a clean run. A read that
# fails passes its own error through and fails the run before anything is
# mailed: a stall read without its resumption would mail an episode that is
# already over.
EVENTS=""
for type in agent.claims_stalled agent.claims_resumed; do
    if ! out="$(gc events --type "$type" --since "$LOOKBACK")"; then
        echo "notify-claims-stall: could not read the $type events of the last $LOOKBACK" >&2
        exit 1
    fi
    EVENTS="$EVENTS$out"$'\n'
done

# One compact line per event, in sequence order: seq, type, then the fields
# the mail needs. The typed payload sits directly under .payload.
if ! ROWS="$(printf '%s\n' "$EVENTS" | jq -c -s '
    [ .[] | select(type == "object" and (.type == "agent.claims_stalled" or .type == "agent.claims_resumed")) ]
    | unique_by(.seq) | sort_by(.seq) | .[]
    | { seq: .seq, type: .type, message: (.message // ""),
        template: (.payload.template // .subject // ""),
        provider: (.payload.provider // ""),
        episode: (.payload.episode // ""),
        cause: (.payload.cause // "") }')"; then
    echo "notify-claims-stall: could not decode the events gc events printed" >&2
    exit 1
fi
[ -n "$ROWS" ] || exit 0

# An episode that ended inside the window is reported by its resumption
# alone: its stall was mailed by an earlier run, or never needs to be.
RESUMED="$(printf '%s\n' "$ROWS" | jq -r 'select(.type == "agent.claims_resumed") | .episode')"

FAILED=0
while IFS= read -r row; do
    [ -n "$row" ] || continue
    type="$(printf '%s' "$row" | jq -r '.type')"
    template="$(printf '%s' "$row" | jq -r '.template')"
    provider="$(printf '%s' "$row" | jq -r '.provider')"
    episode="$(printf '%s' "$row" | jq -r '.episode')"
    message="$(printf '%s' "$row" | jq -r '.message')"
    cause="$(printf '%s' "$row" | jq -r '.cause')"
    if [ -z "$template" ] || [ -z "$episode" ]; then
        continue
    fi
    on="${provider:-its configured command}"
    key="claims-stalled:$episode"
    case "$type" in
        agent.claims_stalled)
            if printf '%s\n' "$RESUMED" | grep -qxF -- "$episode"; then
                continue
            fi
            body="$message

Episode: $episode
See: gc events --type agent.claims_stalled --since 24h
     gc session list --template $template"
            if ! raise_incident "$key" "$episode" \
                "Agent $template stopped claiming its work on $on" "$body" HIGH; then
                echo "notify-claims-stall: could not escalate the stall of $template ($episode)" >&2
                FAILED=$((FAILED + 1))
            fi
            ;;
        agent.claims_resumed)
            # The episode is over. Resolving its incident drops escalate.sh's
            # record of it, which nothing else removes.
            if ! resolve_incident "$key"; then
                echo "notify-claims-stall: could not resolve the stall incident of $template ($episode)" >&2
                FAILED=$((FAILED + 1))
            fi
            subject="Agent $template claims its work again on $on"
            if [ "$cause" = config_changed ]; then
                subject="Agent $template was moved off $on; its claims stall is over"
            fi
            if ! raise_incident "claims-resumed:$episode" "$episode" "$subject" "$message"; then
                echo "notify-claims-stall: could not report that $template resumed ($episode)" >&2
                FAILED=$((FAILED + 1))
            fi
            ;;
    esac
done <<< "$ROWS"

if [ "$FAILED" -gt 0 ]; then
    exit 1
fi
