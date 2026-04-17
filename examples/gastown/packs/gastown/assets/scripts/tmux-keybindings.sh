#!/bin/sh
# tmux-keybindings.sh — Gas Town navigation keybindings (n/p/g/a + mail click)
# Usage: tmux-keybindings.sh <config-dir>
CONFIGDIR="$1"

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

# ── Navigation bindings (prefix table) ────────────────────────────────
"$CONFIGDIR"/assets/scripts/bind-key.sh n "run-shell '$CONFIGDIR/assets/scripts/cycle.sh next #{session_name} #{client_tty}'"
"$CONFIGDIR"/assets/scripts/bind-key.sh p "run-shell '$CONFIGDIR/assets/scripts/cycle.sh prev #{session_name} #{client_tty}'"
"$CONFIGDIR"/assets/scripts/bind-key.sh g "run-shell '$CONFIGDIR/assets/scripts/agent-menu.sh #{client_tty}'"

# ── Mail click binding (root table: left-click on status-right) ───────
# Shows unread mail preview in a popup when clicking the status-right area.
guard="tmux ${GC_TMUX_SOCKET:+-L $GC_TMUX_SOCKET} show-environment -t '#{session_name}' GC_AGENT >/dev/null 2>&1"
existing=$(gcmux list-keys -T root MouseDown1StatusRight 2>/dev/null || true)
if ! printf '%s' "$existing" | grep -q 'gc mail'; then
    fallback=""
    if [ -n "$existing" ]; then
        fallback=$(printf '%s' "$existing" | head -1 | awk '
        {
            i = 1; if ($i == "bind-key") i++; if ($i == "-r") i++
            if ($i == "-T") i += 3
            cmd = ""; for (; i <= NF; i++) cmd = cmd (cmd ? " " : "") $i
            print cmd
        }')
    fi
    [ -z "$fallback" ] && fallback=":"
    gcmux bind-key -T root MouseDown1StatusRight \
        if-shell "$guard" \
        "display-popup -E -w 60 -h 15 'gc mail peek || echo No unread mail'" \
        "$fallback"
fi
