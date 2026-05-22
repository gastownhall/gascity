#!/bin/sh
# cycle.sh — cycle between Gas City agent sessions in the same scope group.
# Usage: cycle.sh next|prev <current-session> <client-tty>
# Called via tmux run-shell from a keybinding.
#
# Grouping rules (driven by SDK session-name primitives — no role awareness):
#   Rig group:      "<rig>--*"     all agents in same rig cycle together.
#   Scope group:    "<scope>__*"   all agents in same scope cycle together.
#   Pool:           "<base>-N"     same-base pool members cycle (e.g. dog-1).
#   Catch-all:      anything else cycles all sessions on the socket.
#
# The "--" and "__" separators correspond to the SDK session-name mapping
# (slash → "--", dot → "__") in internal/agent/session_name.go. Keying on
# the separator rather than role names lets this script work for any pack,
# including custom packs whose role taxonomy differs from gastown's.

direction="$1"
current="$2"
client="$3"

[ -z "$direction" ] || [ -z "$current" ] || [ -z "$client" ] && exit 0

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

# Determine the group filter pattern based on session-name shape.
case "$current" in
    # Rig-scoped: any "<rig>--*" session.
    *--*)
        rig="${current%%--*}"
        pattern="^${rig}--"
        ;;
    # Scope-scoped: any "<scope>__*" session (city or imported scope).
    *__*)
        scope="${current%%__*}"
        pattern="^${scope}__"
        ;;
    # Pool: "<base>-N" naming (generic; covers dog-1, dog-2, ... and any
    # custom pool with the same shape).
    *-[0-9]*)
        base="${current%-*}"
        pattern="^${base}-[0-9][0-9]*$"
        ;;
    # Unknown shape — cycle all sessions on this socket.
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
