#!/bin/bash
# witness-watchdog.sh — Ensure witnesses are running for all active rigs
#
# Runs every 5 minutes via cron. For each rig that should have a witness:
# 1. Check if tmux session exists
# 2. Check if Claude process is alive inside it
# 3. If dead → restart via gt boot (or direct spawn)
#
# This is the DETERMINISTIC guarantee that witnesses are always up.
# The deacon spawns them initially; this watchdog catches crashes.
#
# Cron: */5 * * * * /home/pratham2/gt/mayor/scripts/witness-watchdog.sh

set -uo pipefail

GT_ROOT="${GT_ROOT:-$HOME/gt}"
LOGFILE="$GT_ROOT/logs/witness-watchdog.log"
LOCKFILE="/tmp/witness-watchdog.lock"

mkdir -p "$(dirname "$LOGFILE")"
log() { echo "$(date +%Y-%m-%dT%H:%M:%S) [witness-wd] $*" >> "$LOGFILE"; }

# Flock
exec 200>"$LOCKFILE"
flock -n 200 || { log "SKIP — another watchdog running"; exit 0; }

# Product rigs that MUST have witnesses
# Skip internal rigs (gastown, events) and empty/planned rigs (content_studio)
ACTIVE_RIGS=(
  officeworld
  deepwork_site
  villa_alc_ai
  villa_ai_planogram
  command_center
  products
  media_studio
)

restarted=0
alive=0

for rig in "${ACTIVE_RIGS[@]}"; do
  session="${rig//_/-}-witness"
  # Normalize: villa_ai_planogram → vap-witness
  case "$rig" in
    villa_ai_planogram) session="vap-witness" ;;
    villa_alc_ai)       session="vaa-witness" ;;
    deepwork_site)      session="ds-witness" ;;
    officeworld)        session="of-witness" ;;
    command_center)     session="cc-witness" ;;
    products)           session="prd-witness" ;;
    media_studio)       session="med-witness" ;;
  esac

  # Check if tmux session exists
  if ! tmux has-session -t "$session" 2>/dev/null; then
    log "DEAD: $session (no tmux session) — restarting"

    # Spawn witness via gt boot (preferred) or direct Claude spawn
    if timeout 30 gt boot "$rig" --witness-only 2>/dev/null; then
      log "  Restarted via gt boot"
      restarted=$((restarted + 1))
    else
      # Fallback: direct tmux + claude spawn
      witness_dir="$GT_ROOT/$rig/witness"
      if [ -d "$witness_dir" ]; then
        settings_file="$witness_dir/.claude/settings.json"
        settings_flag=""
        [ -f "$settings_file" ] && settings_flag="--settings $settings_file"

        tmux new-session -d -s "$session" -c "$witness_dir" \
          "claude --dangerously-skip-permissions $settings_flag '[GAS TOWN] witness (rig: $rig) <- watchdog • $(date +%Y-%m-%dT%H:%M) • patrol  Run \`gt prime --hook\` and begin patrol.'" 2>/dev/null \
          && { log "  Restarted via direct spawn"; restarted=$((restarted + 1)); } \
          || log "  ERROR: Failed to restart $session"
      else
        log "  ERROR: No witness directory at $witness_dir"
      fi
    fi
    continue
  fi

  # Session exists — check if Claude is alive inside
  pane_pid=$(tmux list-panes -t "$session" -F '#{pane_pid}' 2>/dev/null | head -1)
  if [ -z "$pane_pid" ]; then
    log "DEAD: $session (no pane PID) — killing and restarting"
    tmux kill-session -t "$session" 2>/dev/null
    # Next cron run will catch the missing session and restart
    continue
  fi

  # Check if there's a claude process under this pane
  if pgrep -P "$pane_pid" -f "claude" >/dev/null 2>&1; then
    alive=$((alive + 1))
  else
    # Pane exists but claude is dead — check if pane itself is dead
    pane_dead=$(tmux list-panes -t "$session" -F '#{pane_dead}' 2>/dev/null | head -1)
    if [ "$pane_dead" = "1" ]; then
      log "DEAD: $session (pane dead) — killing and restarting"
      tmux kill-session -t "$session" 2>/dev/null
      # Next run will restart
    else
      # Pane alive but no claude — maybe it exited cleanly
      log "WARN: $session pane alive but no claude process — will restart next run"
      tmux kill-session -t "$session" 2>/dev/null
    fi
  fi
done

if [ $restarted -gt 0 ]; then
  log "Summary: $alive alive, $restarted restarted"
else
  log "OK: $alive/$((alive + 0)) witnesses alive"
fi
