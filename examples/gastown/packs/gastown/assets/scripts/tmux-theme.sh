#!/bin/sh
# tmux-theme.sh — Gas Town status bar theme with colors and icons.
# Usage: tmux-theme.sh <session> <agent> <config-dir>
#
# Applies role-based color theme and status bar formatting.
# Role is extracted from the agent name (bare name or rig--name).
SESSION="$1" AGENT="$2" CONFIGDIR="$3"

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

# ── Determine role from agent name ──────────────────────────────────────
# Rig-scoped: "myrig--polecat-1" → role "polecat"
# City-scoped: "mayor" → role "mayor", "dog-2" → role "dog"
case "$AGENT" in
    */polecat-*|*--polecat-*) role="polecat" ;;
    */polecat|*--polecat)     role="polecat" ;;
    */witness|*--witness)     role="witness" ;;
    */refinery|*--refinery)   role="refinery" ;;
    */crew/*|*--crew--*)      role="crew" ;;
    mayor)                    role="mayor" ;;
    deacon)                   role="deacon" ;;
    boot)                     role="boot" ;;
    dog-[0-9]*|dog)           role="dog" ;;
    *)                        role="" ;;
esac

# ── Color theme (bg/fg) per role ────────────────────────────────────────
# Matches the Go palette in internal/session/tmux/theme.go.
case "$role" in
    mayor)   bg="#3d3200" fg="#ffd700" ;;  # gold/dark
    deacon)  bg="#2d1f3d" fg="#c0b0d0" ;;  # purple/silver
    dog)     bg="#3d2f1f" fg="#d0c0a0" ;;  # brown/tan
    witness) bg="#0d5c63" fg="#e0e0e0" ;;  # teal
    refinery) bg="#4a5568" fg="#e0e0e0" ;; # slate
    polecat) bg="#1e3a5f" fg="#e0e0e0" ;;  # ocean
    crew)    bg="#2d5a3d" fg="#e0e0e0" ;;  # forest
    boot)    bg="#1a1a2e" fg="#c0c0c0" ;;  # midnight
    *)       bg="#4a5568" fg="#e0e0e0" ;;  # slate (default)
esac

# ── Role icon ───────────────────────────────────────────────────────────
case "$role" in
    mayor)   icon="🎩" ;;
    deacon)  icon="🐺" ;;
    witness) icon="🦉" ;;
    refinery) icon="🏭" ;;
    crew)    icon="👷" ;;
    polecat) icon="😺" ;;
    dog)     icon="🐕" ;;
    boot)    icon="⚡" ;;
    *)       icon="●" ;;
esac

# ── Apply theme ─────────────────────────────────────────────────────────
gcmux set-option -t "$SESSION" status-position bottom
gcmux set-option -t "$SESSION" status-style "bg=$bg,fg=$fg"
gcmux set-option -t "$SESSION" status-left-length 25
gcmux set-option -t "$SESSION" status-left "$icon $AGENT "
gcmux set-option -t "$SESSION" status-right-length 80
gcmux set-option -t "$SESSION" status-interval 5
gcmux set-option -t "$SESSION" status-right "#($CONFIGDIR/assets/scripts/status-line.sh $AGENT) %H:%M"

# ── Mouse + clipboard ─────────────────────────────────────────────────
gcmux set-option -t "$SESSION" mouse on
gcmux set-option -t "$SESSION" set-clipboard on
