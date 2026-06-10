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
gc mail send "$RECIPIENT" -s "$SUBJECT" -m "$MESSAGE"
