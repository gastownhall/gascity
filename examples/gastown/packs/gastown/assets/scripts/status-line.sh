#!/bin/sh
# status-line.sh — tmux status-right helper for Gas City agents.
# Usage: status-line.sh <agent-name>
# Called by tmux every status-interval seconds via #(command).
# Always exits 0 — tmux must never see errors.

GC_BIN="${GC_BIN:-gc}"
agent="$1"
[ -z "$agent" ] && exit 0

# Count pending work items (non-empty lines from gc hook).
w=$("$GC_BIN" hook "$agent" 2>/dev/null | grep -c . || true)

# Count unread mail (first word of gc mail check output is the count).
m=$("$GC_BIN" mail check "$agent" 2>/dev/null | awk '{print $1+0}' || true)

# Format: agent | hook-icon N | mail-icon N  (omit segments that are 0)
printf '%s' "$agent"
[ "${w:-0}" -gt 0 ] && printf ' | 🪝 %d' "$w"
[ "${m:-0}" -gt 0 ] && printf ' | 📬 %d' "$m"
