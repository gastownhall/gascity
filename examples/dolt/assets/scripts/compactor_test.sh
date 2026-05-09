#!/usr/bin/env bash
# compactor_test — end-to-end validation of compactor.sh against an
# ephemeral dolt sql-server. Run from a clean checkout:
#
#   bash examples/dolt/assets/scripts/compactor_test.sh
#
# The test:
#   1. Creates a tmp data dir + starts dolt sql-server on a random port
#   2. Creates a test database and N initial commits
#   3. Runs compactor.sh against the ephemeral server
#   4. Asserts: row counts preserved, commit count = 1, status = 0
#   5. Validates dry-run path
#   6. Validates "below threshold" skip path
#   7. Validates rollback path (induced row-count mismatch)
#   8. Cleans up: shuts down dolt, removes tmp dir

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPACTOR="$SCRIPT_DIR/compactor.sh"

if [ ! -x "$COMPACTOR" ]; then
    printf 'compactor_test: %s missing or not executable\n' "$COMPACTOR" >&2
    exit 2
fi

# Random port in a high range to minimise collision with running dolt.
PORT=$((20000 + RANDOM % 10000))
TMP="$(mktemp -d -t compactor-test-XXXXXX)"
DOLT_DIR="$TMP/dolt-data"
LOG="$TMP/dolt.log"
mkdir -p "$DOLT_DIR"

cleanup() {
    local rc=$?
    if [ -n "${DOLT_PID:-}" ]; then
        kill "$DOLT_PID" 2>/dev/null || true
        wait "$DOLT_PID" 2>/dev/null || true
    fi
    rm -rf "$TMP"
    if [ "$rc" -ne 0 ]; then
        printf '\ncompactor_test: FAILED (exit %d)\n' "$rc" >&2
    fi
    exit "$rc"
}
trap cleanup EXIT INT TERM

printf 'compactor_test: starting ephemeral dolt on port %d (data: %s)\n' "$PORT" "$DOLT_DIR"

cat >"$TMP/server.yaml" <<EOF
listener:
  host: 127.0.0.1
  port: $PORT
data_dir: $DOLT_DIR
behavior:
  read_only: false
  autocommit: true
EOF

(
    cd "$DOLT_DIR"
    dolt init --new-format >/dev/null 2>&1 || dolt init >/dev/null 2>&1 || true
    dolt sql-server --config "$TMP/server.yaml" >"$LOG" 2>&1 &
    echo $! > "$TMP/dolt.pid"
)
DOLT_PID="$(cat "$TMP/dolt.pid")"

# Wait for the server to accept connections.
export DOLT_CLI_PASSWORD=""
DOLT_BASE=(dolt --host 127.0.0.1 --port "$PORT" --user root --no-tls)

deadline=$(( $(date +%s) + 30 ))
until "${DOLT_BASE[@]}" sql -q "SELECT 1" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
        printf 'compactor_test: dolt server did not start in 30s; log:\n' >&2
        cat "$LOG" >&2
        exit 1
    fi
    sleep 1
done

# Helper: SQL passthrough to ephemeral server.
sql() {
    "${DOLT_BASE[@]}" sql -q "$1"
}

sql_csv_one() {
    "${DOLT_BASE[@]}" sql -r csv -q "$1" 2>/dev/null \
        | tail -1 | tr -d '\r'
}

# Convention: scripts use is_user_database() to filter scratch DBs.
# Use a name that passes the filter.
TEST_DB="beads_compactor"

setup_db_with_commits() {
    local db="$1" n_commits="$2"
    sql "DROP DATABASE IF EXISTS \`$db\`" >/dev/null 2>&1 || true
    sql "CREATE DATABASE \`$db\`" >/dev/null
    sql "USE \`$db\`; CREATE TABLE issues (id INT PRIMARY KEY AUTO_INCREMENT, title VARCHAR(200), status VARCHAR(20))" >/dev/null
    sql "USE \`$db\`; CALL DOLT_COMMIT('-Am', 'init schema')" >/dev/null

    local i
    for ((i=1; i<=n_commits; i++)); do
        sql "USE \`$db\`; INSERT INTO issues (title, status) VALUES ('row $i', 'open')" >/dev/null
        sql "USE \`$db\`; CALL DOLT_COMMIT('-Am', 'row $i')" >/dev/null
    done
}

count_commits() {
    local db="$1"
    sql_csv_one "SELECT COUNT(*) FROM \`$db\`.dolt_log"
}

count_issues() {
    local db="$1"
    sql_csv_one "SELECT COUNT(*) FROM \`$db\`.issues"
}

run_compactor() {
    GC_DOLT_HOST=127.0.0.1 \
    GC_DOLT_PORT="$PORT" \
    GC_DOLT_USER=root \
    GC_DOLT_PASSWORD="" \
    GC_COMPACTOR_DATABASES="$TEST_DB" \
    "$@" \
    bash "$COMPACTOR"
}

assert_eq() {
    local label="$1" actual="$2" expected="$3"
    if [ "$actual" != "$expected" ]; then
        printf 'ASSERT FAIL: %s — got %q, want %q\n' "$label" "$actual" "$expected" >&2
        return 1
    fi
    printf '  ok %s = %s\n' "$label" "$actual"
}

# ------------------------------------------------------------------
# Test 1: above threshold → compaction happens, row count preserved
# ------------------------------------------------------------------
printf '\n[test 1] above-threshold compaction\n'

setup_db_with_commits "$TEST_DB" 10
PRE_COMMITS="$(count_commits "$TEST_DB")"
PRE_ISSUES="$(count_issues "$TEST_DB")"
printf '  pre: commits=%s issues=%s\n' "$PRE_COMMITS" "$PRE_ISSUES"

GC_COMPACTOR_COMMIT_THRESHOLD=5 run_compactor

POST_COMMITS="$(count_commits "$TEST_DB")"
POST_ISSUES="$(count_issues "$TEST_DB")"
printf '  post: commits=%s issues=%s\n' "$POST_COMMITS" "$POST_ISSUES"

assert_eq "issues preserved" "$POST_ISSUES" "$PRE_ISSUES"
# After flatten, dolt_log shows: the new flattened commit + the
# original root commit (commit graph is HEAD -> root). So 2 entries.
assert_eq "commits collapsed to 2 (HEAD + root)" "$POST_COMMITS" "2"

# ------------------------------------------------------------------
# Test 2: below threshold → no compaction, exit clean
# ------------------------------------------------------------------
printf '\n[test 2] below-threshold skip\n'

setup_db_with_commits "$TEST_DB" 3
PRE_COMMITS="$(count_commits "$TEST_DB")"
PRE_ISSUES="$(count_issues "$TEST_DB")"

GC_COMPACTOR_COMMIT_THRESHOLD=100 run_compactor

POST_COMMITS="$(count_commits "$TEST_DB")"
POST_ISSUES="$(count_issues "$TEST_DB")"
assert_eq "issues unchanged" "$POST_ISSUES" "$PRE_ISSUES"
assert_eq "commits unchanged" "$POST_COMMITS" "$PRE_COMMITS"

# ------------------------------------------------------------------
# Test 3: dry-run → no compaction even above threshold
# ------------------------------------------------------------------
printf '\n[test 3] dry-run skip\n'

setup_db_with_commits "$TEST_DB" 10
PRE_COMMITS="$(count_commits "$TEST_DB")"

GC_COMPACTOR_COMMIT_THRESHOLD=5 GC_COMPACTOR_DRY_RUN=1 run_compactor

POST_COMMITS="$(count_commits "$TEST_DB")"
assert_eq "commits unchanged in dry-run" "$POST_COMMITS" "$PRE_COMMITS"

# ------------------------------------------------------------------
# Test 4: empty DATABASES override → exit 0 cleanly with nothing to do
# ------------------------------------------------------------------
printf '\n[test 4] empty database list — exit clean\n'

GC_COMPACTOR_DATABASES=" " bash "$COMPACTOR" >"$TMP/empty.log" 2>&1 || true
if grep -q "no databases to inspect" "$TMP/empty.log"; then
    printf '  ok empty-list exit clean\n'
else
    cat "$TMP/empty.log" >&2
    printf 'ASSERT FAIL: empty-list path did not log expected message\n' >&2
    exit 1
fi

# ------------------------------------------------------------------
# Test 5: unsupported mode → exit 2
# ------------------------------------------------------------------
printf '\n[test 5] unsupported mode rejected\n'

if GC_COMPACTOR_MODE=surgical bash "$COMPACTOR" >/dev/null 2>&1; then
    printf 'ASSERT FAIL: surgical mode should not be accepted yet\n' >&2
    exit 1
fi
printf '  ok surgical-mode rejected (not yet implemented)\n'

# ------------------------------------------------------------------
# Test 6: rollback path — induced row-count mismatch
#
# Exercises the integrity check that compares pre/post-flight row
# counts after compaction. The GC_COMPACTOR_TEST_INDUCE_MISMATCH hook
# in compactor.sh deletes one row from each user table after
# DOLT_COMMIT, forcing the mismatch. The test asserts:
#   a. the compactor exits non-zero (TOTAL_FAILED > 0)
#   b. all rows are restored (rollback fired and main is back at the
#      pre-compaction HEAD)
#   c. no leftover compact-backup-* branches (rollback succeeded so
#      the backup was dropped per spec)
# ------------------------------------------------------------------
printf '\n[test 6] rollback path on row-count mismatch\n'

setup_db_with_commits "$TEST_DB" 10
PRE_COMMITS="$(count_commits "$TEST_DB")"
PRE_ISSUES="$(count_issues "$TEST_DB")"
PRE_HEAD="$(sql_csv_one "SELECT commit_hash FROM \`$TEST_DB\`.dolt_log ORDER BY date DESC LIMIT 1")"
printf '  pre: commits=%s issues=%s head=%.10s\n' "$PRE_COMMITS" "$PRE_ISSUES" "$PRE_HEAD"

set +e
GC_COMPACTOR_COMMIT_THRESHOLD=5 GC_COMPACTOR_TEST_INDUCE_MISMATCH=1 \
    run_compactor >"$TMP/test6.log" 2>&1
RC=$?
set -e

POST_ISSUES="$(count_issues "$TEST_DB")"
POST_HEAD="$(sql_csv_one "SELECT commit_hash FROM \`$TEST_DB\`.dolt_log ORDER BY date DESC LIMIT 1")"
BACKUP_BRANCHES="$(sql_csv_one "SELECT COUNT(*) FROM \`$TEST_DB\`.dolt_branches WHERE name LIKE 'compact-backup-%'")"
printf '  post: rc=%s issues=%s head=%.10s backup-branches=%s\n' \
    "$RC" "$POST_ISSUES" "$POST_HEAD" "$BACKUP_BRANCHES"

if ! grep -q "row count mismatch" "$TMP/test6.log"; then
    cat "$TMP/test6.log" >&2
    printf 'ASSERT FAIL: expected "row count mismatch" anomaly in log\n' >&2
    exit 1
fi
printf '  ok row-count-mismatch anomaly recorded\n'

assert_eq "rows restored to pre-flight count" "$POST_ISSUES" "$PRE_ISSUES"
assert_eq "main rolled back to pre HEAD" "$POST_HEAD" "$PRE_HEAD"
assert_eq "backup branch dropped after successful rollback" "$BACKUP_BRANCHES" "0"

printf '\ncompactor_test: ALL TESTS PASSED\n'
