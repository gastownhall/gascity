#!/usr/bin/env bash
# Test: reaper Step 6 bd-prune type-scope guard (ga-t832q4.2)
#
# Acceptance criteria:
#   1. Mixed population (non-session beads within scope) → prune NOT invoked,
#      anomaly recorded, non-session beads survive.
#   2. Session-only population (count=0)                 → prune IS invoked normally
#      (mayor's primary ask: a gm-prefixed city keeps its non-session beads, but
#      still reaps session beads -- this must not regress into a no-op).
#   3. City database unresolved                          → guard cannot verify
#      type scope, but bd's own prune proceeds unverified; anomaly recorded
#      (matching every other CITY_DB-unresolved branch elsewhere in this file;
#      blocking prune here would regress the zero-Dolt-databases contract).
#   4. Backup-age gate already skipping                   → guard's own SQL count is
#      not run at all (short-circuit; only the backup-age anomaly fires).
#   5. SESSION_BEAD_PATTERN contains SQL metacharacters (e.g. a single quote)
#      → rejected before the type-scope guard builds any SQL text: no
#      get_sql_count call (pattern never reaches dolt_sql), prune skipped,
#      anomaly recorded.
#   6. The guard's own count cannot be computed (get_sql_count hit one of its
#      failure paths: it records an anomaly, zeroes SQL_COUNT_RESULT and
#      returns 0) → prune skipped, anomaly recorded. A transient Dolt fault
#      must not read as "scope is clear".

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REAPER="$SCRIPT_DIR/../internal/bootstrap/packs/core/assets/scripts/reaper.sh"
FAILED=0

pass() { printf '\033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }

if [ ! -f "$REAPER" ]; then
    printf 'ERROR: reaper.sh not found at %s\n' "$REAPER" >&2
    exit 1
fi

STEP6=$(awk '
  /^# Step 6:/{found=1; depth=0}
  found && /^if[[:space:]]/{depth++}
  found{
    print
    if(/^fi$/) {
      depth--
      if(depth<=0) {found=0; exit}
    }
  }
' "$REAPER")

# run_step6 <sql_count> <city_db> <backup_fresh> [pattern]
#
# sql_count: the count get_sql_count reports, or the literal "fail" to make the
# stub reproduce get_sql_count's real failure shape -- it appends to ANOMALIES
# (every failure path calls record_anomaly, which does that) and leaves
# SQL_COUNT_RESULT at 0, exactly as the production helper does.
# Returns: <bd_called>|<anomaly_called>|<anomaly_count>|<query_seen>|<anomaly_msg>
run_step6() {
    local sql_count="$1"
    local city_db="$2"
    local backup_fresh="${3:-fresh}"
    local pattern="${4:-gm-*}"
    local tmpdir bd_flag anomaly_flag anomaly_msg_file query_file step6_file run_script
    local pattern_escaped

    tmpdir=$(mktemp -d)
    bd_flag="$tmpdir/bd_called"
    anomaly_flag="$tmpdir/anomaly_called"
    anomaly_msg_file="$tmpdir/anomaly_msg"
    query_file="$tmpdir/query_seen"
    step6_file="$tmpdir/step6.sh"
    run_script="$tmpdir/run.sh"

    mkdir -p "$tmpdir/.beads/backup"
    if [ "$backup_fresh" = "fresh" ]; then
        _NOW_TS=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
        printf '{"last_dolt_commit":"test","timestamp":"%s"}\n' "$_NOW_TS" \
            > "$tmpdir/.beads/backup/backup_state.json"
    fi

    printf '%s\n' "$STEP6" > "$step6_file"

    # Single-quote-escape the pattern for safe embedding as a literal bash
    # assignment below -- the pattern itself may contain quotes (that is
    # exactly what T5 exercises) and must reach SESSION_BEAD_PATTERN intact,
    # not be allowed to break out of the generated script's own syntax.
    pattern_escaped=$(printf '%s' "$pattern" | sed "s/'/'\\\\''/g")

    # get_sql_count stub: the success shape by default, or the production
    # helper's failure shape (anomaly appended, result left at 0) for "fail".
    local sql_count_stub
    if [ "$sql_count" = "fail" ]; then
        sql_count_stub="get_sql_count() { printf '%s\n' \"\$3\" >> '$query_file'; ANOMALIES=\"\${ANOMALIES:-}type scope guard count failed for test_db: connection refused
\"; SQL_COUNT_RESULT=0; }"
    else
        sql_count_stub="get_sql_count() { printf '%s\n' \"\$3\" >> '$query_file'; SQL_COUNT_RESULT='$sql_count'; }"
    fi

    # NB: heredoc terminator must be at column 0; variables below are expanded by
    # the outer shell when writing the script (intentional), except \$* / \$3
    # which we want runtime-expanded inside the generated stub functions.
    cat > "$run_script" << RUNEOF
#!/usr/bin/env bash
set -euo pipefail
gc()            { touch '$bd_flag'; printf '{"pruned_count":3}'; }
record_anomaly(){ touch '$anomaly_flag'; printf '%s\n' "\$*" >> '$anomaly_msg_file'; }
$sql_count_stub
export -f gc record_anomaly get_sql_count
CITY_ABS='$tmpdir'
CITY_BEADS_DIR='$tmpdir/.beads'
SESSION_BEAD_PATTERN='$pattern_escaped'
SESSION_PURGE_AGE='720h'
DRY_RUN=''
TOTAL_SESSIONS_PRUNED=0
SESSION_PRUNE_ATTEMPTED=0
CITY_DB='$city_db'
GC_BACKUP_MAX_AGE_FOR_BULK_DELETE=86400
. '$step6_file'
RUNEOF

    local rc=0
    bash "$run_script" 2>/dev/null || rc=$?

    local bd_result anomaly_result anomaly_count anomaly_msg_val query_val
    bd_result=$([ -f "$bd_flag" ] && echo yes || echo no)
    anomaly_result=$([ -f "$anomaly_flag" ] && echo yes || echo no)
    anomaly_count=$([ -f "$anomaly_msg_file" ] && wc -l < "$anomaly_msg_file" || echo 0)
    anomaly_msg_val=$([ -f "$anomaly_msg_file" ] && tr '\n' ';' < "$anomaly_msg_file" || echo "")
    query_val=$([ -f "$query_file" ] && tr '\n' ';' < "$query_file" || echo "")
    rm -rf "$tmpdir"
    printf '%s|%s|%s|%s|%s\n' "$bd_result" "$anomaly_result" "$anomaly_count" "$query_val" "$anomaly_msg_val"
}

# T1: mixed population (3 non-session beads in scope) → prune skipped
result=$(run_step6 "3" "test_db" "fresh")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f5-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ] \
        && printf '%s' "$anomaly_msg" | grep -qi "scope\|type"; then
    pass "T1: non-session beads in scope (count=3) → prune skipped, anomaly recorded"
else
    fail "T1: non-session beads in scope (count=3) → expected bd=no anomaly=yes+scope keyword; got bd=$bd_called anomaly=$anomaly_called msg=$anomaly_msg"
fi

# T2: session-only population (count=0) → prune proceeds normally
result=$(run_step6 "0" "test_db" "fresh")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
if [ "$bd_called" = "yes" ] && [ "$anomaly_called" = "no" ]; then
    pass "T2: session-only population (count=0) → prune proceeds, no anomaly"
else
    fail "T2: session-only population (count=0) → expected bd=yes anomaly=no; got bd=$bd_called anomaly=$anomaly_called"
fi

# T3: city database unresolved → type-scope guard cannot verify, but bd's own
# prune proceeds unverified (bd resolves its own store independently of this
# file's direct-SQL CITY_DB; failing prune closed here would regress
# TestReaperSessionPruneRunsWhenNoDoltDatabases's established contract).
result=$(run_step6 "0" "" "fresh")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f5-)
if [ "$bd_called" = "yes" ] && [ "$anomaly_called" = "yes" ] \
        && printf '%s' "$anomaly_msg" | grep -qi "database"; then
    pass "T3: city database unresolved → type-scope guard skipped (unverifiable), bd prune still proceeds, anomaly recorded"
else
    fail "T3: city database unresolved → expected bd=yes anomaly=yes+database keyword; got bd=$bd_called anomaly=$anomaly_called msg=$anomaly_msg"
fi

# T4: backup-age gate already stale → guard's own SQL count never runs
result=$(run_step6 "3" "test_db" "stale")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_count=$(printf '%s' "$result" | cut -d'|' -f3)
query_seen=$(printf '%s' "$result" | cut -d'|' -f4)
if [ "$bd_called" = "no" ] && [ "$anomaly_count" -eq 1 ] && [ -z "$query_seen" ]; then
    pass "T4: backup-age gate already stale → type-scope count never runs, single anomaly"
else
    fail "T4: backup-age gate already stale → expected bd=no anomaly_count=1 query=unset; got bd=$bd_called anomaly_count=$anomaly_count query=$query_seen"
fi

# T5: SESSION_BEAD_PATTERN contains a single quote (SQL injection attempt) →
# rejected before any SQL is built; prune skipped, no get_sql_count call,
# anomaly recorded (ga-t832q4.2 round 2: the type-scope guard's own LIKE
# query previously spliced this value in unsanitized).
result=$(run_step6 "0" "test_db" "fresh" "x' OR '1'='1")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
query_seen=$(printf '%s' "$result" | cut -d'|' -f4)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f5-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ] && [ -z "$query_seen" ] \
        && printf '%s' "$anomaly_msg" | grep -qi "pattern"; then
    pass "T5: SESSION_BEAD_PATTERN with single quote → rejected before SQL built, prune skipped, anomaly recorded"
else
    fail "T5: SESSION_BEAD_PATTERN with single quote → expected bd=no anomaly=yes+pattern keyword query=unset; got bd=$bd_called anomaly=$anomaly_called query=$query_seen msg=$anomaly_msg"
fi

# T6: the guard's own count cannot be computed (get_sql_count hit a failure
# path: anomaly recorded, SQL_COUNT_RESULT left at 0) → prune skipped. Every
# other harness stubs get_sql_count as an always-successful assignment, so
# nothing else in this suite can reach the helper's real failure shape --
# which is how the fail-open survived review: a transient Dolt fault is
# otherwise indistinguishable from "scope is clear" and lets --force through.
result=$(run_step6 "fail" "test_db" "fresh")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f5-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ] \
        && printf '%s' "$anomaly_msg" | grep -qi "scope\|type"; then
    pass "T6: type-scope count uncomputable (get_sql_count failed) → prune skipped, anomaly recorded"
else
    fail "T6: type-scope count uncomputable → expected bd=no anomaly=yes+scope keyword; got bd=$bd_called anomaly=$anomaly_called msg=$anomaly_msg"
fi

[ "$FAILED" -eq 0 ] && exit 0 || exit 1
