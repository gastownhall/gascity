#!/usr/bin/env bash
# mol-dog-doctor — probe Dolt server health and report findings.
#
# Replaces mol-dog-doctor formula. All checks are read-only: SQL probe,
# PROCESSLIST count, disk usage, orphan DB detection, backup freshness.
# No LLM judgment needed — runs inline in the controller.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

PORT="${GC_DOLT_PORT:-3307}"
HOST="${GC_DOLT_HOST:-127.0.0.1}"
USER="${GC_DOLT_USER:-root}"
LATENCY_WARN_S="${GC_DOCTOR_LATENCY_WARN_S:-1}"
CONN_MAX="${GC_DOCTOR_CONN_MAX:-50}"
CONN_WARN_PCT="${GC_DOCTOR_CONN_WARN_PCT:-80}"
BACKUP_STALE_S="${GC_DOCTOR_BACKUP_STALE_S:-43200}"  # 2x 6h backup interval

dolt_sql() {
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" \
        run_bounded 10 \
        dolt --host "$HOST" --port "$PORT" --user "$USER" --no-tls sql "$@"
}

# --- Step 1: Probe connectivity and measure latency ---

PROBE_START=$(date +%s)
if ! dolt_sql -q "SELECT active_branch()" >/dev/null 2>&1; then
    gc mail send mayor/ \
        -s "ESCALATION: Dolt server unreachable on port $PORT [CRITICAL]" \
        -m "Doctor probe failed: server did not respond to active_branch() query." \
        2>/dev/null || true
    gc nudge deacon/ "DOG_DONE: doctor — server: UNREACHABLE (escalated)" 2>/dev/null || true
    echo "doctor: server unreachable on port $PORT (escalated)"
    exit 0
fi
PROBE_END=$(date +%s)
LATENCY_S=$((PROBE_END - PROBE_START))
LATENCY_WARN=""
if [ "$LATENCY_S" -ge "$LATENCY_WARN_S" ]; then
    LATENCY_WARN=" [WARN: latency ${LATENCY_S}s >= threshold ${LATENCY_WARN_S}s]"
fi

# --- Step 2: Check resource conditions ---

CONN_COUNT=$(dolt_sql -r csv -q "SELECT COUNT(*) FROM information_schema.PROCESSLIST" 2>/dev/null \
    | tail -1 || echo "0")
CONN_WARN=""
CONN_WARN_AT=$(( (CONN_MAX * CONN_WARN_PCT) / 100 ))
if [ "${CONN_COUNT:-0}" -ge "$CONN_WARN_AT" ]; then
    CONN_WARN=" [WARN: ${CONN_COUNT} connections >= ${CONN_WARN_PCT}% of max ${CONN_MAX}]"
fi

# Disk usage of Dolt data directory.
DISK_USAGE=$(du -sh "$DOLT_DATA_DIR" 2>/dev/null | cut -f1 || echo "unknown")

# Orphan database detection.
ALL_DBS=$(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2 || true)
ORPHAN_PATTERNS="^testdb_\|^beads_t\|^beads_pt\|^beads_vr\|^doctest_\|^doctortest_"
SYSTEM_DBS="^information_schema$\|^mysql$\|^dolt_cluster$\|^__gc_probe$"
ORPHANS=$(echo "$ALL_DBS" | grep -i "$ORPHAN_PATTERNS" | grep -vi "$SYSTEM_DBS" || true)
ORPHAN_COUNT=$(echo "$ORPHANS" | grep -c . || echo "0")
ORPHAN_WARN=""
if [ "${ORPHAN_COUNT:-0}" -gt 0 ]; then
    ORPHAN_WARN=" [WARN: $ORPHAN_COUNT orphan DBs detected — run gc dolt cleanup]"
fi

# Backup freshness: check mtime of most-recently-modified backup file.
BACKUP_STALE=""
if [ -d "$DOLT_DATA_DIR" ]; then
    NEWEST_BACKUP=$(find "$DOLT_DATA_DIR" -name "*.bak" -o -name "*.backup" 2>/dev/null \
        | xargs ls -t 2>/dev/null | head -1 || true)
    if [ -n "$NEWEST_BACKUP" ]; then
        BACKUP_MTIME=$(stat -c %Y "$NEWEST_BACKUP" 2>/dev/null \
            || stat -f %m "$NEWEST_BACKUP" 2>/dev/null || echo "0")
        NOW_S=$(date +%s)
        BACKUP_AGE=$((NOW_S - BACKUP_MTIME))
        if [ "$BACKUP_AGE" -gt "$BACKUP_STALE_S" ]; then
            BACKUP_STALE=" [WARN: newest backup is $((BACKUP_AGE / 3600))h old]"
        fi
    fi
fi

# --- Step 3: Compose report and escalate if critical ---

WARNINGS="${LATENCY_WARN}${CONN_WARN}${ORPHAN_WARN}${BACKUP_STALE}"
if [ -n "$WARNINGS" ]; then
    gc mail send mayor/ \
        -s "Dolt health advisory [MEDIUM]" \
        -m "Latency: ${LATENCY_S}s${LATENCY_WARN}
Connections: ${CONN_COUNT}/${CONN_MAX}${CONN_WARN}
Disk: ${DISK_USAGE}
Orphan DBs: ${ORPHAN_COUNT}${ORPHAN_WARN}${BACKUP_STALE}" \
        2>/dev/null || true
fi

SUMMARY="doctor — server: ok, latency: ${LATENCY_S}s, conns: ${CONN_COUNT}/${CONN_MAX}, disk: ${DISK_USAGE}, orphans: ${ORPHAN_COUNT}"
gc nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "doctor: $SUMMARY"
