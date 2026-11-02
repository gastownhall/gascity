#!/usr/bin/env bash
# Shared notification helpers for deterministic bd/dolt maintenance scripts.

dolt_notify_script_dir="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

dolt_resolve_escalate_script() {
    local candidate
    local pack
    local city_path="${GC_CITY_PATH:-${GC_CITY:-.}}"
    local system_packs="${GC_SYSTEM_PACKS_DIR:-$city_path/.gc/system/packs}"

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
    candidate="$dolt_notify_script_dir/../../../../../internal/bootstrap/packs/core/assets/scripts/escalate.sh"
    if [ -x "$candidate" ]; then
        printf '%s\n' "$candidate"
        return
    fi
    printf '%s\n' ""
}

DOLT_ESCALATE_SCRIPT="${DOLT_ESCALATE_SCRIPT:-$(dolt_resolve_escalate_script)}"

# dolt_escalate_refused_incident_flags RC STDERR is true when an escalate.sh
# override refused the incident flags as a usage error, as one written before
# them (the 1.5.0 contract) does: with exit 2, the usage-error status (the
# 1.5.0 escalate.sh, argparse, Go's flag package), or with a message that
# names the flag (getopt-style parsers exit 1). The core pack's escalate.sh
# (installed, or this checkout's) takes the flags, so no failure of it is read
# as a refusal.
dolt_escalate_refused_incident_flags() {
    local city_path="${GC_CITY_PATH:-${GC_CITY:-.}}"
    local core_scripts="${GC_SYSTEM_PACKS_DIR:-$city_path/.gc/system/packs}/core/assets/scripts"

    [ "$1" -ne 0 ] || return 1
    if [ "$DOLT_ESCALATE_SCRIPT" -ef "$core_scripts/escalate.sh" ] \
        || [ "$DOLT_ESCALATE_SCRIPT" -ef "$dolt_notify_script_dir/../../../../../internal/bootstrap/packs/core/assets/scripts/escalate.sh" ]; then
        return 1
    fi
    [ "$1" -ne 2 ] || return 0
    case "$2" in
        *incident-key*|*--fingerprint*|*resolve-incident*) return 0 ;;
    esac
    return 1
}

# dolt_escalate SUBJECT MESSAGE [INCIDENT_KEY FINGERPRINT] escalates MESSAGE,
# as incident INCIDENT_KEY when one is given. An override that refuses the
# incident flags is sent the alert again without them, so it keeps alerting,
# undeduplicated, rather than losing the alert.
dolt_escalate() {
    local subject="$1"
    local message="$2"
    local incident_key="${3:-}"
    local fingerprint="${4:-}"
    local rc=0
    local err

    if [ -z "$DOLT_ESCALATE_SCRIPT" ] || [ ! -x "$DOLT_ESCALATE_SCRIPT" ]; then
        echo "dolt notify: no executable escalate.sh found" >&2
        return 1
    fi
    if [ -z "$incident_key" ]; then
        "$DOLT_ESCALATE_SCRIPT" --subject "$subject" --message "$message"
        return
    fi
    # stdout passes through; stderr is held to tell a refusal from a failure.
    { err=$("$DOLT_ESCALATE_SCRIPT" --subject "$subject" --message "$message" \
        --incident-key "$incident_key" --fingerprint "$fingerprint" 2>&1 >&3 3>&-); } 3>&1 || rc=$?
    if ! dolt_escalate_refused_incident_flags "$rc" "$err"; then
        [ -z "$err" ] || printf '%s\n' "$err" >&2
        return "$rc"
    fi
    echo "dolt notify: $DOLT_ESCALATE_SCRIPT predates incident keys (exit $rc on --incident-key); sent the alert without them, so it is not deduplicated" >&2
    "$DOLT_ESCALATE_SCRIPT" --subject "$subject" --message "$message"
}

# dolt_resolve_incident tells escalate.sh that an incident cleared, so its next
# occurrence notifies at once instead of waiting out the reminder interval. An
# override that refuses the flag keeps no incidents, so there is nothing to
# clear.
dolt_resolve_incident() {
    local rc=0
    local err

    if [ -z "$DOLT_ESCALATE_SCRIPT" ] || [ ! -x "$DOLT_ESCALATE_SCRIPT" ]; then
        return 0
    fi
    err=$("$DOLT_ESCALATE_SCRIPT" --resolve-incident "$1" 2>&1 >/dev/null) || rc=$?
    if dolt_escalate_refused_incident_flags "$rc" "$err"; then
        echo "dolt notify: $DOLT_ESCALATE_SCRIPT predates incident keys (exit $rc on --resolve-incident); its alerts are not deduplicated" >&2
        return 0
    fi
    [ -z "$err" ] || printf '%s\n' "$err" >&2
    return "$rc"
}

dolt_notify_done() {
    local summary="$1"
    local target="${GC_MAINTENANCE_DONE_TARGET:-}"

    [ -n "$target" ] || return 0
    gc session nudge "$target" "MAINTENANCE_DONE: $summary" 2>/dev/null || true
}
