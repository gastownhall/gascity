#!/usr/bin/env bash
# reaper — close stale wisps with closed parents, purge old closed data, auto-close stale issues.
#
# Replaces mol-dog-reaper formula. All operations are deterministic:
# SQL queries with age thresholds, bd close/update commands, count
# comparisons against alert thresholds.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/dolt-target.sh"
CITY_ABS="$(cd "$CITY" 2>/dev/null && pwd -P || printf '%s\n' "$CITY")"
CITY_BEADS_DIR="$CITY_ABS/.beads"

# Configurable thresholds.
MAX_AGE="${GC_REAPER_MAX_AGE:-24h}"
PURGE_AGE="${GC_REAPER_PURGE_AGE:-168h}"
STALE_ISSUE_AGE="${GC_REAPER_STALE_ISSUE_AGE:-720h}"
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
        information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe|benchdb|testdb_*|beads_pt*|beads_vr*|doctest_*|doctortest_*)
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
# beads_vr*, doctest_*, doctortest_* — matching the Go cleanup planner
# contract); the remainder are bead stores.
DATABASES=$(
    while IFS= read -r db; do
        if is_user_database "$db"; then
            printf '%s\n' "$db"
        fi
    done < <(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2)
)
if [ -z "$DATABASES" ]; then
    # No databases accessible — nothing to do.
    exit 0
fi

TOTAL_STALE_WISPS=0
TOTAL_CLOSED_WISPS=0
TOTAL_PURGED=0
TOTAL_ISSUES_CLOSED=0
TOTAL_STALE_ISSUES_SKIPPED=0
ANOMALIES=""

sanitize_output() {
    printf '%s' "$1" | tr '\n' ' ' | cut -c1-500
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
        record_anomaly "$db" "$label count failed for $db: $(sanitize_output "$output $stderr_output")"
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
        record_anomaly "$db" "$label query failed for $db: $(sanitize_output "$output $stderr_output")"
        return 0
    fi
    rm -f "$stderr_file"

    SQL_ROWS_RESULT=$(printf '%s\n' "$output" | tail -n +2 | tr -d '\r')
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
    if ! output=$(dolt_sql -r csv -q "
$query;
SELECT ROW_COUNT();
    " 2>"$stderr_file"); then
        stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
        rm -f "$stderr_file"
        record_anomaly "$db" "$label failed for $db: $(sanitize_output "$output $stderr_output")"
        return 1
    fi
    stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
    rm -f "$stderr_file"

    rows=$(printf '%s\n' "$output" | tail -1 | tr -d '\r')
    if [ -z "$rows" ] || ! [[ "$rows" =~ ^[0-9]+$ ]]; then
        record_anomaly "$db" "$label returned non-numeric row count for $db: $(sanitize_output "$output $stderr_output")"
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

    DB_MUTATIONS=0

    # Step 1: Count stale non-closed wisps, then close only candidates whose
    # explicit parent-child edge points to a closed parent. Wisps
    # without a parent edge are reported but not closed by age alone.
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
            INNER JOIN \`$DB\`.dependencies d
                ON d.issue_id = w.id
                AND d.type = 'parent-child'
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
        if [ "$CLOSE_WISP_BATCH" -eq 0 ] || [ -n "$DRY_RUN" ]; then
            break
        fi

        if run_sql_change "$DB" "closing stale wisps" "
            UPDATE \`$DB\`.wisps SET status='closed', closed_at=NOW()
            WHERE status IN ('open', 'hooked', 'in_progress')
            AND created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            AND id IN (
                SELECT id FROM (
                    SELECT w.id FROM \`$DB\`.wisps w
                    INNER JOIN \`$DB\`.dependencies d
                        ON d.issue_id = w.id
                        AND d.type = 'parent-child'
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
            SELECT DISTINCT d.depends_on_id FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.wisps child_wisp ON d.issue_id = child_wisp.id
            WHERE d.type = 'parent-child'
            AND d.depends_on_id IS NOT NULL
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
                SELECT DISTINCT d.depends_on_id FROM \`$DB\`.dependencies d
                INNER JOIN \`$DB\`.wisps child_wisp ON d.issue_id = child_wisp.id
                WHERE d.type = 'parent-child'
                AND d.depends_on_id IS NOT NULL
                AND child_wisp.status IN ('open', 'hooked', 'in_progress')
            )
        "; then
            PURGED_ROWS=$SQL_CHANGE_ROWS_RESULT
            DB_PURGED=$((DB_PURGED + PURGED_ROWS))
            TOTAL_PURGED=$((TOTAL_PURGED + PURGED_ROWS))
            DB_MUTATIONS=$((DB_MUTATIONS + PURGED_ROWS))
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

    # Step 5: Anomaly check — open wisp count.
    get_sql_count "$DB" "open wisp" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
    "
    OPEN_WISPS=$SQL_COUNT_RESULT

    if [ "$OPEN_WISPS" -gt "$ALERT_THRESHOLD" ]; then
        ANOMALIES="${ANOMALIES}$DB: $OPEN_WISPS open wisps (threshold: $ALERT_THRESHOLD)\n"
    fi

    # Commit Dolt changes. Must use CALL (not SELECT) and have an active
    # database via USE so CALL DOLT_COMMIT(...) runs in the target database.
    # Commit failures are surfaced as anomalies so the dog loop does not
    # silently retry forever.
    if [ -z "$DRY_RUN" ] && [ "$DB_MUTATIONS" -gt 0 ]; then
        if ! COMMIT_OUTPUT=$(dolt_sql -q "
            USE \`$DB\`;
            CALL DOLT_COMMIT('-Am', 'reaper: stale_wisps=$STALE_WISP_COUNT closed_wisps=$DB_CLOSED_WISPS purged=$DB_PURGED stale_issues=$DB_ISSUES_CLOSED', '--author', 'reaper <reaper@gastown.local>')
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

# Report.
if [ -n "$ANOMALIES" ]; then
    gc mail send mayor/ -s "ESCALATION: Reaper anomalies detected [MEDIUM]" \
        -m "$ANOMALIES" 2>/dev/null || true
fi

SUMMARY="reaper — stale_wisps:$TOTAL_STALE_WISPS, closed_wisps:$TOTAL_CLOSED_WISPS, purged:$TOTAL_PURGED, closed:$TOTAL_ISSUES_CLOSED, skipped_non_city_issues:$TOTAL_STALE_ISSUES_SKIPPED"
if [ -n "$DRY_RUN" ]; then
    SUMMARY="$SUMMARY (dry run)"
fi

gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "reaper: $SUMMARY"
