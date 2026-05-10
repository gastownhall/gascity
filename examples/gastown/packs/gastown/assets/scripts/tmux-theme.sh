#!/bin/sh
# tmux-theme.sh — Gas Town status bar theme with colors and icons.
# Usage: tmux-theme.sh <session> <agent> <config-dir>
#
# Theme tier is driven by SDK session-name primitives — no role awareness:
#
#   "<rig>--*"     -> rig tier   (witness, refinery, polecat, crew within a rig)
#   "<scope>__*"   -> scope tier (mayor, deacon — city-scoped roles)
#   "<base>-N"     -> pool tier  (dog-1, dog-2, generic pool members)
#   anything else  -> default tier
#
# The "--" and "__" separators correspond to the SDK session-name mapping
# (slash → "--", dot → "__") in internal/agent/session_name.go. Keying on
# the separator rather than role names lets this script work for any pack,
# including custom packs whose role taxonomy differs from gastown's. Same
# pattern as cycle.sh (#1571) and bind-key.sh (#1573).
SESSION="$1" AGENT="$2" CONFIGDIR="$3"

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

# Determine theme tier by session-name shape.
case "$SESSION" in
    *--*)       tier="rig" ;;
    *__*)       tier="scope" ;;
    *-[0-9]*)   tier="pool" ;;
    *)          tier="default" ;;
esac

# Tier color theme (bg/fg).
case "$tier" in
    rig)     bg="#1e3a5f" fg="#e0e0e0" ;;  # ocean
    scope)   bg="#2d1f3d" fg="#c0b0d0" ;;  # purple/silver
    pool)    bg="#3d2f1f" fg="#d0c0a0" ;;  # brown/tan
    *)       bg="#4a5568" fg="#e0e0e0" ;;  # slate
esac

# Tier icon.
case "$tier" in
    rig)     icon="⛏" ;;
    scope)   icon="🏛" ;;
    pool)    icon="🌊" ;;
    *)       icon="●" ;;
esac

# Apply theme.
gcmux set-option -t "$SESSION" status-position bottom
gcmux set-option -t "$SESSION" status-style "bg=$bg,fg=$fg"
gcmux set-option -t "$SESSION" status-left-length 25
gcmux set-option -t "$SESSION" status-left "$icon $AGENT "
gcmux set-option -t "$SESSION" status-right-length 80
gcmux set-option -t "$SESSION" status-interval 5
gcmux set-option -t "$SESSION" status-right "#($CONFIGDIR/assets/scripts/status-line.sh $AGENT) %H:%M"

# Mouse + clipboard.
gcmux set-option -t "$SESSION" mouse on
gcmux set-option -t "$SESSION" set-clipboard on
