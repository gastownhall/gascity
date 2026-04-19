#!/bin/sh
# bind-key.sh — idempotent tmux keybinding with fallback preservation.
# Usage: bind-key.sh <key> <gc-command> [guard-pattern]
#
# If the key already has a GC binding (if-shell + gc), does nothing.
# Otherwise captures the existing binding as fallback, then installs
# an if-shell binding that runs <gc-command> in GC sessions and the
# original binding in non-GC sessions.
#
# With per-city socket isolation, all sessions on the socket are GC
# sessions. The guard checks for the GC_AGENT env var (set by the
# controller on every agent session) as a reliable indicator.
set -e

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

key="$1"
gc_command="$2"
guard_pattern="${3:-GC_AGENT}"

[ -z "$key" ] || [ -z "$gc_command" ] && exit 1

# Check if already a GC binding (idempotent).
existing=$(gcmux list-keys -T prefix "$key" 2>/dev/null || true)
if printf '%s' "$existing" | grep -q 'if-shell' && printf '%s' "$existing" | grep -q 'gc '; then
    exit 0
fi

# Parse existing binding command as fallback.
# tmux list-keys format: bind-key [-r] -T <table> <key> <command> [args...]
fallback=""
if [ -n "$existing" ]; then
    # Skip past "-T prefix <key>" to get the command portion.
    # Handle optional -r flag.
    fallback=$(printf '%s' "$existing" | head -1 | awk '
    {
        i = 1
        # skip "bind-key"
        if ($i == "bind-key") i++
        # skip optional -r
        if ($i == "-r") i++
        # skip -T <table> <key>
        if ($i == "-T") i += 3
        # rest is the command
        cmd = ""
        for (; i <= NF; i++) cmd = cmd (cmd ? " " : "") $i
        print cmd
    }')
fi

# Default fallbacks for common keys.
if [ -z "$fallback" ]; then
    case "$key" in
        n) fallback="next-window" ;;
        p) fallback="previous-window" ;;
        *) fallback="command-prompt" ;;
    esac
fi

# Install the if-shell binding.
# Guard checks for GC_AGENT env var in the session environment,
# which the controller sets on every agent session at startup.
guard="tmux ${GC_TMUX_SOCKET:+-L $GC_TMUX_SOCKET} show-environment -t '#{session_name}' ${guard_pattern} >/dev/null 2>&1"
gcmux bind-key -T prefix "$key" if-shell "$guard" "$gc_command" "$fallback"
