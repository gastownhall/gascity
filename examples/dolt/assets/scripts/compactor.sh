#!/usr/bin/env bash
# compactor — flatten Dolt commit history on production bead-store databases.
#
# Replaces mol-dog-compactor formula's "ZFC-exempt, no daemon" stub. All
# operations are deterministic: SQL queries to count/inspect, soft-reset
# to root, single commit-all, integrity verify, dolt_gc reclaim.
#
# Runs as an exec order (no LLM, no agent, no wisp). Mirrors the
# implementation pattern of reaper.sh / jsonl-export.sh in the
# maintenance pack.
#
# Algorithm per database:
#   1. Pre-flight: row counts for every user table, current HEAD hash
#   2. Backup: create compact-backup-<epoch> branch at current HEAD
#   3. Find root commit (earliest ancestor on main)
#   4. Soft reset main to root: CALL DOLT_RESET('--soft', '<root>')
#   5. Commit-all: CALL DOLT_COMMIT('-Am', 'compaction: flatten history')
#   6. Post-flight: re-count rows; compare to pre-flight
#   7. On mismatch: hard-reset main to backup branch (rollback), escalate
#   8. On match: drop the backup branch
#   9. After all databases: CALL dolt_gc() to reclaim unreferenced chunks
#
# Concurrent-write safety (flatten mode): merge base shifts but data is
# preserved because soft-reset keeps working tree intact and DOLT_COMMIT
# captures everything as a single new commit. The backup branch is the
# rollback anchor; if anything goes wrong, the prior HEAD is recoverable.

set -euo pipefail

CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
CITY_ABS="$(cd "$CITY" 2>/dev/null && pwd -P || printf '%s\n' "$CITY")"
CITY_BEADS_DIR="$CITY_ABS/.beads"

# Configurable (env > order vars > defaults).
COMMIT_THRESHOLD="${GC_COMPACTOR_COMMIT_THRESHOLD:-500}"
MODE="${GC_COMPACTOR_MODE:-flatten}"
DATABASES_OVERRIDE="${GC_COMPACTOR_DATABASES:-}"
DRY_RUN="${GC_COMPACTOR_DRY_RUN:-}"
SKIP_GC="${GC_COMPACTOR_SKIP_GC:-}"
DOLT_HOST="${GC_DOLT_HOST:-127.0.0.1}"
DOLT_USER="${GC_DOLT_USER:-root}"
DOLT_PASSWORD="${GC_DOLT_PASSWORD:-}"

# Port resolution: prefer GC_DOLT_PORT from env, otherwise read from the
# managed dolt runtime state file (deterministic per-city port). Mirrors
# the logic in maintenance/assets/scripts/dolt-target.sh; replicated here
# rather than sourced because compactor.sh ships in a different pack.
# The previous hardcoded default (17360) almost never matched the actual
# managed port and would silently misroute against an unrelated server.
resolve_dolt_port() {
    local state_file="$1"
    [ -f "$state_file" ] || return 0
    local running pid port data_dir
    running=$(sed -n 's/.*"running"[[:space:]]*:[[:space:]]*\([^,}[:space:]]*\).*/\1/p' "$state_file" 2>/dev/null | head -1)
    pid=$(sed -n 's/.*"pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$state_file" 2>/dev/null | head -1)
    port=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$state_file" 2>/dev/null | head -1)
    [ "$running" = "true" ] || return 0
    [ -n "$pid" ] || return 0
    [ -n "$port" ] || return 0
    if kill -0 "$pid" 2>/dev/null; then
        printf '%s\n' "$port"
    fi
}

if [ -z "${GC_DOLT_PORT:-}" ]; then
    DOLT_STATE_FILE="${GC_DOLT_STATE_FILE:-${GC_CITY_RUNTIME_DIR:-$CITY_ABS/.gc/runtime}/packs/dolt/dolt-state.json}"
    GC_DOLT_PORT="$(resolve_dolt_port "$DOLT_STATE_FILE" || true)"
fi
: "${GC_DOLT_PORT:=3307}"

case "$GC_DOLT_PORT" in
    ''|*[!0-9]*)
        printf 'compactor: invalid GC_DOLT_PORT: %q\n' "$GC_DOLT_PORT" >&2
        exit 1
        ;;
esac

DOLT_PORT="$GC_DOLT_PORT"

if [ "$MODE" != "flatten" ]; then
    printf 'compactor: mode %q is not yet implemented; only "flatten" is supported.\n' "$MODE" >&2
    exit 2
fi

# dolt_sql — execute a SQL statement against the running dolt sql-server.
# Mirrors the helper in maintenance/assets/scripts/dolt-target.sh: uses
# `dolt --host ... --port ... --user ... --no-tls sql ...` (global flags
# before the `sql` subcommand). Inlined here so this script depends only
# on the bundled dolt CLI.
dolt_sql() {
    DOLT_CLI_PASSWORD="$DOLT_PASSWORD" dolt \
        --host "$DOLT_HOST" \
        --port "$DOLT_PORT" \
        --user "$DOLT_USER" \
        --no-tls \
        sql "$@"
}

dolt_q() {
    # dolt_q "<sql>" — return rows as CSV without the header.
    dolt_sql -r csv -q "$1" 2>/dev/null | tail -n +2
}

dolt_q_one() {
    # dolt_q_one "<sql>" — return single scalar from row 1, col 1.
    dolt_q "$1" | head -1 | tr -d '\r'
}

# valid_database_identifier — guard before interpolating any database
# name into SQL. Mirrors reaper.sh:131-141. Without this, a name from
# SHOW DATABASES (server-controlled) or GC_COMPACTOR_DATABASES (operator
# env, medium risk) containing a backtick / apostrophe / semicolon
# could escape the backtick-quoted SQL context and corrupt or inject
# into DOLT_RESET / DOLT_COMMIT / DOLT_BRANCH calls.
valid_database_identifier() {
    local name="$1"
    case "$name" in
        ''|-*|*[!A-Za-z0-9_-]*)
            return 1
            ;;
    esac
    return 0
}

# is_user_database — filter out dolt/MySQL system DBs and known scratch
# patterns. Mirrors the equivalent helper in reaper.sh.
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

# Database discovery.
if [ -n "$DATABASES_OVERRIDE" ]; then
    DATABASES="$(printf '%s\n' "$DATABASES_OVERRIDE" | tr ',' '\n' | sed 's/^ *//;s/ *$//' | grep -v '^$' || true)"
else
    DATABASES="$(
        while IFS= read -r db; do
            db="${db//\"/}"
            db="$(printf '%s' "$db" | tr -d '\r' | sed 's/^ *//;s/ *$//')"
            [ -z "$db" ] && continue
            if is_user_database "$db"; then
                printf '%s\n' "$db"
            fi
        done < <(dolt_q "SHOW DATABASES")
    )"
fi

if [ -z "$DATABASES" ]; then
    printf 'compactor: no databases to inspect; exiting cleanly.\n'
    exit 0
fi

# Counters for the final report.
TOTAL_INSPECTED=0
TOTAL_SKIPPED=0
TOTAL_COMPACTED=0
TOTAL_FAILED=0
ANOMALIES=""

record_anomaly() {
    local db="$1"
    shift
    ANOMALIES="${ANOMALIES}$db: $*
"
}

# user_tables_for — list non-system tables in $1.
user_tables_for() {
    local db="$1"
    dolt_q "SELECT table_name FROM information_schema.tables WHERE table_schema='$db' AND table_name NOT LIKE 'dolt\\_%' AND table_type='BASE TABLE' ORDER BY table_name" | tr -d '\r'
}

# count_rows — return CSV "table,count" for every user table in $1.
count_rows() {
    local db="$1"
    local table count
    while IFS= read -r table; do
        [ -z "$table" ] && continue
        count="$(dolt_q_one "SELECT COUNT(*) FROM \`$db\`.\`$table\`")"
        if [ -z "$count" ]; then
            count="?"
        fi
        printf '%s,%s\n' "$table" "$count"
    done < <(user_tables_for "$db")
}

# count_commits — return commit count for $1, capped at 10000 (LIMIT keeps
# the query fast for very large graphs; we only need to know if it's over
# the threshold).
count_commits() {
    local db="$1"
    dolt_q_one "SELECT COUNT(*) FROM (SELECT 1 FROM \`$db\`.dolt_log LIMIT 10000) AS t"
}

# find_root_commit — earliest ancestor on the current branch (the commit
# at the bottom of dolt_log when ordered by date asc).
find_root_commit() {
    local db="$1"
    dolt_q_one "SELECT commit_hash FROM \`$db\`.dolt_log ORDER BY date ASC, commit_hash ASC LIMIT 1"
}

current_head() {
    local db="$1"
    dolt_q_one "SELECT commit_hash FROM \`$db\`.dolt_log ORDER BY date DESC, commit_hash DESC LIMIT 1"
}

backup_branch_name() {
    local epoch
    epoch="$(date +%s)"
    printf 'compact-backup-%s' "$epoch"
}

create_backup() {
    local db="$1" branch="$2" head="$3"
    dolt_sql -q "USE \`$db\`; CALL DOLT_BRANCH('$branch', '$head')" >/dev/null
}

drop_backup() {
    local db="$1" branch="$2"
    dolt_sql -q "USE \`$db\`; CALL DOLT_BRANCH('-D', '$branch')" >/dev/null 2>&1 || true
}

rollback_to_backup() {
    local db="$1" branch="$2"
    dolt_sql -q "USE \`$db\`; CALL DOLT_RESET('--hard', '$branch')" >/dev/null
}

flatten_db() {
    local db="$1"
    local pre_head pre_counts post_counts root backup
    local commit_count

    if ! valid_database_identifier "$db"; then
        record_anomaly "$db" "rejected: not a valid identifier (alnum/_/- only); refusing to interpolate into SQL"
        TOTAL_FAILED=$((TOTAL_FAILED + 1))
        return 0
    fi

    commit_count="$(count_commits "$db")"
    if [ -z "$commit_count" ] || ! [[ "$commit_count" =~ ^[0-9]+$ ]]; then
        record_anomaly "$db" "could not read commit count; skipping"
        TOTAL_SKIPPED=$((TOTAL_SKIPPED + 1))
        return 0
    fi

    if [ "$commit_count" -lt "$COMMIT_THRESHOLD" ]; then
        printf '  [SKIP] %s — commits=%s (below threshold %s)\n' "$db" "$commit_count" "$COMMIT_THRESHOLD"
        TOTAL_SKIPPED=$((TOTAL_SKIPPED + 1))
        return 0
    fi

    pre_head="$(current_head "$db")"
    if [ -z "$pre_head" ]; then
        record_anomaly "$db" "could not read HEAD; skipping"
        TOTAL_SKIPPED=$((TOTAL_SKIPPED + 1))
        return 0
    fi

    pre_counts="$(count_rows "$db")"

    if [ -n "$DRY_RUN" ]; then
        printf '  [DRY-RUN] %s — would compact (commits=%s -> 1)\n' "$db" "$commit_count"
        TOTAL_SKIPPED=$((TOTAL_SKIPPED + 1))
        return 0
    fi

    root="$(find_root_commit "$db")"
    if [ -z "$root" ]; then
        record_anomaly "$db" "could not locate root commit; skipping"
        TOTAL_SKIPPED=$((TOTAL_SKIPPED + 1))
        return 0
    fi

    if [ "$root" = "$pre_head" ]; then
        printf '  [SKIP] %s — already a single commit\n' "$db"
        TOTAL_SKIPPED=$((TOTAL_SKIPPED + 1))
        return 0
    fi

    backup="$(backup_branch_name)"

    printf '  [COMPACT] %s — commits=%s root=%.10s head=%.10s backup=%s\n' \
        "$db" "$commit_count" "$root" "$pre_head" "$backup"

    if ! create_backup "$db" "$backup" "$pre_head"; then
        record_anomaly "$db" "could not create backup branch $backup; skipping"
        TOTAL_FAILED=$((TOTAL_FAILED + 1))
        return 0
    fi

    # Soft reset main to root (HEAD pointer moves; working tree unchanged).
    if ! dolt_sql -q "USE \`$db\`; CALL DOLT_RESET('--soft', '$root')" >/dev/null; then
        record_anomaly "$db" "DOLT_RESET --soft failed; rolling back"
        rollback_to_backup "$db" "$backup" || true
        drop_backup "$db" "$backup"
        TOTAL_FAILED=$((TOTAL_FAILED + 1))
        return 0
    fi

    # Re-commit everything as a single compacted commit. The -A captures
    # everything the soft-reset un-staged.
    if ! dolt_sql -q "USE \`$db\`; CALL DOLT_COMMIT('-Am', 'compaction: flatten history')" >/dev/null; then
        record_anomaly "$db" "DOLT_COMMIT failed; rolling back"
        # If rollback itself fails, retain the backup branch as the only
        # remaining anchor to the prior HEAD. Dropping it unconditionally
        # would leave the database with no path back to its pre-compaction
        # state.
        if rollback_to_backup "$db" "$backup"; then
            drop_backup "$db" "$backup"
        else
            record_anomaly "$db" "rollback also failed; backup branch $backup retained as recovery anchor"
        fi
        TOTAL_FAILED=$((TOTAL_FAILED + 1))
        return 0
    fi

    post_counts="$(count_rows "$db")"
    if [ "$pre_counts" != "$post_counts" ]; then
        record_anomaly "$db" "row count mismatch (pre vs post); rolling back. pre=$(printf '%s' "$pre_counts" | tr '\n' ';') post=$(printf '%s' "$post_counts" | tr '\n' ';')"
        rollback_to_backup "$db" "$backup" || true
        drop_backup "$db" "$backup"
        TOTAL_FAILED=$((TOTAL_FAILED + 1))
        return 0
    fi

    drop_backup "$db" "$backup"
    TOTAL_COMPACTED=$((TOTAL_COMPACTED + 1))
}

# Main loop.
printf 'compactor: starting cycle (mode=%s threshold=%s)\n' "$MODE" "$COMMIT_THRESHOLD"

while IFS= read -r DB; do
    [ -z "$DB" ] && continue
    TOTAL_INSPECTED=$((TOTAL_INSPECTED + 1))
    flatten_db "$DB"
done <<EOF
$DATABASES
EOF

# Post-cycle GC. Run once across all DBs by switching into each and
# calling dolt_gc — DOLT_GC operates on the active database.
if [ -z "$SKIP_GC" ] && [ "$TOTAL_COMPACTED" -gt 0 ]; then
    while IFS= read -r DB; do
        [ -z "$DB" ] && continue
        if ! dolt_sql -q "USE \`$DB\`; CALL dolt_gc()" >/dev/null 2>&1; then
            record_anomaly "$DB" "dolt_gc failed (chunk reclaim skipped)"
        fi
    done <<EOF
$DATABASES
EOF
fi

# Report.
printf '\ncompactor: cycle complete\n'
printf '  inspected: %d\n' "$TOTAL_INSPECTED"
printf '  compacted: %d\n' "$TOTAL_COMPACTED"
printf '  skipped:   %d\n' "$TOTAL_SKIPPED"
printf '  failed:    %d\n' "$TOTAL_FAILED"
if [ -n "$ANOMALIES" ]; then
    printf 'ANOMALIES:\n%s' "$ANOMALIES"
fi

# Best-effort completion nudge to deacon. Optional — runs only if `gc`
# is on PATH and the deacon agent exists.
if command -v gc >/dev/null 2>&1; then
    msg="DOG_DONE: compactor — inspected:$TOTAL_INSPECTED, compacted:$TOTAL_COMPACTED, skipped:$TOTAL_SKIPPED, failed:$TOTAL_FAILED"
    gc session nudge deacon "$msg" >/dev/null 2>&1 || true
fi

# Surface non-zero exit if any DB hit a hard failure so order tracking
# records the cycle as failed and the operator is alerted.
if [ "$TOTAL_FAILED" -gt 0 ]; then
    exit 1
fi
