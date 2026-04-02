#!/bin/bash
# knowledge/cron-evolve.sh — Cron-safe wrapper for knowledge evolution
#
# Runs as a cron job every 6 hours. Handles locking, logging, and error recovery.
# This is the DETERMINISTIC path — it runs regardless of whether deacon is alive.
#
# Install: Add to crontab:
#   0 */6 * * * /home/pratham2/gt/mayor/knowledge/cron-evolve.sh >> /home/pratham2/gt/logs/knowledge-evolve.log 2>&1

set -euo pipefail

LOCKFILE="/tmp/knowledge-evolve.lock"
LOGFILE="/home/pratham2/gt/logs/knowledge-evolve.log"
KB_DIR="/home/pratham2/gt/mayor/knowledge"

# Ensure log dir exists
mkdir -p "$(dirname "$LOGFILE")"

# Flock to prevent concurrent runs
exec 200>"$LOCKFILE"
if ! flock -n 200; then
  echo "$(date): SKIP — another instance running"
  exit 0
fi

echo "$(date): Starting knowledge evolution"

# Run the evolution script
if bash "$KB_DIR/evolve.sh" 2>&1; then
  echo "$(date): Evolution completed successfully"
else
  echo "$(date): Evolution failed (exit $?), continuing"
fi

# Also scan for recent changelog-worthy events:
# Check if any beads were closed in the last 6h with substantial close reasons
DOLT_CMD="dolt --host 127.0.0.1 --port 3307 --user root --password '' --no-tls"
CL_SCRIPT="/home/pratham2/gt/mayor/changelog/append.sh"

for rig in officeworld deepwork_site villa_alc_ai villa_ai_planogram; do
  results=$($DOLT_CMD sql -q "
    SELECT id, title, close_reason
    FROM ${rig}.issues
    WHERE status='closed'
      AND closed_at > DATE_SUB(NOW(), INTERVAL 6 HOUR)
      AND close_reason IS NOT NULL
      AND close_reason != ''
      AND LENGTH(close_reason) > 30
    LIMIT 10
  " -r csv 2>/dev/null || echo "")

  [ -z "$results" ] && continue
  [ "$results" = "id,title,close_reason" ] && continue

  echo "$results" | tail -n +2 | while IFS=, read -r id title reason; do
    bash "$CL_SCRIPT" "fix" "$rig" "$title" "Closed: $reason" 2>/dev/null || true
  done
done

echo "$(date): Cron evolution complete"
