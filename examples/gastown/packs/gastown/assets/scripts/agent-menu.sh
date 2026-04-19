#!/bin/sh
# agent-menu.sh — tmux popup menu for switching between GC agent sessions.
# Usage: agent-menu.sh <client-tty>
# Called via tmux run-shell from a keybinding (typically prefix-g).
# Always exits 0 — tmux must never see errors from run-shell.
#
# With per-city socket isolation, all sessions on the socket are GC sessions.

client="$1"
[ -z "$client" ] && exit 0

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

# Collect all sessions (all are GC sessions on this socket).
sessions=$(gcmux list-sessions -F '#{session_name}' 2>/dev/null | sort)
[ -z "$sessions" ] && exit 0

# Build tmux display-menu arguments.
# Each session gets a numbered shortcut (1-9, then a-z).
set -- "display-menu" "-T" "#[fg=cyan,bold]Gas City Agents" "-x" "C" "-y" "C"

i=0
for s in $sessions; do
    # Shortcut key: 1-9, then a-z.
    if [ "$i" -lt 9 ]; then
        key=$((i + 1))
    elif [ "$i" -lt 35 ]; then
        key=$(printf "\\$(printf '%03o' $((i - 9 + 97)))")
    else
        key=""
    fi

    set -- "$@" "$s" "$key" "switch-client -c '$client' -t '$s'"
    i=$((i + 1))
done

gcmux "$@"
