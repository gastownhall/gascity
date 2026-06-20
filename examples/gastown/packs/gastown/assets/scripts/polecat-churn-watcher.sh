#!/usr/bin/env bash
# polecat-churn-watcher.sh — observability for pool-reconciler polecat kills.
#
# What it watches:
#   - $GC_CITY/.gc/nudges/pollers/polecat-*.pid   (live polecat sessions)
#   - rig bd: open beads with metadata.work_dir set OR
#             metadata.polecat_session set                (claimed work)
#
# What it logs:
#   When a polecat PID file disappears AND a bead it had claimed is back
#   in OPEN/no-assignee state — that's churn. The pool reconciler killed
#   a polecat mid-claim and silently recycled.
#
# Output: appends one JSON line per detected churn event to LOG_FILE.
#
# Cron-friendly: idempotent, fast (sub-second), reads only.
#
# Env / args:
#   GC_CITY     city root (auto-discovered if unset, walks up from cwd)
#   GC_RIG      rig name (required — used to scope `gc bd list`)
#   LOG_DIR     where to write the log (default: $GC_CITY/.gc/runtime/logs)
#
# Usage:
#   GC_RIG=helm polecat-churn-watcher.sh
#   GC_CITY=/path/to/city GC_RIG=foo polecat-churn-watcher.sh

set -euo pipefail

# Resolve city root: env wins, else walk up from cwd looking for city.toml.
if [ -z "${GC_CITY:-}" ]; then
  dir=$(pwd)
  while [ "$dir" != "/" ]; do
    if [ -f "$dir/city.toml" ]; then
      GC_CITY="$dir"
      break
    fi
    dir=$(dirname "$dir")
  done
fi

if [ -z "${GC_CITY:-}" ] || [ ! -f "$GC_CITY/city.toml" ]; then
  echo "polecat-churn-watcher: GC_CITY not set and no city.toml found" >&2
  exit 2
fi

if [ -z "${GC_RIG:-}" ]; then
  echo "polecat-churn-watcher: GC_RIG must be set (rig name to scope bd list)" >&2
  exit 2
fi

POLLERS_DIR="$GC_CITY/.gc/nudges/pollers"
LOG_DIR="${LOG_DIR:-$GC_CITY/.gc/runtime/logs}"
LOG_FILE="$LOG_DIR/polecat-churn.log"
STATE_FILE="$LOG_DIR/polecat-churn.state"  # last-seen PID set

mkdir -p "$LOG_DIR"
touch "$STATE_FILE"

# Snapshot current live polecat sessions (PID files without .lock suffix).
current_pids=$(ls "$POLLERS_DIR" 2>/dev/null \
  | grep -E '^polecat-[a-z0-9-]+\.pid$' \
  | sed -E 's/^polecat-(.+)\.pid$/\1/' \
  | sort -u)

# Last-seen set from previous run.
prev_pids=$(cat "$STATE_FILE" 2>/dev/null | sort -u)

# Disappeared since last tick.
disappeared=$(comm -23 <(printf "%s\n" "$prev_pids") <(printf "%s\n" "$current_pids") || true)

# Update state file for next tick.
printf "%s\n" "$current_pids" > "$STATE_FILE"

[ -z "$disappeared" ] && exit 0

# For each disappeared polecat session, check rig bd for orphan-claim.
# We look at OPEN beads whose metadata.work_dir mentions the dead session,
# OR whose metadata.polecat_session equals it (if the field is set).
ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

open_beads_json=$(gc --rig "$GC_RIG" bd list --status=open --json 2>/dev/null || echo "[]")

for dead in $disappeared; do
  orphans=$(printf '%s' "$open_beads_json" \
    | jq -r --arg s "$dead" '
        .[] | select(
          (.metadata.work_dir // "" | contains($s)) or
          (.metadata.polecat_session // "" == $s)
        ) | .id' 2>/dev/null || true)

  if [ -n "$orphans" ]; then
    for bead in $orphans; do
      printf '{"ts":"%s","event":"polecat_killed_mid_claim","rig":"%s","session":"%s","bead":"%s"}\n' \
        "$ts" "$GC_RIG" "$dead" "$bead" >> "$LOG_FILE"
    done
  else
    # Polecat died but no orphan claim — clean exit, just record for trend.
    printf '{"ts":"%s","event":"polecat_disappeared_clean","rig":"%s","session":"%s"}\n' \
      "$ts" "$GC_RIG" "$dead" >> "$LOG_FILE"
  fi
done
