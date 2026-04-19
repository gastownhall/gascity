#!/bin/sh
# cycle.sh — cycle between Gas Town agent sessions in the same group.
# Usage: cycle.sh next|prev <current-session> <client-tty>
# Called via tmux run-shell from a keybinding.
#
# This is Gas Town-specific. It knows the role names and grouping rules:
#   Town group:    mayor ↔ deacon
#   Dog pool:      dog-1 ↔ dog-2 ↔ dog-3
#   Rig ops:       {rig}--witness ↔ {rig}--refinery ↔ {rig}--polecat-*  (per rig)
#   Crew:          {rig}--{name} members  (per rig, excluding witness/refinery/polecat)
#
# Session name format (per-city socket isolation): {agent}
#   Town-scoped:  mayor, deacon, dog-1
#   Rig-scoped:   myrig--witness, myrig--polecat-1

direction="$1"
current="$2"
client="$3"

[ -z "$direction" ] || [ -z "$current" ] || [ -z "$client" ] && exit 0

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

# Determine the group filter pattern based on known Gas Town roles.
case "$current" in
    # Rig ops: witness ↔ refinery ↔ polecats in same rig.
    *--witness|*--refinery|*--polecat-*)
        rig="${current%%--*}"
        pattern="^${rig}--\(witness\|refinery\|polecat-\)"
        ;;
    # Other rig-scoped (crew, etc): cycle all same-rig non-infra agents.
    *--*)
        rig="${current%%--*}"
        pattern="^${rig}--"
        ;;
    # Town group: mayor ↔ deacon.
    mayor|deacon)
        pattern="^\(mayor\|deacon\)$"
        ;;
    # Dog pool: cycle between dog instances.
    dog-[0-9]*)
        pattern="^dog-[0-9]"
        ;;
    # Unknown — cycle all sessions on this socket (all are GC sessions).
    *)
        pattern="."
        ;;
esac

# Get target session: filter to same group, find current, pick next/prev.
target=$(gcmux list-sessions -F '#{session_name}' 2>/dev/null \
    | grep "$pattern" \
    | sort \
    | awk -v cur="$current" -v dir="$direction" '
        { a[NR] = $0; if ($0 == cur) idx = NR }
        END {
            if (NR <= 1 || idx == 0) exit
            if (dir == "next") t = (idx % NR) + 1
            else t = ((idx - 2 + NR) % NR) + 1
            print a[t]
        }')

[ -z "$target" ] && exit 0
gcmux switch-client -c "$client" -t "$target"
