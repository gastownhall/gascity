#!/usr/bin/env bash
# escalate — generic Core escalation hook for deterministic maintenance scripts.
#
# Packs can override escalation by shipping assets/scripts/escalate.sh and
# placing that pack earlier in GC_ESCALATE_SEARCH_PACKS.
set -euo pipefail

SUBJECT=""
MESSAGE=""
SEVERITY=""

while [ "$#" -gt 0 ]; do
    case "$1" in
        --subject)
            [ "$#" -ge 2 ] || { echo "escalate: --subject requires a value" >&2; exit 2; }
            SUBJECT="$2"
            shift 2
            ;;
        --message|-m)
            [ "$#" -ge 2 ] || { echo "escalate: --message requires a value" >&2; exit 2; }
            MESSAGE="$2"
            shift 2
            ;;
        --severity)
            [ "$#" -ge 2 ] || { echo "escalate: --severity requires a value" >&2; exit 2; }
            SEVERITY="$2"
            shift 2
            ;;
        --)
            shift
            break
            ;;
        *)
            echo "escalate: unknown argument $1" >&2
            exit 2
            ;;
    esac
done

if [ -z "$SUBJECT" ]; then
    echo "escalate: --subject is required" >&2
    exit 2
fi

if [ -n "$SEVERITY" ] && ! printf '%s' "$SUBJECT" | grep -Eq '\[[^]]+\]$'; then
    SUBJECT="$SUBJECT [$SEVERITY]"
fi

RECIPIENT="${GC_ESCALATION_RECIPIENT:-human}"

# Wake the recipient when it is an agent session. Without this the send writes
# a message bead and emits an event with no subscriber, so a paused agent finds
# the escalation only on a turn boundary it may never reach — which is how a
# maintenance advisory can fire on schedule for a day and reach nobody.
#
# `human` is the exception: it names an operator inbox with no session behind
# it, so there is nothing to wake and the flag would fail against it.
#
# The send is bounded because a wake can outlive the send it follows, and an
# escalation hanging here would stall the maintenance run that raised it. The
# mail is already written by the time the wake blocks, so a bound that trips
# costs the wake and not the message. Delivery is best-effort either way, so
# nothing that must survive belongs only in this mail.
NOTIFY_ARGS=""
if [ "$RECIPIENT" != "human" ]; then
    NOTIFY_ARGS="--notify"
fi

ESCALATE_SEND_TIMEOUT_SECS="${GC_ESCALATE_SEND_TIMEOUT_SECS:-30}"
case "$ESCALATE_SEND_TIMEOUT_SECS" in
    ''|*[!0-9]*) ESCALATE_SEND_TIMEOUT_SECS=30 ;;
    *[1-9]*) ;;
    *) ESCALATE_SEND_TIMEOUT_SECS=30 ;;
esac

if command -v timeout >/dev/null 2>&1; then
    # Capture the code on the same line: `set -e` is active, so a bare call
    # would abort the script before the 124 check below could run.
    send_rc=0
    # shellcheck disable=SC2086 # NOTIFY_ARGS is a controlled empty-or-one-flag string
    timeout "$ESCALATE_SEND_TIMEOUT_SECS" gc mail send "$RECIPIENT" $NOTIFY_ARGS -s "$SUBJECT" -m "$MESSAGE" || send_rc=$?
    # 124 means the wake outlived its bound after the mail was already written.
    # Reporting that as a failed escalation would be wrong, and would make a
    # caller retry a message that landed.
    if [ "$send_rc" -eq 124 ]; then
        echo "escalate: mail to $RECIPIENT sent; wake exceeded ${ESCALATE_SEND_TIMEOUT_SECS}s and was abandoned" >&2
        exit 0
    fi
    exit "$send_rc"
fi

# shellcheck disable=SC2086 # NOTIFY_ARGS is a controlled empty-or-one-flag string
gc mail send "$RECIPIENT" $NOTIFY_ARGS -s "$SUBJECT" -m "$MESSAGE"
