#!/usr/bin/env bash
# escalate — generic Core escalation hook for deterministic maintenance scripts.
#
# Usage:
#   escalate.sh --subject S [--message M] [--severity SEV]
#               [--incident-key KEY [--fingerprint F]]
#   escalate.sh --resolve-incident KEY
#
# With --incident-key, an incident whose fingerprint (default: the subject and
# message) is unchanged since its last delivered notification is not sent again
# until GC_ESCALATE_REMINDER_SECONDS (default 86400) have passed; the
# suppressed run says so on stdout and exits 0. --resolve-incident KEY forgets
# that incident and sends nothing, so its next occurrence notifies at once.
# Incident state lives in GC_ESCALATE_INCIDENT_DIR (default
# <city>/.gc/incidents) and is best-effort: when it cannot be read or written,
# the escalation is sent without dedupe.
#
# Packs can override escalation by shipping assets/scripts/escalate.sh and
# placing that pack earlier in GC_ESCALATE_SEARCH_PACKS. An override must accept
# or ignore --incident-key, --fingerprint, and --resolve-incident, and
# --resolve-incident must send nothing.
set -euo pipefail

SUBJECT=""
MESSAGE=""
SEVERITY=""
INCIDENT_KEY=""
FINGERPRINT=""
RESOLVE_KEY=""

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
        --incident-key)
            [ "$#" -ge 2 ] || { echo "escalate: --incident-key requires a value" >&2; exit 2; }
            INCIDENT_KEY="$2"
            shift 2
            ;;
        --fingerprint)
            [ "$#" -ge 2 ] || { echo "escalate: --fingerprint requires a value" >&2; exit 2; }
            FINGERPRINT="$2"
            shift 2
            ;;
        --resolve-incident)
            [ "$#" -ge 2 ] || { echo "escalate: --resolve-incident requires a value" >&2; exit 2; }
            RESOLVE_KEY="$2"
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

INCIDENT_DIR="${GC_ESCALATE_INCIDENT_DIR:-${GC_CITY_PATH:-${GC_CITY:-.}}/.gc/incidents}"

# incident_state_path prints the state file that records an incident key.
incident_state_path() {
    printf '%s/%s.state' "$INCIDENT_DIR" "$(printf '%s' "$1" | cksum | awk '{print $1}')"
}

if [ -n "$RESOLVE_KEY" ]; then
    RESOLVE_STATE=$(incident_state_path "$RESOLVE_KEY")
    if ! rm -f "$RESOLVE_STATE" 2>/dev/null; then
        echo "escalate: could not clear incident $RESOLVE_KEY at $RESOLVE_STATE; its next occurrence may wait for the reminder interval" >&2
        exit 1
    fi
    exit 0
fi

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
# escalation hanging here would stall the maintenance run that raised it.
# Delivery is best-effort either way, so nothing that must survive belongs only
# in this mail.
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
# GC_ESCALATE_NOW_EPOCH pins the clock for tests only. A deployment that sets
# it freezes "now", so an unchanged incident would never be reminded.
NOW_EPOCH="${GC_ESCALATE_NOW_EPOCH:-$(date -u '+%s')}"
REMINDER_SECONDS="${GC_ESCALATE_REMINDER_SECONDS:-86400}"
case "$NOW_EPOCH" in ''|*[!0-9]*) NOW_EPOCH=$(date -u '+%s') ;; esac
case "$REMINDER_SECONDS" in ''|*[!0-9]*) REMINDER_SECONDS=86400 ;; esac

# incident_time prints an epoch as a UTC timestamp (GNU date, then BSD date).
incident_time() {
    date -u -d "@$1" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null \
        || date -u -r "$1" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null \
        || printf '@%s' "$1"
}

# A repeat is suppressed only on a readable record of a delivery in the past
# with the same fingerprint; any I/O failure sends the escalation undeduped.
INCIDENT_STATE=""
INCIDENT_FINGERPRINT=""
if [ -n "$INCIDENT_KEY" ]; then
    INCIDENT_FINGERPRINT=$(printf '%s' "${FINGERPRINT:-$SUBJECT
$MESSAGE}" | cksum | awk '{print $1}')
    if mkdir -p "$INCIDENT_DIR" 2>/dev/null; then
        INCIDENT_STATE=$(incident_state_path "$INCIDENT_KEY")
    else
        echo "escalate: incident state unavailable ($INCIDENT_DIR); sending without dedupe" >&2
    fi
fi
if [ -n "$INCIDENT_STATE" ] && [ -e "$INCIDENT_STATE" ]; then
    if LAST_STATE=$(cat "$INCIDENT_STATE" 2>/dev/null); then
        LAST_FINGERPRINT=$(printf '%s\n' "$LAST_STATE" | sed -n 's/^fingerprint=//p')
        LAST_NOTIFIED=$(printf '%s\n' "$LAST_STATE" | sed -n 's/^notified=//p')
        case "$LAST_NOTIFIED" in ''|*[!0-9]*) LAST_NOTIFIED=0 ;; esac
        if [ "$LAST_NOTIFIED" -gt 0 ] \
            && [ "$INCIDENT_FINGERPRINT" = "$LAST_FINGERPRINT" ] \
            && [ "$NOW_EPOCH" -ge "$LAST_NOTIFIED" ] \
            && [ $((NOW_EPOCH - LAST_NOTIFIED)) -lt "$REMINDER_SECONDS" ]; then
            echo "escalate: incident $INCIDENT_KEY unchanged since $(incident_time "$LAST_NOTIFIED"); next reminder after $(incident_time $((LAST_NOTIFIED + REMINDER_SECONDS)))"
            exit 0
        fi
    else
        echo "escalate: incident state unreadable ($INCIDENT_STATE); sending without dedupe" >&2
    fi
fi

send_rc=0

if command -v timeout >/dev/null 2>&1; then
    # gc prints "Sent message <id> to <recipient>" once the mail is written
    # and before it wakes the recipient, so that line decides what a tripped
    # bound cost. Capture the code on the same line: `set -e` is active, so a
    # bare call would abort the script before the 124 check below could run.
    # shellcheck disable=SC2086 # NOTIFY_ARGS is a controlled empty-or-one-flag string
    SEND_OUTPUT=$(timeout "$ESCALATE_SEND_TIMEOUT_SECS" gc mail send "$RECIPIENT" $NOTIFY_ARGS -s "$SUBJECT" -m "$MESSAGE") || send_rc=$?
    if [ -n "$SEND_OUTPUT" ]; then
        printf '%s\n' "$SEND_OUTPUT"
    fi
    if [ "$send_rc" -eq 124 ]; then
        case "$SEND_OUTPUT" in
            *"Sent message "*)
                # Only what follows the write overran, the wake or gc's exit,
                # so the escalation landed. Reporting a failure would make a
                # caller retry a message that was delivered.
                echo "escalate: mail to $RECIPIENT sent; gc exceeded ${ESCALATE_SEND_TIMEOUT_SECS}s after sending it and was abandoned" >&2
                send_rc=0
                ;;
            *)
                # gc overran before reporting the write, for example while
                # opening the store, so the mail may not exist. It stays a
                # failure so the next run sends again.
                echo "escalate: mail send to $RECIPIENT exceeded ${ESCALATE_SEND_TIMEOUT_SECS}s before gc reported it sent and was abandoned" >&2
                ;;
        esac
    fi
else
    # shellcheck disable=SC2086 # NOTIFY_ARGS is a controlled empty-or-one-flag string
    gc mail send "$RECIPIENT" $NOTIFY_ARGS -s "$SUBJECT" -m "$MESSAGE" || send_rc=$?
fi

# Only a delivered escalation is recorded, so a failed send is retried by the
# next run instead of being suppressed for a reminder interval.
if [ "$send_rc" -eq 0 ] && [ -n "$INCIDENT_STATE" ]; then
    INCIDENT_TMP="$INCIDENT_STATE.tmp.$$"
    if ! { printf 'fingerprint=%s\nnotified=%s\nkey=%s\n' "$INCIDENT_FINGERPRINT" "$NOW_EPOCH" "$INCIDENT_KEY" >"$INCIDENT_TMP"; } 2>/dev/null \
        || ! mv -f "$INCIDENT_TMP" "$INCIDENT_STATE" 2>/dev/null; then
        rm -f "$INCIDENT_TMP" 2>/dev/null || true
        echo "escalate: could not record incident state at $INCIDENT_STATE; the next run will notify again" >&2
    fi
fi
exit "$send_rc"
