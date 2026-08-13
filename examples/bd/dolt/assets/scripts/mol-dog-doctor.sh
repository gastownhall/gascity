#!/usr/bin/env bash
# mol-dog-doctor — probe Dolt server health and report findings.
#
# Converted from the former mol-dog-doctor formula. All checks are read-only: SQL probe,
# PROCESSLIST count, disk usage, orphan DB detection, backup artifact freshness.
# No LLM judgment needed — runs inline in the controller.
#
# Runs as an exec order (no LLM, no agent, no wisp).
#
# RPO note: BACKUP_STALE_S (default 43200 = 12h = 2x backup interval) is the
# threshold at which backup artifact age triggers a [WARN: backup stale] advisory.
# With 6h backup syncs and fail-closed journal corruption recovery, maximum data
# loss on journal corruption without manual intervention is one 6h backup interval.
# If BACKUP_STALE_S exceeds 2x the backup interval, a single missed backup cycle
# is undetected. Keep BACKUP_STALE_S <= 2x the configured backup order
# interval (`interval` in orders/mol-dog-backup.toml, currently 6h).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
. "$PACK_DIR/assets/scripts/latency.sh"
. "$PACK_DIR/assets/scripts/loadavg.sh"
. "$PACK_DIR/assets/scripts/supervisor_signals.sh"
. "$PACK_DIR/assets/scripts/advisory_state.sh"
. "$PACK_DIR/assets/scripts/_notify.sh"

PORT="$GC_DOLT_PORT"
HOST="${GC_DOLT_HOST:-127.0.0.1}"
USER="${GC_DOLT_USER:-root}"
# Latency warn threshold in milliseconds. GC_DOCTOR_LATENCY_WARN_MS takes
# precedence; otherwise derive from the legacy seconds knob (default 1s ->
# 1000ms) for backward compatibility.
LATENCY_WARN_MS="${GC_DOCTOR_LATENCY_WARN_MS:-$(( ${GC_DOCTOR_LATENCY_WARN_S:-1} * 1000 ))}"
CONN_WARN_PCT="${GC_DOCTOR_CONN_WARN_PCT:-80}"
BACKUP_STALE_S="${GC_DOCTOR_BACKUP_STALE_S:-43200}"  # 2x 6h backup interval
BACKUP_ARTIFACT_DIR="${GC_BACKUP_ARTIFACT_DIR:-$GC_CITY_PATH/.dolt-backup}"
# Advisory dedup state (#3409): records the signature of the last-sent [MEDIUM]
# advisory so a persistent condition collapses into one rolling alert instead of
# a fresh bead every 5-min tick. DOLT_STATE_DIR is set by runtime.sh.
ADVISORY_STATE_FILE="${GC_DOCTOR_ADVISORY_STATE_FILE:-$DOLT_STATE_DIR/doctor-advisory-state}"

# Watchdog soft-signal knobs (Step 2b): detect the store destabilizing under
# CPU load. Both checks are dolt-free — a local load-average read and a read of
# the controller's own reconcile trace — so they add no query load to the
# server they protect.
#
# Per-core 1-minute load-average warn threshold. A CPU-saturated host starves
# the managed dolt server (latency climbs, the client circuit breaker trips), so
# a sustained per-core load at or above this raises the advisory. Default
# 4.0/core is unambiguous saturation; 0 disables. GC_DOCTOR_LOADAVG_1MIN
# overrides the measured value for tests / constrained hosts.
LOADAVG_WARN_PER_CORE_CENTI="$(to_centi "${GC_DOCTOR_LOADAVG_WARN_PER_CORE:-4.0}")"
# Lookback window for the supervisor reconcile-trace read, sized just above the
# order interval (`interval` in orders/mol-dog-doctor.toml) so consecutive ticks
# overlap and no snapshot is missed between them.
SUPERVISOR_WINDOW="${GC_DOCTOR_SUPERVISOR_WINDOW:-6m}"
# Wall-clock bound (seconds) for that trace read. It is a local, dolt-free
# `gc trace show`, but under the very CPU saturation this watchdog exists to
# catch even a local subprocess can stall — so bound it, exactly as dolt_sql
# bounds its query, to keep the SOFT contract ("never hang, never pile on")
# honest. On timeout the read yields no signal and the doctor degrades to
# "supervisor: ok" for the tick rather than blocking; the next tick re-checks.
SUPERVISOR_TIMEOUT_SECS="${GC_DOCTOR_SUPERVISOR_TIMEOUT_SECS:-10}"

dolt_sql() {
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" \
        run_bounded 10 \
        dolt --host "$HOST" --port "$PORT" --user "$USER" --no-tls sql "$@"
}

# CONN_MAX: explicit override > server @@GLOBAL.max_connections > fallback.
if [ -n "${GC_DOCTOR_CONN_MAX:-}" ]; then
    CONN_MAX="$GC_DOCTOR_CONN_MAX"
else
    _server_max=$(dolt_sql -r csv -q "SELECT @@GLOBAL.max_connections" 2>/dev/null | tail -1 || true)
    case "${_server_max:-}" in
        ''|*[!0-9]*) CONN_MAX=256 ;;
        *) CONN_MAX="$_server_max" ;;
    esac
    unset _server_max
fi

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

send_escalation() {
    local subject="$1"
    local message="$2"
    local err
    if ! err=$(dolt_escalate "$subject" "$message" 2>&1 >/dev/null); then
        if [ -n "$err" ]; then
            echo "doctor: escalation failed: $err" >&2
        else
            echo "doctor: escalation failed" >&2
        fi
        return 1
    fi
}

# --- Step 1: Probe connectivity and measure latency ---

PROBE_START_MS=$(now_ms)
if ! dolt_sql -q "SELECT active_branch()" >/dev/null 2>&1; then
    if send_escalation \
        "ESCALATION: Dolt server unreachable on port $PORT [CRITICAL]" \
        "Doctor probe failed: server did not respond to active_branch() query."; then
        dolt_notify_done "doctor — server: UNREACHABLE (escalated)"
        echo "doctor: server unreachable on port $PORT (escalated)"
    else
        dolt_notify_done "doctor — server: UNREACHABLE (escalation failed)"
        echo "doctor: server unreachable on port $PORT (escalation failed)"
    fi
    exit 0
fi
PROBE_END_MS=$(now_ms)
LATENCY_MS=$((PROBE_END_MS - PROBE_START_MS))
LATENCY_WARN=""
if latency_should_warn "$LATENCY_MS" "$LATENCY_WARN_MS"; then
    LATENCY_WARN=" [WARN: latency ${LATENCY_MS}ms >= threshold ${LATENCY_WARN_MS}ms]"
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
# Every user database is in scope. DBs without a configured <db>-backup
# remote are reported as a coverage gap rather than silently excluded —
# the exclusion is how unconfigured production DBs went unbacked-up until
# journal corruption made them unrecoverable (#3176). mol-dog-backup.sh
# auto-configures the remote on its next run, so this warning self-heals
# unless the backup dog itself is failing.
BACKUP_ELIGIBLE_DBS=""
BACKUP_STALE_ITEMS=""
for db in $USER_DBS; do
    db_dir="$DOLT_DATA_DIR/$db"
    if [ -d "$db_dir/.dolt" ]; then
        if (cd "$db_dir" && run_bounded 30 dolt backup 2>/dev/null | awk '{print $1}' | grep -qx "${db}-backup"); then
            BACKUP_ELIGIBLE_DBS="$BACKUP_ELIGIBLE_DBS $db"
        else
            append_backup_stale "$db backup remote missing"
        fi
    fi
done
BACKUP_ELIGIBLE_DBS=$(printf '%s\n' "$BACKUP_ELIGIBLE_DBS" | tr ' ' '\n' | grep -v '^$' || true)

BACKUP_STALE=""
if [ -n "$BACKUP_ELIGIBLE_DBS" ]; then
    if [ ! -d "$BACKUP_ARTIFACT_DIR" ]; then
        BACKUP_STALE=" [WARN: backup artifact dir missing]"
    else
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
    fi
fi
if [ -n "$BACKUP_STALE_ITEMS" ]; then
    BACKUP_STALE="$BACKUP_STALE [WARN: backup freshness: $BACKUP_STALE_ITEMS]"
fi

# --- Step 2b: Soft degradation signals (dolt-free) ---
#
# Catch the store destabilizing under CPU load *before* it becomes an outage,
# without adding any query load to the server we are watching. Both signals are
# read from outside dolt:
#   - the host 1-minute load average (a local read), and
#   - the controller's own reconcile trace (`gc trace show`), which surfaces
#     scale-check / store PARTIAL reads — the supervisor-side shadow of a tripped
#     "dolt circuit breaker is open".
# If dolt is too wedged to answer, these still report, so the advisory fires.

LOADAVG_1MIN="$(loadavg_1min)"
CPU_COUNT="$(cpu_count)"
LOADAVG_CENTI="$(to_centi "$LOADAVG_1MIN")"
LOADAVG_PER_CORE_CENTI="$(( LOADAVG_CENTI / CPU_COUNT ))"
LOADAVG_WARN=""
if loadavg_should_warn "$LOADAVG_CENTI" "$CPU_COUNT" "$LOADAVG_WARN_PER_CORE_CENTI"; then
    LOADAVG_WARN=" [WARN: load avg 1m $(centi_to_dec "$LOADAVG_PER_CORE_CENTI")/core >= threshold $(centi_to_dec "$LOADAVG_WARN_PER_CORE_CENTI")/core — server may starve]"
fi

# Read the supervisor's recorded reconcile signals. The fetch is both time-bounded
# (run_bounded, like dolt_sql) and error-guarded (|| true) so a missing, slow, or
# wedged trace store can never hang or abort the doctor (SOFT: back off, don't
# pile on); on timeout it yields empty output — i.e. no signal this tick. The
# parse lives in supervisor_signals.sh so it is unit-testable on canned JSON.
SUPERVISOR_JSON="$(run_bounded "$SUPERVISOR_TIMEOUT_SECS" gc trace show --since "$SUPERVISOR_WINDOW" --type cycle_input_snapshot --json 2>/dev/null || true)"
SUPERVISOR_ACTIVE="$(supervisor_signals "$SUPERVISOR_JSON")"
SUPERVISOR_STATE="ok"
SCALECHECK_WARN=""
STORE_WARN=""
case " $SUPERVISOR_ACTIVE " in
    *" scalecheck "*)
        SCALECHECK_WARN=" [WARN: supervisor scale-check query PARTIAL — pools may not scale, work stalls]"
        ;;
esac
case " $SUPERVISOR_ACTIVE " in
    *" store "*)
        STORE_WARN=" [WARN: supervisor store read PARTIAL since last tick — circuit breaker / store instability]"
        ;;
esac
if [ -n "$SCALECHECK_WARN$STORE_WARN" ]; then
    SUPERVISOR_STATE="PARTIAL (${SUPERVISOR_ACTIVE})"
fi

# --- Step 3: Compose report and escalate if critical ---

WARNINGS="${LATENCY_WARN}${CONN_WARN}${ORPHAN_WARN}${BACKUP_STALE}${LOADAVG_WARN}${SCALECHECK_WARN}${STORE_WARN}"
if [ -n "$WARNINGS" ]; then
    # Dedup (#3409): key on which conditions are active — not their tick-volatile
    # values (exact latency ms, connection count, backup age) — and re-send only
    # when that set changes. Record after a successful send so a failed
    # escalation retries next tick. The CRITICAL "server unreachable" path above
    # is never deduped, so a true outage always alerts.
    ADVISORY_SIG=""
    if [ -n "$LATENCY_WARN" ]; then ADVISORY_SIG="${ADVISORY_SIG}latency "; fi
    if [ -n "$CONN_WARN" ]; then ADVISORY_SIG="${ADVISORY_SIG}conn "; fi
    if [ -n "$ORPHAN_WARN" ]; then ADVISORY_SIG="${ADVISORY_SIG}orphan "; fi
    if [ -n "$BACKUP_STALE" ]; then ADVISORY_SIG="${ADVISORY_SIG}backup "; fi
    if [ -n "$LOADAVG_WARN" ]; then ADVISORY_SIG="${ADVISORY_SIG}load "; fi
    if [ -n "$SCALECHECK_WARN" ]; then ADVISORY_SIG="${ADVISORY_SIG}scalecheck "; fi
    if [ -n "$STORE_WARN" ]; then ADVISORY_SIG="${ADVISORY_SIG}store "; fi
    if advisory_changed "$ADVISORY_SIG" "$ADVISORY_STATE_FILE"; then
        if send_escalation \
            "Dolt health advisory [MEDIUM]" \
            "Latency: ${LATENCY_MS}ms${LATENCY_WARN}
Connections: ${CONN_COUNT}/${CONN_MAX}${CONN_WARN}
Disk: ${DISK_USAGE}
Orphan DBs: ${ORPHAN_COUNT}${ORPHAN_WARN}${BACKUP_STALE}
Load avg (1m): ${LOADAVG_1MIN} over ${CPU_COUNT} core(s)${LOADAVG_WARN}
Supervisor reconcile: ${SUPERVISOR_STATE}${SCALECHECK_WARN}${STORE_WARN}"; then
            advisory_record "$ADVISORY_SIG" "$ADVISORY_STATE_FILE"
        fi
    fi
else
    # Healthy this tick. If the prior tick was degraded (state file non-empty),
    # send exactly one [RECOVERED] note — the recovery mirror of the dedup: one
    # alert per transition — then forget the advisory so a future condition
    # re-alerts. Clear only after a successful send so a failed recovery note
    # retries next tick.
    if [ -s "$ADVISORY_STATE_FILE" ]; then
        PRIOR_SIG=""
        IFS= read -r PRIOR_SIG < "$ADVISORY_STATE_FILE" 2>/dev/null || true
        if send_escalation \
            "Dolt health advisory [RECOVERED]" \
            "Dolt health has returned to normal after a degraded advisory.
Latency: ${LATENCY_MS}ms
Connections: ${CONN_COUNT}/${CONN_MAX}
Load avg (1m): ${LOADAVG_1MIN} over ${CPU_COUNT} core(s)
Supervisor reconcile: ${SUPERVISOR_STATE}
Cleared conditions: ${PRIOR_SIG}"; then
            advisory_clear "$ADVISORY_STATE_FILE"
        fi
    else
        advisory_clear "$ADVISORY_STATE_FILE"
    fi
fi

SUMMARY="doctor — server: ok, latency: ${LATENCY_MS}ms, conns: ${CONN_COUNT}/${CONN_MAX}, disk: ${DISK_USAGE}, orphans: ${ORPHAN_COUNT}, load: ${LOADAVG_1MIN}/${CPU_COUNT}c, supervisor: ${SUPERVISOR_STATE}"
dolt_notify_done "$SUMMARY"
echo "doctor: $SUMMARY"
