#!/bin/sh
# bind-key.sh — install a tmux prefix keybinding directly.
# Usage: bind-key.sh <key> <command>
#
# Per-city tmux socket isolation (GC_TMUX_SOCKET, set by the controller
# in internal/runtime/tmux/adapter.go) makes every session on the socket
# a GC session. There is no non-GC fallback path to preserve, so the
# binding installs <command> directly without if-shell wrapping.
#
# tmux's bind-key naturally overwrites existing bindings; calling this
# script twice with the same args is a no-op at the tmux level. The
# early-exit on already-matching binding is an optimization to skip the
# tmux call.
set -e

key="$1"
command="$2"

[ -z "$key" ] || [ -z "$command" ] && exit 1

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

# Skip the bind-key call if the binding already contains the requested
# command. Fixed-string substring match is robust against tmux's quoting
# variations across versions.
existing=$(gcmux list-keys -T prefix "$key" 2>/dev/null || true)
if printf '%s' "$existing" | grep -qF "$command"; then
    exit 0
fi

gcmux bind-key -T prefix "$key" "$command"
