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
# Per-city socket isolation makes every session on this socket a GC
# session, so we install the popup directly without an if-shell guard.
mail_popup="display-popup -E -w 60 -h 15 'gc mail peek || echo No unread mail'"
existing=$(gcmux list-keys -T root MouseDown1StatusRight 2>/dev/null || true)
if ! printf '%s' "$existing" | grep -qF "$mail_popup"; then
    gcmux bind-key -T root MouseDown1StatusRight "$mail_popup"
fi

# ── Mouse-wheel scrollback (root table) ───────────────────────────────
# Make the wheel drive tmux copy-mode scrollback instead of leaking to the
# focused app. Without this, "mouse on" (set in tmux-theme.sh) hands the wheel
# to mouse-reporting TUIs — Claude Code scrolls its own history, a pager/shell
# gets Up-arrows — and only a bare prompt reaches copy-mode. Force copy-mode
# even over mouse-reporting apps (no mouse_any_flag check) so scrollback wins;
# once in copy-mode the wheel passes through (-M) for normal scrolling, and -e
# exits at the bottom. Shift+wheel still does native terminal selection.
gcmux bind-key -T root WheelUpPane   if-shell -F -t= "#{pane_in_mode}" "send-keys -M" "copy-mode -e"
gcmux bind-key -T root WheelDownPane send-keys -M
