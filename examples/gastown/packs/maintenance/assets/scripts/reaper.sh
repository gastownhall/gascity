#!/usr/bin/env bash
# reaper — close stale wisps with closed parents/roots, purge old closed data, auto-close stale and TTL-expired issues.
#
# Replaces mol-dog-reaper formula. All operations are deterministic:
# SQL queries with age thresholds, bd close/update commands, count
# comparisons against alert thresholds.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "reaper"

CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/dolt-target.sh"
CITY_ABS="$(cd "$CITY" 2>/dev/null && pwd -P || printf '%s\n' "$CITY")"
CITY_BEADS_DIR="$CITY_ABS/.beads"

# Configurable thresholds.
MAX_AGE="${GC_REAPER_MAX_AGE:-24h}"
PURGE_AGE="${GC_REAPER_PURGE_AGE:-168h}"
STALE_ISSUE_AGE="${GC_REAPER_STALE_ISSUE_AGE:-720h}"
SESSION_PURGE_AGE="${GC_REAPER_SESSION_PURGE_AGE:-720h}"
SESSION_STATE_PRUNE_AGE="${GC_REAPER_SESSION_STATE_PRUNE_AGE:-24h}"
ALERT_THRESHOLD="${GC_REAPER_ALERT_THRESHOLD:-500}"
MAIL_ALERT_THRESHOLD="${GC_REAPER_MAIL_ALERT_THRESHOLD:-0}"  # 0 = disabled
DRY_RUN="${GC_REAPER_DRY_RUN:-}"
# Closing follows only ownership edges. `blocks` is sequencing, not ownership;
# purge protection may include it because that only prevents deletion.
WISP_CLOSE_EDGE_PREDICATE="(d.type = 'parent-child' OR (d.type = 'tracks' AND JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.root_bead_id\"')) = d.depends_on_id))"
WISP_PURGE_PROTECT_EDGE_TYPES="'parent-child', 'tracks', 'blocks'"

# Convert Go durations to SQL INTERVAL hours for Dolt.
duration_to_hours() {
    local dur="$1"
    # Strip trailing 'h' and return as integer.
    echo "${dur%h}"
}

MAX_AGE_H=$(duration_to_hours "$MAX_AGE")
PURGE_AGE_H=$(duration_to_hours "$PURGE_AGE")
STALE_AGE_H=$(duration_to_hours "$STALE_ISSUE_AGE")

CITY_DB_METADATA_RESULT=""

city_database_name() {
    local metadata="$CITY_BEADS_DIR/metadata.json"
    local db=""
    CITY_DB_METADATA_RESULT=""

    if [ -f "$metadata" ]; then
        if command -v jq >/dev/null 2>&1; then
            if ! db=$(jq -er '.dolt_database // empty | strings' "$metadata" 2>/dev/null); then
                return 0
            fi
        elif command -v python3 >/dev/null 2>&1; then
            if ! db=$(python3 - "$metadata" 2>/dev/null <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as f:
    value = json.load(f).get("dolt_database", "")
if isinstance(value, str) and value:
    print(value)
PY
            ); then
                return 0
            fi
        elif command -v grep >/dev/null 2>&1 && command -v sed >/dev/null 2>&1 && command -v head >/dev/null 2>&1; then
            if grep -q '}' "$metadata" 2>/dev/null; then
                db=$(grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$metadata" 2>/dev/null \
                    | sed 's/.*"dolt_database"[[:space:]]*:[[:space:]]*"//;s/"//' \
                    | head -1 || true)
            fi
        else
            return 0
        fi
    fi

    if [ -n "$db" ]; then
        CITY_DB_METADATA_RESULT="$db"
    fi
}

is_user_database() {
    case "$1" in
        information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe|benchdb|testdb_*|beads_pt*|beads_vr*|beads_test_bench_*|doctest_*|doctortest_*)
            return 1
            ;;
        beads_t*)
            local suffix="${1#beads_t}"
            if [[ "$suffix" =~ ^[0-9a-f]{8,}$ ]]; then
                return 1
            fi
            return 0
            ;;
        *)
            return 0
            ;;
    esac
}

# Discover databases from Dolt server. Exclude Dolt/MySQL system schemas,
# Gas City's internal health-probe database, and test-fixture scratch
# databases (benchdb, testdb_*, lowercase beads_t[0-9a-f]{8,}, beads_pt*,
# beads_vr*, beads_test_bench_*, doctest_*, doctortest_* — matching the Go
# cleanup planner contract); the remainder are bead stores.
DATABASES=$(
    while IFS= read -r db; do
        if is_user_database "$db"; then
            printf '%s\n' "$db"
        fi
    done < <(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2)
)
HAD_DATABASES=1
if [ -z "$DATABASES" ]; then
    # The Dolt-backed cleanup loop has no work, but the session-bead
    # prune below still operates through bd's configured task store.
    HAD_DATABASES=0
fi

TOTAL_STALE_WISPS=0
TOTAL_CLOSED_WISPS=0
TOTAL_WOULD_CLOSE_WISPS=0
TOTAL_WOULD_EXPIRE=0
TOTAL_PURGED=0
TOTAL_MAIL_WISPS=0
TOTAL_ISSUES_CLOSED=0
TOTAL_STALE_ISSUES_SKIPPED=0
TOTAL_EXPIRED_ISSUES_CLOSED=0
TOTAL_EXPIRED_ISSUES_SKIPPED=0
TOTAL_SESSIONS_PRUNED=0
SESSION_PRUNE_ATTEMPTED=0
ANOMALIES=""

sanitize_output() {
    printf '%s' "$1" | tr '\n' ' ' | cut -c1-4000
}

record_anomaly() {
    local db="$1"
    shift
    ANOMALIES="${ANOMALIES}$db: $*
"
}

CITY_DB_ANOMALY_RECORDED=0

valid_database_identifier() {
    local name="$1"

    case "$name" in
        ''|-*|*[!A-Za-z0-9_-]*)
            return 1
            ;;
    esac

    return 0
}

database_list_contains() {
    local needle="$1"
    local db

    while IFS= read -r db; do
        if [ "$db" = "$needle" ]; then
            return 0
        fi
    done <<EOF
$DATABASES
EOF

    return 1
}

CITY_DB=""
CITY_DB_SOURCE="$CITY_BEADS_DIR/metadata.json"
city_database_name
CITY_METADATA_DB="$CITY_DB_METADATA_RESULT"

if [ -n "${GC_REAPER_CITY_DATABASE:-}" ]; then
    CITY_DB_SOURCE="GC_REAPER_CITY_DATABASE"
    if [ -z "$CITY_METADATA_DB" ]; then
        record_anomaly "city" "city database $GC_REAPER_CITY_DATABASE from GC_REAPER_CITY_DATABASE could not be verified against $CITY_BEADS_DIR/metadata.json; stale issue auto-close disabled"
        CITY_DB_ANOMALY_RECORDED=1
    elif [ "$GC_REAPER_CITY_DATABASE" != "$CITY_METADATA_DB" ]; then
        record_anomaly "city" "city database $GC_REAPER_CITY_DATABASE from GC_REAPER_CITY_DATABASE does not match city metadata database $CITY_METADATA_DB; stale issue auto-close disabled"
        CITY_DB_ANOMALY_RECORDED=1
    else
        CITY_DB="$GC_REAPER_CITY_DATABASE"
    fi
else
    CITY_DB="$CITY_METADATA_DB"
fi

if [ -n "$CITY_DB" ] && ! valid_database_identifier "$CITY_DB"; then
    record_anomaly "city" "city database $CITY_DB from $CITY_DB_SOURCE is not a safe Dolt identifier; stale issue auto-close disabled"
    CITY_DB=""
    CITY_DB_ANOMALY_RECORDED=1
elif [ -n "$CITY_DB" ] && ! database_list_contains "$CITY_DB"; then
    record_anomaly "city" "city database $CITY_DB from $CITY_DB_SOURCE was not found in discovered databases; stale issue auto-close disabled"
    CITY_DB=""
    CITY_DB_ANOMALY_RECORDED=1
fi

SQL_COUNT_RESULT=0
get_sql_count() {
    local db="$1"
    local label="$2"
    local query="$3"
    local output
    local stderr_file
    local stderr_output
    local count

    SQL_COUNT_RESULT=0
    if ! stderr_file=$(mktemp); then
        record_anomaly "$db" "$label count failed for $db: could not create stderr capture file"
        return 0
    fi
    if ! output=$(dolt_sql -r csv -q "$query" 2>"$stderr_file"); then
        stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
        rm -f "$stderr_file"
        record_anomaly "$db" "$label count failed for $db: $(sanitize_output "$stderr_output $output")"
        return 0
    fi
    rm -f "$stderr_file"

    count=$(printf '%s\n' "$output" | tail -1 | tr -d '\r')
    if [ -z "$count" ] || ! [[ "$count" =~ ^[0-9]+$ ]]; then
        record_anomaly "$db" "$label count returned non-numeric value for $db: $(sanitize_output "$output")"
        return 0
    fi

    SQL_COUNT_RESULT="$count"
}

SQL_ROWS_RESULT=""
get_sql_rows() {
    local db="$1"
    local label="$2"
    local query="$3"
    local output
    local stderr_file
    local stderr_output

    SQL_ROWS_RESULT=""
    if ! stderr_file=$(mktemp); then
        record_anomaly "$db" "$label query failed for $db: could not create stderr capture file"
        return 0
    fi
    if ! output=$(dolt_sql -r csv -q "$query" 2>"$stderr_file"); then
        stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
        rm -f "$stderr_file"
        record_anomaly "$db" "$label query failed for $db: $(sanitize_output "$stderr_output $output")"
        return 0
    fi
    rm -f "$stderr_file"

    SQL_ROWS_RESULT=$(printf '%s\n' "$output" | tail -n +2 | tr -d '\r')
}

has_dependency_target_column() {
    local db="$1"
    local table="$2"
    local output
    local fields

    if ! output=$(dolt_sql -r csv -q "SHOW COLUMNS FROM \`$db\`.$table" 2>/dev/null); then
        return 1
    fi

    fields=$(printf '%s\n' "$output" | tail -n +2 | cut -d, -f1 | tr -d '\r')
    if [ -z "$fields" ]; then
        return 0
    fi

    printf '%s\n' "$fields" | grep -qx 'issue_id' || return 1
    printf '%s\n' "$fields" | grep -qx 'depends_on_id' || return 1
}

SQL_CHANGE_ROWS_RESULT=0
close_city_issue() {
    local issue_id="$1"
    local reason="$2"

    if [ ! -d "$CITY_BEADS_DIR" ]; then
        printf 'city bead store %s is unavailable' "$CITY_BEADS_DIR"
        return 1
    fi

    (
        cd "$CITY_ABS"
        BEADS_DIR="$CITY_BEADS_DIR" bd close "$issue_id" --reason "$reason"
    )
}

run_sql_change() {
    local db="$1"
    local label="$2"
    local query="$3"
    local output
    local rows
    local stderr_file
    local stderr_output

    SQL_CHANGE_ROWS_RESULT=0
    if ! stderr_file=$(mktemp); then
        record_anomaly "$db" "$label failed for $db: could not create stderr capture file"
        return 1
    fi
    # DML (DELETE/UPDATE) against a database-qualified table still needs an
    # active database selected, or Dolt can reject it with "no database
    # selected" (Error 1105) even though the target is fully qualified —
    # reads (get_sql_count/get_sql_rows) do not. USE the target db first,
    # mirroring the DOLT_COMMIT block below.
    if ! output=$(dolt_sql -r csv -q "
USE \`$db\`;
$query;
SELECT ROW_COUNT();
    " 2>"$stderr_file"); then
        stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
        rm -f "$stderr_file"
        record_anomaly "$db" "$label failed for $db: $(sanitize_output "$stderr_output $output")"
        return 1
    fi
    stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
    rm -f "$stderr_file"

    rows=$(printf '%s\n' "$output" | tail -1 | tr -d '\r')
    if [ -z "$rows" ] || ! [[ "$rows" =~ ^[0-9]+$ ]]; then
        record_anomaly "$db" "$label returned non-numeric row count for $db: $(sanitize_output "$stderr_output $output")"
        return 1
    fi

    SQL_CHANGE_ROWS_RESULT="$rows"
    return 0
}

while IFS= read -r DB; do
    [ -z "$DB" ] && continue
    if ! valid_database_identifier "$DB"; then
        record_anomaly "$DB" "unsafe Dolt database identifier skipped by reaper"
        continue
    fi
    if ! has_wisps_table "$DB"; then
        # Not a bd-managed bead store. Skip silently; recording an
        # anomaly here would just turn every schemaless DB on the
        # server into noise. See gastownhall/gascity#1816.
        continue
    fi
    if ! has_dependency_target_column "$DB" "dependencies" || ! has_dependency_target_column "$DB" "wisp_dependencies"; then
        # Older or incompatible dependency schema. Skip silently like the
        # has_wisps_table gate above; the only fix is a bead-store schema
        # migration, which the reaper cannot perform.
        continue
    fi

    DB_MUTATIONS=0

    # Step 1: Count stale non-closed wisps, then close only candidates whose
    # explicit ownership edge points to a closed parent/root. Wisps
    # without an ownership edge are reported but not closed by age alone.
    get_sql_count "$DB" "stale non-closed wisp" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
        AND created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
    "
    STALE_WISP_COUNT=$SQL_COUNT_RESULT

    if [ "$STALE_WISP_COUNT" -gt 0 ]; then
        TOTAL_STALE_WISPS=$((TOTAL_STALE_WISPS + STALE_WISP_COUNT))
    fi

    CLOSE_WISP_COUNT=0
    DB_CLOSED_WISPS=0
    DB_PURGED=0
    while [ "$STALE_WISP_COUNT" -gt 0 ] && [ "$CLOSE_WISP_COUNT" -lt "$STALE_WISP_COUNT" ]; do
        get_sql_count "$DB" "schema-safe stale wisp" "
            SELECT COUNT(DISTINCT w.id) FROM \`$DB\`.wisps w
            INNER JOIN \`$DB\`.wisp_dependencies d
                ON d.issue_id = w.id
                AND $WISP_CLOSE_EDGE_PREDICATE
            LEFT JOIN \`$DB\`.wisps parent_wisp ON d.depends_on_id = parent_wisp.id
            LEFT JOIN \`$DB\`.issues parent_issue ON d.depends_on_id = parent_issue.id
            WHERE w.status IN ('open', 'hooked', 'in_progress')
            AND w.created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            AND (
                parent_wisp.status = 'closed'
                OR parent_issue.status = 'closed'
            )
        "
        CLOSE_WISP_BATCH=$SQL_COUNT_RESULT
        if [ "$CLOSE_WISP_BATCH" -eq 0 ]; then
            break
        fi
        if [ -n "$DRY_RUN" ]; then
            TOTAL_WOULD_CLOSE_WISPS=$((TOTAL_WOULD_CLOSE_WISPS + CLOSE_WISP_BATCH))
            break
        fi

        if run_sql_change "$DB" "closing stale wisps" "
            UPDATE \`$DB\`.wisps SET status='closed', closed_at=NOW()
            WHERE status IN ('open', 'hooked', 'in_progress')
            AND created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            AND id IN (
                SELECT id FROM (
                    SELECT w.id FROM \`$DB\`.wisps w
                    INNER JOIN \`$DB\`.wisp_dependencies d
                        ON d.issue_id = w.id
                        AND $WISP_CLOSE_EDGE_PREDICATE
                    LEFT JOIN \`$DB\`.wisps parent_wisp ON d.depends_on_id = parent_wisp.id
                    LEFT JOIN \`$DB\`.issues parent_issue ON d.depends_on_id = parent_issue.id
                    WHERE w.status IN ('open', 'hooked', 'in_progress')
                    AND w.created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
                    AND (
                        parent_wisp.status = 'closed'
                        OR parent_issue.status = 'closed'
                    )
                ) reaper_wisp_candidates
            )
        "; then
            CLOSE_WISP_ROWS=$SQL_CHANGE_ROWS_RESULT
            if [ "$CLOSE_WISP_ROWS" -eq 0 ]; then
                break
            fi
            CLOSE_WISP_COUNT=$((CLOSE_WISP_COUNT + CLOSE_WISP_ROWS))
            DB_CLOSED_WISPS=$((DB_CLOSED_WISPS + CLOSE_WISP_ROWS))
            TOTAL_CLOSED_WISPS=$((TOTAL_CLOSED_WISPS + CLOSE_WISP_ROWS))
            DB_MUTATIONS=$((DB_MUTATIONS + CLOSE_WISP_ROWS))
        else
            break
        fi
    done

    # Step 2: Purge — delete closed wisps past purge_age.
    get_sql_count "$DB" "closed wisp purge" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status = 'closed'
        AND closed_at < DATE_SUB(NOW(), INTERVAL $PURGE_AGE_H HOUR)
        AND id NOT IN (
            SELECT DISTINCT d.depends_on_id FROM \`$DB\`.wisp_dependencies d
            INNER JOIN \`$DB\`.wisps child_wisp ON d.issue_id = child_wisp.id
            WHERE d.type IN ($WISP_PURGE_PROTECT_EDGE_TYPES)
            AND child_wisp.status IN ('open', 'hooked', 'in_progress')
        )
    "
    PURGE_COUNT=$SQL_COUNT_RESULT

    if [ "$PURGE_COUNT" -gt 0 ] && [ -z "$DRY_RUN" ]; then
        if run_sql_change "$DB" "purging closed wisps" "
            DELETE FROM \`$DB\`.wisps
            WHERE status = 'closed'
            AND closed_at < DATE_SUB(NOW(), INTERVAL $PURGE_AGE_H HOUR)
            AND id NOT IN (
                SELECT DISTINCT d.depends_on_id FROM \`$DB\`.wisp_dependencies d
                INNER JOIN \`$DB\`.wisps child_wisp ON d.issue_id = child_wisp.id
                WHERE d.type IN ($WISP_PURGE_PROTECT_EDGE_TYPES)
                AND child_wisp.status IN ('open', 'hooked', 'in_progress')
            )
        "; then
            PURGED_ROWS=$SQL_CHANGE_ROWS_RESULT
            DB_PURGED=$((DB_PURGED + PURGED_ROWS))
            TOTAL_PURGED=$((TOTAL_PURGED + PURGED_ROWS))
            DB_MUTATIONS=$((DB_MUTATIONS + PURGED_ROWS))
        fi
    fi

    # Step 3: Close nudge beads whose metadata.expires_at is in the past.
    # Only beads labelled gc:nudge are candidates — other bead types that stamp
    # expires_at (e.g. gc:extmsg-binding session bindings) must not be closed
    # here.  The COALESCE handles whole-second RFC3339+Z, microsecond-width
    # RFC3339 (MySQL %f tops out at 6 fractional digits), and full
    # RFC3339Nano (7-9 fractional digits) by truncating the fractional part to
    # whole seconds for parsing — sub-second precision is immaterial for TTL
    # expiry.  Rows where every pattern fails STR_TO_DATE return NULL and are
    # recorded as anomalies rather than silently skipped.
    DB_EXPIRED_ISSUES_CLOSED=0
    get_sql_rows "$DB" "expired nudge bead with parse anomaly" "
        SELECT i.id
        FROM \`$DB\`.issues i
        INNER JOIN \`$DB\`.labels lbl ON lbl.issue_id = i.id AND lbl.label = 'gc:nudge'
        WHERE i.status IN ('open', 'in_progress')
        AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')) IS NOT NULL
        AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')) != ''
        AND COALESCE(
            STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '%Y-%m-%dT%H:%i:%s.%fZ'),
            STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '%Y-%m-%dT%H:%i:%sZ'),
            STR_TO_DATE(CONCAT(SUBSTRING_INDEX(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '.', 1), 'Z'), '%Y-%m-%dT%H:%i:%sZ')
        ) IS NULL
    "
    if [ -n "$SQL_ROWS_RESULT" ]; then
        while IFS= read -r bad_id; do
            [ -z "$bad_id" ] && continue
            record_anomaly "$DB" "nudge bead $bad_id in $DB has unparseable expires_at; skipped by TTL reaper"
        done <<< "$SQL_ROWS_RESULT"
    fi

    get_sql_rows "$DB" "expired nudge bead" "
        SELECT i.id
        FROM \`$DB\`.issues i
        INNER JOIN \`$DB\`.labels lbl ON lbl.issue_id = i.id AND lbl.label = 'gc:nudge'
        WHERE i.status IN ('open', 'in_progress')
        AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')) IS NOT NULL
        AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')) != ''
        AND COALESCE(
            STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '%Y-%m-%dT%H:%i:%s.%fZ'),
            STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '%Y-%m-%dT%H:%i:%sZ'),
            STR_TO_DATE(CONCAT(SUBSTRING_INDEX(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.expires_at')), '.', 1), 'Z'), '%Y-%m-%dT%H:%i:%sZ')
        ) < UTC_TIMESTAMP()
    "
    EXPIRED_IDS=$SQL_ROWS_RESULT
    if [ -n "$EXPIRED_IDS" ]; then
        WOULD_EXPIRE_COUNT=$(printf '%s\n' "$EXPIRED_IDS" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')
        TOTAL_WOULD_EXPIRE=$((TOTAL_WOULD_EXPIRE + WOULD_EXPIRE_COUNT))
    fi

    if [ -n "$EXPIRED_IDS" ] && [ -z "$DRY_RUN" ]; then
        if [ -z "$CITY_DB" ]; then
            if [ "$CITY_DB_ANOMALY_RECORDED" -eq 0 ]; then
                record_anomaly "city" "city database could not be determined from GC_REAPER_CITY_DATABASE or $CITY/.beads/metadata.json; expired nudge close disabled"
                CITY_DB_ANOMALY_RECORDED=1
            fi
            SKIPPED_COUNT=$(printf '%s\n' "$EXPIRED_IDS" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')
            TOTAL_EXPIRED_ISSUES_SKIPPED=$((TOTAL_EXPIRED_ISSUES_SKIPPED + SKIPPED_COUNT))
        elif [ "$DB" != "$CITY_DB" ]; then
            SKIPPED_COUNT=$(printf '%s\n' "$EXPIRED_IDS" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')
            TOTAL_EXPIRED_ISSUES_SKIPPED=$((TOTAL_EXPIRED_ISSUES_SKIPPED + SKIPPED_COUNT))
        else
            while IFS= read -r issue_id; do
                [ -z "$issue_id" ] && continue
                if CLOSE_OUTPUT=$(close_city_issue "$issue_id" "ttl:expired by reaper" 2>&1); then
                    DB_EXPIRED_ISSUES_CLOSED=$((DB_EXPIRED_ISSUES_CLOSED + 1))
                    TOTAL_EXPIRED_ISSUES_CLOSED=$((TOTAL_EXPIRED_ISSUES_CLOSED + 1))
                    DB_MUTATIONS=$((DB_MUTATIONS + 1))
                else
                    record_anomaly "$DB" "closing expired nudge bead $issue_id failed for $DB: $(sanitize_output "$CLOSE_OUTPUT")"
                fi
            done <<< "$EXPIRED_IDS"
        fi
    fi

    # Step 4: Auto-close stale issues (exclude P0/P1, epics, active deps).
    DB_ISSUES_CLOSED=0
    get_sql_rows "$DB" "stale issue" "
        SELECT id FROM \`$DB\`.issues
        WHERE status IN ('open', 'in_progress')
        AND updated_at < DATE_SUB(NOW(), INTERVAL $STALE_AGE_H HOUR)
        AND priority > 1
        AND issue_type != 'epic'
        AND (
            JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at')) IS NULL
            OR JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at')) = ''
        )
        AND id NOT IN (
            SELECT DISTINCT d.issue_id FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.issues i ON d.depends_on_id = i.id
            WHERE i.status IN ('open', 'in_progress')
            UNION
            SELECT DISTINCT d.depends_on_id FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.issues i ON d.issue_id = i.id
            WHERE i.status IN ('open', 'in_progress')
        )
    "
    STALE_IDS=$SQL_ROWS_RESULT

    if [ -n "$STALE_IDS" ] && [ -z "$DRY_RUN" ]; then
        if [ -z "$CITY_DB" ]; then
            if [ "$CITY_DB_ANOMALY_RECORDED" -eq 0 ]; then
                record_anomaly "city" "city database could not be determined from GC_REAPER_CITY_DATABASE or $CITY/.beads/metadata.json; stale issue auto-close disabled"
                CITY_DB_ANOMALY_RECORDED=1
            fi
            SKIPPED_ISSUES=$(printf '%s\n' "$STALE_IDS" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')
            TOTAL_STALE_ISSUES_SKIPPED=$((TOTAL_STALE_ISSUES_SKIPPED + SKIPPED_ISSUES))
        elif [ "$DB" != "$CITY_DB" ]; then
            SKIPPED_ISSUES=$(printf '%s\n' "$STALE_IDS" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')
            TOTAL_STALE_ISSUES_SKIPPED=$((TOTAL_STALE_ISSUES_SKIPPED + SKIPPED_ISSUES))
        else
            while IFS= read -r issue_id; do
                [ -z "$issue_id" ] && continue
                if CLOSE_OUTPUT=$(close_city_issue "$issue_id" "stale:auto-closed by reaper" 2>&1); then
                    DB_ISSUES_CLOSED=$((DB_ISSUES_CLOSED + 1))
                    TOTAL_ISSUES_CLOSED=$((TOTAL_ISSUES_CLOSED + 1))
                    DB_MUTATIONS=$((DB_MUTATIONS + 1))
                else
                    record_anomaly "$DB" "closing stale issue $issue_id failed for $DB: $(sanitize_output "$CLOSE_OUTPUT")"
                fi
            done <<< "$STALE_IDS"
        fi
    fi

    # Step 5a: Anomaly check — stale open wisp count. Fresh workflow load can
    # legitimately exceed the threshold on busy cities; only old non-message
    # rows indicate a reaper leak.
    get_sql_count "$DB" "stale open wisp anomaly" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
        AND issue_type NOT IN ('message')
        AND created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
    "
    REAPABLE_WISPS=$SQL_COUNT_RESULT

    if [ "$REAPABLE_WISPS" -gt "$ALERT_THRESHOLD" ]; then
        ANOMALIES="${ANOMALIES}$DB: $REAPABLE_WISPS stale open wisps (threshold: $ALERT_THRESHOLD, age: ${MAX_AGE})\n"
    fi

    # Step 5b: Mail-wisp backlog count, observed separately from reapable wisps.
    get_sql_count "$DB" "open mail wisp" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
        AND issue_type = 'message'
    "
    MAIL_WISPS=$SQL_COUNT_RESULT
    TOTAL_MAIL_WISPS=$((TOTAL_MAIL_WISPS + MAIL_WISPS))

    if [ "$MAIL_ALERT_THRESHOLD" -gt 0 ] && [ "$MAIL_WISPS" -gt "$MAIL_ALERT_THRESHOLD" ]; then
        ANOMALIES="${ANOMALIES}$DB: $MAIL_WISPS open mail-wisps (mail threshold: $MAIL_ALERT_THRESHOLD)\n"
    fi

    # Commit Dolt changes. Must use CALL (not SELECT) and have an active
    # database via USE so CALL DOLT_COMMIT(...) runs in the target database.
    # Commit failures are surfaced as anomalies so the dog loop does not
    # silently retry forever.
    if [ -z "$DRY_RUN" ] && [ "$DB_MUTATIONS" -gt 0 ]; then
        if ! COMMIT_OUTPUT=$(dolt_sql -q "
            USE \`$DB\`;
            CALL DOLT_COMMIT('-Am', 'reaper: stale_wisps=$STALE_WISP_COUNT closed_wisps=$DB_CLOSED_WISPS purged=$DB_PURGED stale_issues=$DB_ISSUES_CLOSED expired_issues=$DB_EXPIRED_ISSUES_CLOSED', '--author', 'reaper <reaper@gastown.local>')
        " 2>&1); then
            case "$COMMIT_OUTPUT" in
                *"nothing to commit"*|*"Nothing to commit"*)
                    :
                    ;;
                *)
                    record_anomaly "$DB" "Dolt commit failed for $DB: $(sanitize_output "$COMMIT_OUTPUT")"
                    ;;
            esac
        fi
    fi
done <<EOF
$DATABASES
EOF

# Step 6: prune closed gm session beads from the city's primary bead store.
if [ -d "$CITY_BEADS_DIR" ] && command -v bd >/dev/null 2>&1; then
    SESSION_PRUNE_ATTEMPTED=1
    BD_PRUNE_ARGS=(prune --pattern 'gm-*' --older-than "$SESSION_PURGE_AGE")
    if [ -z "$DRY_RUN" ]; then
        BD_PRUNE_ARGS+=(--force)
    fi
    BD_PRUNE_ARGS+=(--json)

    if PRUNE_JSON=$((
        cd "$CITY_ABS" && BEADS_DIR="$CITY_BEADS_DIR" bd "${BD_PRUNE_ARGS[@]}"
    ) 2>/dev/null); then
        :
    else
        PRUNE_JSON='{"pruned_count":0}'
    fi
    PRUNE_COUNT=$(printf '%s' "$PRUNE_JSON" | sed -n 's/.*"pruned_count"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -1)
    [ -z "$PRUNE_COUNT" ] && PRUNE_COUNT=0
    TOTAL_SESSIONS_PRUNED=$PRUNE_COUNT
    if [ "$PRUNE_COUNT" -gt 1000 ]; then
        record_anomaly "gm" "$PRUNE_COUNT closed session beads pruned in one run (threshold: 1000)"
    fi
fi

if [ -d "$CITY_BEADS_DIR" ] && [ -z "$DRY_RUN" ] && command -v gc >/dev/null 2>&1; then
    SESSION_PRUNE_ATTEMPTED=1
    if SESSION_STATE_PRUNE_JSON=$((
        cd "$CITY_ABS" && BEADS_DIR="$CITY_BEADS_DIR" gc session prune --state drained --before "$SESSION_STATE_PRUNE_AGE" --json
    ) 2>&1); then
        :
    else
        record_anomaly "gm" "terminal session-state prune failed: $(sanitize_output "$SESSION_STATE_PRUNE_JSON")"
        SESSION_STATE_PRUNE_JSON='{"count":0}'
    fi
    SESSION_STATE_PRUNE_COUNT=$(printf '%s' "$SESSION_STATE_PRUNE_JSON" | sed -n 's/.*"count"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -1)
    [ -z "$SESSION_STATE_PRUNE_COUNT" ] && SESSION_STATE_PRUNE_COUNT=0
    TOTAL_SESSIONS_PRUNED=$((TOTAL_SESSIONS_PRUNED + SESSION_STATE_PRUNE_COUNT))
    if [ "$SESSION_STATE_PRUNE_COUNT" -gt 1000 ]; then
        record_anomaly "gm" "$SESSION_STATE_PRUNE_COUNT terminal session-state beads pruned in one run (threshold: 1000)"
    fi
fi

if [ "$HAD_DATABASES" -eq 0 ] && [ "$SESSION_PRUNE_ATTEMPTED" -eq 0 ]; then
    exit 0
fi

# Report.
if [ -n "$ANOMALIES" ]; then
    gc mail send mayor/ -s "ESCALATION: Reaper anomalies detected [MEDIUM]" \
        -m "$ANOMALIES" 2>/dev/null || true
fi

SUMMARY="reaper — stale_wisps:$TOTAL_STALE_WISPS, closed_wisps:$TOTAL_CLOSED_WISPS, purged:$TOTAL_PURGED, sessions-pruned:$TOTAL_SESSIONS_PRUNED, closed:$TOTAL_ISSUES_CLOSED, expired:$TOTAL_EXPIRED_ISSUES_CLOSED, expired_skipped:$TOTAL_EXPIRED_ISSUES_SKIPPED, skipped_non_city_issues:$TOTAL_STALE_ISSUES_SKIPPED, mail_wisps:$TOTAL_MAIL_WISPS"
if [ -n "$DRY_RUN" ]; then
    SUMMARY="$SUMMARY, would_close_wisps:$TOTAL_WOULD_CLOSE_WISPS, would_expire:$TOTAL_WOULD_EXPIRE (dry run)"
fi

gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "reaper: $SUMMARY"
