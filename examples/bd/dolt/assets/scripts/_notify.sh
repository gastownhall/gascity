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

dolt_escalate() {
    local subject="$1"
    local message="$2"

    if [ -z "$DOLT_ESCALATE_SCRIPT" ] || [ ! -x "$DOLT_ESCALATE_SCRIPT" ]; then
        echo "dolt notify: no executable escalate.sh found" >&2
        return 1
    fi
    "$DOLT_ESCALATE_SCRIPT" --subject "$subject" --message "$message"
}

dolt_notify_done() {
    local summary="$1"
    local target="${GC_MAINTENANCE_DONE_TARGET:-}"

    [ -n "$target" ] || return 0
    gc session nudge "$target" "MAINTENANCE_DONE: $summary" 2>/dev/null || true
}
