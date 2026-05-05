#!/usr/bin/env bash
# reaper — reap stale wisps, purge old closed data, auto-close stale issues.
#
# Replaces mol-dog-reaper formula. All operations are deterministic:
# SQL queries with age thresholds, bd close/update commands, count
# comparisons against alert thresholds.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

CITY="${GC_CITY:-.}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/dolt-target.sh"

# Configurable thresholds (defaults match the old formula).
MAX_AGE="${GC_REAPER_MAX_AGE:-24h}"
PURGE_AGE="${GC_REAPER_PURGE_AGE:-168h}"
STALE_ISSUE_AGE="${GC_REAPER_STALE_ISSUE_AGE:-720h}"
MAIL_DELETE_AGE="${GC_REAPER_MAIL_DELETE_AGE:-168h}"
ALERT_THRESHOLD="${GC_REAPER_ALERT_THRESHOLD:-500}"
DRY_RUN="${GC_REAPER_DRY_RUN:-}"

# Convert Go durations to SQL INTERVAL hours for Dolt.
duration_to_hours() {
    local dur="$1"
    # Strip trailing 'h' and return as integer.
    echo "${dur%h}"
}

MAX_AGE_H=$(duration_to_hours "$MAX_AGE")
PURGE_AGE_H=$(duration_to_hours "$PURGE_AGE")
STALE_AGE_H=$(duration_to_hours "$STALE_ISSUE_AGE")
MAIL_AGE_H=$(duration_to_hours "$MAIL_DELETE_AGE")

# Discover databases from Dolt server. Exclude Dolt/MySQL system schemas,
# Gas City's internal health-probe database, and test-fixture scratch
# databases (benchdb, testdb_*, lowercase beads_t[0-9a-f]{8,}, beads_pt*,
# beads_vr*, doctest_*, doctortest_* — matching the Go cleanup planner
# contract); the remainder are bead stores.
DATABASES=$(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2 \
    | grep -vi '^information_schema$\|^mysql$\|^dolt_cluster$\|^performance_schema$\|^sys$\|^__gc_probe$\|^benchdb$\|^testdb_\|^beads_pt\|^beads_vr\|^doctest_\|^doctortest_' \
    | grep -v '^beads_t[0-9a-f]\{8,\}$' || true)
if [ -z "$DATABASES" ]; then
    # No databases accessible — nothing to do.
    exit 0
fi

TOTAL_REAPED=0
TOTAL_PURGED=0
TOTAL_MAIL_PURGED=0
TOTAL_ISSUES_CLOSED=0
ANOMALIES=""

for DB in $DATABASES; do
    # Step 1: Reap — close open wisps past max_age with closed/missing parent.
    REAP_COUNT=$(dolt_sql -r csv -q "
        SELECT COUNT(*) FROM \`$DB\`.wisps w
        LEFT JOIN \`$DB\`.wisps parent ON w.parent_id = parent.id
        WHERE w.status IN ('open', 'hooked', 'in_progress')
        AND w.created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
        AND (parent.id IS NULL OR parent.status = 'closed')
    " 2>/dev/null | tail -1 || echo "0")

    if [ "$REAP_COUNT" -gt 0 ] && [ -z "$DRY_RUN" ]; then
        dolt_sql -q "
            UPDATE \`$DB\`.wisps SET status='closed', closed_at=NOW()
            WHERE status IN ('open', 'hooked', 'in_progress')
            AND created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            AND id IN (
                SELECT w.id FROM (SELECT * FROM \`$DB\`.wisps) w
                LEFT JOIN \`$DB\`.wisps parent ON w.parent_id = parent.id
                WHERE parent.id IS NULL OR parent.status = 'closed'
            )
        " 2>/dev/null || true
        TOTAL_REAPED=$((TOTAL_REAPED + REAP_COUNT))
    fi

    # Step 2: Purge — delete closed wisps past purge_age.
    PURGE_COUNT=$(dolt_sql -r csv -q "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status = 'closed'
        AND closed_at < DATE_SUB(NOW(), INTERVAL $PURGE_AGE_H HOUR)
    " 2>/dev/null | tail -1 || echo "0")

    if [ "$PURGE_COUNT" -gt 0 ] && [ -z "$DRY_RUN" ]; then
        dolt_sql -q "
            DELETE FROM \`$DB\`.wisps
            WHERE status = 'closed'
            AND closed_at < DATE_SUB(NOW(), INTERVAL $PURGE_AGE_H HOUR)
        " 2>/dev/null || true
        TOTAL_PURGED=$((TOTAL_PURGED + PURGE_COUNT))
    fi

    # Step 3: Purge closed mail past mail_delete_age.
    MAIL_COUNT=$(dolt_sql -r csv -q "
        SELECT COUNT(*) FROM \`$DB\`.mail
        WHERE status = 'closed'
        AND closed_at < DATE_SUB(NOW(), INTERVAL $MAIL_AGE_H HOUR)
    " 2>/dev/null | tail -1 || echo "0")

    if [ "$MAIL_COUNT" -gt 0 ] && [ -z "$DRY_RUN" ]; then
        dolt_sql -q "
            DELETE FROM \`$DB\`.mail
            WHERE status = 'closed'
            AND closed_at < DATE_SUB(NOW(), INTERVAL $MAIL_AGE_H HOUR)
        " 2>/dev/null || true
        TOTAL_MAIL_PURGED=$((TOTAL_MAIL_PURGED + MAIL_COUNT))
    fi

    # Step 4: Auto-close stale issues (exclude P0/P1, epics, active deps).
    STALE_IDS=$(dolt_sql -r csv -q "
        SELECT id FROM \`$DB\`.issues
        WHERE status IN ('open', 'in_progress')
        AND updated_at < DATE_SUB(NOW(), INTERVAL $STALE_AGE_H HOUR)
        AND priority > 1
        AND issue_type != 'epic'
        AND id NOT IN (
            SELECT DISTINCT d.issue_id FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.issues i ON d.depends_on_id = i.id
            WHERE i.status IN ('open', 'in_progress')
            UNION
            SELECT DISTINCT d.depends_on_id FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.issues i ON d.issue_id = i.id
            WHERE i.status IN ('open', 'in_progress')
        )
    " 2>/dev/null | tail -n +2 || true)

    if [ -n "$STALE_IDS" ] && [ -z "$DRY_RUN" ]; then
        while IFS= read -r issue_id; do
            [ -z "$issue_id" ] && continue
            bd close "$issue_id" --reason "stale:auto-closed by reaper" 2>/dev/null || true
            TOTAL_ISSUES_CLOSED=$((TOTAL_ISSUES_CLOSED + 1))
        done <<< "$STALE_IDS"
    fi

    # Step 5: Anomaly check — open wisp count.
    OPEN_WISPS=$(dolt_sql -r csv -q "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
    " 2>/dev/null | tail -1 || echo "0")

    if [ "$OPEN_WISPS" -gt "$ALERT_THRESHOLD" ]; then
        ANOMALIES="${ANOMALIES}$DB: $OPEN_WISPS open wisps (threshold: $ALERT_THRESHOLD)\n"
    fi

    # Commit Dolt changes.
    if [ -z "$DRY_RUN" ]; then
        dolt_sql -q "
            SELECT DOLT_COMMIT('-Am', 'reaper: reaped=$REAP_COUNT purged=$PURGE_COUNT mail=$MAIL_COUNT stale=$TOTAL_ISSUES_CLOSED', '--author', 'reaper <reaper@gastown.local>')
        " 2>/dev/null || true
    fi
done

# Report.
if [ -n "$ANOMALIES" ]; then
    gc mail send mayor/ -s "ESCALATION: Reaper anomalies detected [MEDIUM]" \
        -m "$(echo -e "$ANOMALIES")" 2>/dev/null || true
fi

SUMMARY="reaper — reaped:$TOTAL_REAPED, purged:$TOTAL_PURGED, mail:$TOTAL_MAIL_PURGED, closed:$TOTAL_ISSUES_CLOSED"
if [ -n "$DRY_RUN" ]; then
    SUMMARY="$SUMMARY (dry run)"
fi

gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "reaper: $SUMMARY"
