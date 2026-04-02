#!/bin/bash
# knowledge/health-check.sh — Verify the knowledge system is operational
#
# Run this to confirm all automation paths are working.
# Returns non-zero if any path is broken.

set -euo pipefail

ERRORS=0
KB_DIR="$(cd "$(dirname "$0")" && pwd)"
GT_ROOT="/home/pratham2/gt"

echo "=== Knowledge System Health Check ==="
echo ""

# 1. Files exist
echo -n "1. Knowledge files exist: "
for f in patterns.md anti-patterns.md decisions.md operations.md products.md; do
  if [ ! -f "$KB_DIR/$f" ]; then
    echo "FAIL — missing $f"
    ERRORS=$((ERRORS+1))
    continue
  fi
done
echo "OK"

# 2. Scripts are executable
echo -n "2. Scripts executable: "
for s in capture.sh evolve.sh cron-evolve.sh on-bead-close.sh health-check.sh; do
  if [ ! -x "$KB_DIR/$s" ]; then
    echo "FAIL — $s not executable"
    ERRORS=$((ERRORS+1))
    continue
  fi
done
echo "OK"

# 3. Changelog exists
echo -n "3. Changelog directory: "
if [ -d "$GT_ROOT/mayor/changelog" ] && [ -x "$GT_ROOT/mayor/changelog/append.sh" ]; then
  echo "OK"
else
  echo "FAIL"
  ERRORS=$((ERRORS+1))
fi

# 4. Cron job installed
echo -n "4. Cron job installed: "
if crontab -l 2>/dev/null | grep -q "cron-evolve.sh"; then
  echo "OK"
else
  echo "FAIL — cron not found in crontab"
  ERRORS=$((ERRORS+1))
fi

# 5. Cron has run recently (within last 12h)
echo -n "5. Cron ran recently: "
LOG="$GT_ROOT/logs/knowledge-evolve.log"
if [ -f "$LOG" ]; then
  LAST_RUN=$(stat -c %Y "$LOG" 2>/dev/null || echo 0)
  NOW=$(date +%s)
  AGE=$(( (NOW - LAST_RUN) / 3600 ))
  if [ $AGE -lt 12 ]; then
    echo "OK (${AGE}h ago)"
  else
    echo "WARN — last run ${AGE}h ago (expected <12h)"
  fi
else
  echo "WARN — no log yet (cron may not have run yet)"
fi

# 6. Plugin registered
echo -n "6. Plugin exists: "
if [ -f "$GT_ROOT/plugins/knowledge-evolve/plugin.md" ]; then
  echo "OK"
else
  echo "FAIL — plugin not found"
  ERRORS=$((ERRORS+1))
fi

# 7. Dolt accessible (needed for evolve.sh)
echo -n "7. Dolt accessible: "
if timeout 5 dolt --host 127.0.0.1 --port 3307 --user root --password "" --no-tls sql -q "SELECT 1" >/dev/null 2>&1; then
  echo "OK"
else
  echo "FAIL — Dolt unreachable"
  ERRORS=$((ERRORS+1))
fi

# 8. AGENTS.md has knowledge instructions
echo -n "8. AGENTS.md has knowledge section: "
if grep -q "Town Knowledge System" "$GT_ROOT/AGENTS.md" 2>/dev/null; then
  echo "OK"
else
  echo "FAIL — agents don't know about knowledge system"
  ERRORS=$((ERRORS+1))
fi

# 9. graceful-handoff.sh has changelog integration
echo -n "9. Handoff logs to changelog: "
if grep -q "changelog/append.sh" "$GT_ROOT/mayor/graceful-handoff.sh" 2>/dev/null; then
  echo "OK"
else
  echo "FAIL — handoff doesn't write changelog"
  ERRORS=$((ERRORS+1))
fi

echo ""
if [ $ERRORS -eq 0 ]; then
  echo "ALL CHECKS PASSED"
else
  echo "FAILED: $ERRORS check(s)"
fi

exit $ERRORS
