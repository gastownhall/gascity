#!/usr/bin/env bash
# mol-dog-doctor — probe Dolt server health and report findings.
#
# Replaces mol-dog-doctor formula. All checks are read-only: SQL probe,
# PROCESSLIST count, disk usage, orphan DB detection, backup artifact freshness.
# No LLM judgment needed — runs inline in the controller.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

PORT="$GC_DOLT_PORT"
HOST="${GC_DOLT_HOST:-127.0.0.1}"
USER="${GC_DOLT_USER:-root}"
LATENCY_WARN_S="${GC_DOCTOR_LATENCY_WARN_S:-1}"
CONN_MAX="${GC_DOCTOR_CONN_MAX:-50}"
CONN_WARN_PCT="${GC_DOCTOR_CONN_WARN_PCT:-80}"
BACKUP_STALE_S="${GC_DOCTOR_BACKUP_STALE_S:-43200}"  # 2x 6h backup interval
BACKUP_ARTIFACT_DIR="${GC_BACKUP_ARTIFACT_DIR:-$GC_CITY_PATH/.dolt-backup}"

dolt_sql() {
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" \
        run_bounded 10 \
        dolt --host "$HOST" --port "$PORT" --user "$USER" --no-tls sql "$@"
}

file_mtime() {
    file_path="$1"
    file_mtime_value=$(stat -c %Y "$file_path" 2>/dev/null \
        || stat -f %m "$file_path" 2>/dev/null || echo "0")
    case "$file_mtime_value" in
        ''|*[!0-9]*) file_mtime_value=0 ;;
    esac
    printf '%s\n' "$file_mtime_value"
}

backup_path_matches_db() {
    db_name="$1"
    backup_rel_path="$2"
    case "$backup_rel_path" in
        "$db_name"|"$db_name"/*|"$db_name".*|"$db_name"-*|*"/$db_name"|*"/$db_name"/*|*"/$db_name".*|*"/$db_name"-*)
            return 0
            ;;
    esac
    return 1
}

newest_backup_mtime_for_db() {
    db_name="$1"
    newest_mtime=0
    while IFS= read -r -d '' backup_path; do
        backup_rel_path="${backup_path#$BACKUP_ARTIFACT_DIR/}"
        if backup_path_matches_db "$db_name" "$backup_rel_path"; then
            backup_mtime=$(file_mtime "$backup_path")
            if [ "$backup_mtime" -gt "$newest_mtime" ]; then
                newest_mtime="$backup_mtime"
            fi
        fi
    done < <(find "$BACKUP_ARTIFACT_DIR" -type f -print0 2>/dev/null)
    printf '%s\n' "$newest_mtime"
}

append_backup_stale() {
    backup_stale_item="$1"
    if [ -n "$BACKUP_STALE_ITEMS" ]; then
        BACKUP_STALE_ITEMS="$BACKUP_STALE_ITEMS, $backup_stale_item"
    else
        BACKUP_STALE_ITEMS="$backup_stale_item"
    fi
}

send_mayor_mail() {
    local mail_err
    if ! mail_err=$(gc mail send mayor/ --from controller "$@" 2>&1 >/dev/null); then
        if [ -n "$mail_err" ]; then
            echo "doctor: mail send failed: $mail_err" >&2
        else
            echo "doctor: mail send failed" >&2
        fi
        return 1
    fi
}

# --- Step 1: Probe connectivity and measure latency ---

PROBE_START=$(date +%s)
if ! dolt_sql -q "SELECT active_branch()" >/dev/null 2>&1; then
    if send_mayor_mail \
        -s "ESCALATION: Dolt server unreachable on port $PORT [CRITICAL]" \
        -m "Doctor probe failed: server did not respond to active_branch() query."; then
        gc session nudge deacon/ "DOG_DONE: doctor — server: UNREACHABLE (escalated)" 2>/dev/null || true
        echo "doctor: server unreachable on port $PORT (escalated)"
    else
        gc session nudge deacon/ "DOG_DONE: doctor — server: UNREACHABLE (mail failed)" 2>/dev/null || true
        echo "doctor: server unreachable on port $PORT (mail failed)"
    fi
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
ORPHAN_PATTERNS="^(testdb_|beads_t|beads_pt|beads_vr|doctest_|doctortest_)"
SYSTEM_DBS="^(information_schema|mysql|dolt_cluster|__gc_probe|performance_schema|sys)$"
USER_DBS=$(printf '%s\n' "$ALL_DBS" | grep -viE "$SYSTEM_DBS" || true)
ORPHANS=$(printf '%s\n' "$USER_DBS" | grep -iE "$ORPHAN_PATTERNS" || true)
ORPHAN_COUNT=$(printf '%s\n' "$ORPHANS" | awk 'NF {count++} END {print count + 0}')
ORPHAN_WARN=""
if [ "${ORPHAN_COUNT:-0}" -gt 0 ]; then
    ORPHAN_WARN=" [WARN: $ORPHAN_COUNT orphan DBs detected — run gc dolt cleanup]"
fi

# Backup freshness: check newest backup artifact per database.
# Scope mirrors mol-dog-backup.sh: only DBs with a configured <db>-backup
# remote are eligible. Cities with user DBs but no backup remotes
# (legitimate config) must not get false stale-backup alarms.
BACKUP_ELIGIBLE_DBS=""
for db in $USER_DBS; do
    db_dir="$DOLT_DATA_DIR/$db"
    if [ -d "$db_dir/.dolt" ]; then
        if (cd "$db_dir" && dolt backup 2>/dev/null | awk '{print $1}' | grep -qx "${db}-backup"); then
            BACKUP_ELIGIBLE_DBS="$BACKUP_ELIGIBLE_DBS $db"
        fi
    fi
done
BACKUP_ELIGIBLE_DBS=$(printf '%s\n' "$BACKUP_ELIGIBLE_DBS" | tr ' ' '\n' | grep -v '^$' || true)

BACKUP_STALE=""
if [ -n "$BACKUP_ELIGIBLE_DBS" ]; then
    if [ ! -d "$BACKUP_ARTIFACT_DIR" ]; then
        BACKUP_STALE=" [WARN: backup artifact dir missing]"
    else
        BACKUP_STALE_ITEMS=""
        NOW_S=$(date +%s)
        for db in $BACKUP_ELIGIBLE_DBS; do
            NEWEST_BACKUP_MTIME=$(newest_backup_mtime_for_db "$db")
            if [ "$NEWEST_BACKUP_MTIME" -le 0 ]; then
                append_backup_stale "$db backup missing"
                continue
            fi
            BACKUP_AGE=$((NOW_S - NEWEST_BACKUP_MTIME))
            if [ "$BACKUP_AGE" -gt "$BACKUP_STALE_S" ]; then
                append_backup_stale "$db backup is $((BACKUP_AGE / 3600))h old"
            fi
        done
        if [ -n "$BACKUP_STALE_ITEMS" ]; then
            BACKUP_STALE=" [WARN: backup freshness: $BACKUP_STALE_ITEMS]"
        fi
    fi
fi

# --- Step 3: Compose report and escalate if critical ---

WARNINGS="${LATENCY_WARN}${CONN_WARN}${ORPHAN_WARN}${BACKUP_STALE}"
if [ -n "$WARNINGS" ]; then
    if ! send_mayor_mail \
        -s "Dolt health advisory [MEDIUM]" \
        -m "Latency: ${LATENCY_S}s${LATENCY_WARN}
Connections: ${CONN_COUNT}/${CONN_MAX}${CONN_WARN}
Disk: ${DISK_USAGE}
Orphan DBs: ${ORPHAN_COUNT}${ORPHAN_WARN}${BACKUP_STALE}"; then
        :
    fi
fi

SUMMARY="doctor — server: ok, latency: ${LATENCY_S}s, conns: ${CONN_COUNT}/${CONN_MAX}, disk: ${DISK_USAGE}, orphans: ${ORPHAN_COUNT}"
gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "doctor: $SUMMARY"
