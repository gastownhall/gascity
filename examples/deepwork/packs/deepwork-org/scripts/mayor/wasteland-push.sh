#!/bin/bash
# wasteland-push.sh — Push wasteland changes to DoltHub
#
# Runs every 2 hours via cron. Simple, deterministic, no LLM needed.
# Just commits any pending changes and pushes to DoltHub so friends see updates.
#
# Cron: 0 */2 * * * /home/pratham2/gt/mayor/scripts/wasteland-push.sh

set -uo pipefail

LOGFILE="/home/pratham2/gt/logs/wasteland-push.log"
LOCKFILE="/tmp/wasteland-push.lock"
dolt_sql() { dolt --host 127.0.0.1 --port 3307 --user root --password "" --no-tls sql -q "$1" 2>&1; }

mkdir -p "$(dirname "$LOGFILE")"
log() { echo "$(date +%Y-%m-%dT%H:%M:%S) $*" >> "$LOGFILE"; }

exec 200>"$LOCKFILE"
flock -n 200 || { log "SKIP — another push running"; exit 0; }

# Commit any uncommitted wasteland changes
dolt_sql "USE gt_collab; CALL dolt_add('-A')" >/dev/null
dolt_sql "USE gt_collab; CALL dolt_commit('-m', 'auto: wasteland sync $(date +%Y-%m-%dT%H:%M)', '--allow-empty')" >/dev/null

# Push to DoltHub
result=$(dolt_sql "USE gt_collab; CALL dolt_push('origin', 'main')")
if echo "$result" | grep -qE "up-to-date|new branch|->"; then
  log "OK: $(echo "$result" | grep -oE 'up-to-date|main -> main')"
else
  log "ERROR: $result"
fi
