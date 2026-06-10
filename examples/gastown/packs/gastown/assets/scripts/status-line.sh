#!/bin/sh
# status-line.sh — tmux status-right helper for Gas City agents.
# Usage: status-line.sh <agent-name>
# Called by tmux every status-interval seconds via #(command).
# Always exits 0 — tmux must never see errors.

agent="$1"
[ -z "$agent" ] && exit 0

# Trace gc hook / gc mail check invocations to $GC_BD_TRACE when set.
# The helper lives in the core pack scripts dir; if it's not
# reachable, status-line continues without tracing.
__bd_trace_helper=""
for __cand in \
    "${GC_CITY_PATH:-}/.gc/system/packs/core/assets/scripts/_bd_trace.sh" \
    "${GC_CITY:-}/.gc/system/packs/core/assets/scripts/_bd_trace.sh"; do
    if [ -n "$__cand" ] && [ -f "$__cand" ]; then
        __bd_trace_helper="$__cand"
        break
    fi
done
if [ -n "$__bd_trace_helper" ]; then
    # shellcheck disable=SC1090
    . "$__bd_trace_helper" "status-line:$agent"
fi

# Count pending work items (non-empty lines from gc hook).
w=$(gc hook "$agent" 2>/dev/null | grep -c . || true)

# Count unread mail via the count-only endpoint (cheaper than mail check).
m=$(gc mail count "$agent" --json 2>/dev/null | jq -r '.unread // 0' 2>/dev/null || echo 0)

# Format: agent | hook-icon N | mail-icon N  (omit segments that are 0)
printf '%s' "$agent"
[ "${w:-0}" -gt 0 ] && printf ' | 🪝 %d' "$w"
[ "${m:-0}" -gt 0 ] && printf ' | 📬 %d' "$m"
