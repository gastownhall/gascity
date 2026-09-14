#!/usr/bin/env bash
# Test: reaper Step 6 bd-prune type-scope guard (ga-t832q4.2)
#
# Acceptance criteria:
#   1. Mixed population (non-session beads within scope) → prune NOT invoked,
#      anomaly recorded, non-session beads survive.
#   2. Session-only population (count=0)                 → prune IS invoked normally
#      (mayor's primary ask: a gm-prefixed city keeps its non-session beads, but
#      still reaps session beads -- this must not regress into a no-op).
#   3. City database unresolved                          → prune NOT invoked
#      (fail closed), anomaly recorded.
#   4. Backup-age gate already skipping                   → guard's own SQL count is
#      not run at all (short-circuit; only the backup-age anomaly fires).

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

# run_step6 <sql_count> <city_db> <backup_fresh>
# Returns: <bd_called>|<anomaly_called>|<anomaly_count>|<query_seen>|<anomaly_msg>
run_step6() {
    local sql_count="$1"
    local city_db="$2"
    local backup_fresh="${3:-fresh}"
    local tmpdir bd_flag anomaly_flag anomaly_msg_file query_file step6_file run_script

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

    # NB: heredoc terminator must be at column 0; variables below are expanded by
    # the outer shell when writing the script (intentional), except \$* / \$3
    # which we want runtime-expanded inside the generated stub functions.
    cat > "$run_script" << RUNEOF
#!/usr/bin/env bash
set -euo pipefail
gc()            { touch '$bd_flag'; printf '{"pruned_count":3}'; }
record_anomaly(){ touch '$anomaly_flag'; printf '%s\n' "\$*" >> '$anomaly_msg_file'; }
get_sql_count() { printf '%s\n' "\$3" >> '$query_file'; SQL_COUNT_RESULT='$sql_count'; }
export -f gc record_anomaly get_sql_count
CITY_ABS='$tmpdir'
CITY_BEADS_DIR='$tmpdir/.beads'
SESSION_BEAD_PATTERN='gm-*'
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

# T3: city database unresolved → fail closed, prune skipped
result=$(run_step6 "0" "" "fresh")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f5-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ] \
        && printf '%s' "$anomaly_msg" | grep -qi "database"; then
    pass "T3: city database unresolved → prune skipped (fail closed), anomaly recorded"
else
    fail "T3: city database unresolved → expected bd=no anomaly=yes+database keyword; got bd=$bd_called anomaly=$anomaly_called msg=$anomaly_msg"
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

[ "$FAILED" -eq 0 ] && exit 0 || exit 1
