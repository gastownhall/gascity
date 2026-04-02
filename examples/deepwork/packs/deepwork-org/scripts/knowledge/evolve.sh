#!/bin/bash
# knowledge/evolve.sh — Periodic knowledge evolution
#
# Runs during deacon patrol (via plugin) or manually.
# Scans recently closed beads for lessons and appends to knowledge base.
#
# What it does:
# 1. Finds beads closed in the last 24h
# 2. Checks if they had multiple attempts (respawn count > 0) or incident labels
# 3. Extracts close_reason as a potential lesson
# 4. Appends non-trivial lessons to anti-patterns.md or patterns.md
#
# Requires: dolt CLI, access to Dolt on port 3307

set -euo pipefail

KB_DIR="$(dirname "$0")"
CAPTURE="$KB_DIR/capture.sh"
DOLT_CMD="dolt --host 127.0.0.1 --port 3307 --user root --password '' --no-tls"

# Query recently closed beads with close_reason across all rig DBs
RIGS="officeworld deepwork_site villa_alc_ai villa_ai_planogram gastown"

for rig in $RIGS; do
  prefix=$(echo "$rig" | head -c 3)

  # Get beads closed in last 24h that have a close_reason
  results=$($DOLT_CMD sql -q "
    SELECT id, title, close_reason
    FROM ${rig}.issues
    WHERE status='closed'
      AND closed_at > DATE_SUB(NOW(), INTERVAL 24 HOUR)
      AND close_reason IS NOT NULL
      AND close_reason != ''
      AND LENGTH(close_reason) > 50
    LIMIT 10
  " -r csv 2>/dev/null || echo "")

  if [ -z "$results" ] || [ "$results" = "id,title,close_reason" ]; then
    continue
  fi

  # Skip header, process each row
  echo "$results" | tail -n +2 | while IFS=, read -r id title reason; do
    # Skip if already captured
    if grep -qF "$id" "$KB_DIR/patterns.md" "$KB_DIR/anti-patterns.md" 2>/dev/null; then
      continue
    fi

    # Determine type: if close_reason mentions "bug", "fix", "broke", "incident" → anti-pattern
    if echo "$reason" | grep -qiE 'bug|broke|incident|crash|fail|wrong|mistake'; then
      bash "$CAPTURE" anti-pattern "$title" "$reason" "$prefix-$id"
    else
      bash "$CAPTURE" pattern "$title" "$reason" "$prefix-$id"
    fi
  done
done

echo "Knowledge evolution complete: $(date)"
