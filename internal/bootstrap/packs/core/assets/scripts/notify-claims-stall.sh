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
# alert, the resumption) runs the order.
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

# Read both types. A read failure (API down, no events yet) is not this
# script's to report: the next event re-runs it.
EVENTS=""
for type in agent.claims_stalled agent.claims_resumed; do
    if out="$(gc events --type "$type" --since "$LOOKBACK" 2>/dev/null)"; then
        EVENTS="$EVENTS$out"$'\n'
    fi
done

# One compact line per event, in sequence order: seq, type, then the fields
# the mail needs. The typed payload sits directly under .payload.
ROWS="$(printf '%s\n' "$EVENTS" | jq -c -s '
    [ .[] | select(type == "object" and (.type == "agent.claims_stalled" or .type == "agent.claims_resumed")) ]
    | unique_by(.seq) | sort_by(.seq) | .[]
    | { seq: .seq, type: .type, message: (.message // ""),
        template: (.payload.template // .subject // ""),
        provider: (.payload.provider // ""),
        episode: (.payload.episode // "") }' 2>/dev/null)" || ROWS=""
[ -n "$ROWS" ] || exit 0

# An episode that ended inside the window is reported by its resumption
# alone: its stall was mailed by an earlier run, or never needs to be.
RESUMED="$(printf '%s\n' "$ROWS" | jq -r 'select(.type == "agent.claims_resumed") | .episode' 2>/dev/null)" || RESUMED=""

FAILED=0
while IFS= read -r row; do
    [ -n "$row" ] || continue
    type="$(printf '%s' "$row" | jq -r '.type')"
    template="$(printf '%s' "$row" | jq -r '.template')"
    provider="$(printf '%s' "$row" | jq -r '.provider')"
    episode="$(printf '%s' "$row" | jq -r '.episode')"
    message="$(printf '%s' "$row" | jq -r '.message')"
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
            if ! bash "$ESCALATE_SCRIPT" \
                --subject "Agent $template stopped claiming its work on $on" \
                --message "$body" \
                --severity HIGH \
                --incident-key "$key" \
                --fingerprint "$episode"; then
                echo "notify-claims-stall: could not escalate the stall of $template ($episode)" >&2
                FAILED=$((FAILED + 1))
            fi
            ;;
        agent.claims_resumed)
            # Resolve first: the stall's incident ends with its episode, so a
            # stall the guard raises later is mailed at once.
            bash "$ESCALATE_SCRIPT" --resolve-incident "$key" || true
            if ! bash "$ESCALATE_SCRIPT" \
                --subject "Agent $template claims its work again on $on" \
                --message "$message" \
                --incident-key "claims-resumed:$episode" \
                --fingerprint "$episode"; then
                echo "notify-claims-stall: could not report that $template resumed ($episode)" >&2
                FAILED=$((FAILED + 1))
            fi
            ;;
    esac
done <<< "$ROWS"

if [ "$FAILED" -gt 0 ]; then
    exit 1
fi
