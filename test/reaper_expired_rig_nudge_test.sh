#!/usr/bin/env bash
# Test: reaper Step 4 expired-nudge close path — rig-store routing
# (gastownhall/gascity#5285: "Expired rig-scoped nudge shadows evade the
# native reaper").
#
# Before the fix, Step 4 only ever closed expired nudge beads in the city
# store; a non-city $DB always fell into the skip+anomaly-count path even
# when it was a legitimate rig store the reaper already knew about via
# discover_rig_store_refs. The fix adds an elif that resolves $DB to a rig
# name (rig_name_for_db) and closes through gc bd --city --rig instead.
#
# Acceptance criteria:
#   1. DB == CITY_DB                        → close_city_issue called, no rig/skip
#   2. DB != CITY_DB, rig binding found      → close_rig_issue called with that
#                                              rig name; no skip, no city close
#   3. DB != CITY_DB, no rig binding found   → falls back to skip+anomaly-count
#                                              (unchanged prior behavior), no
#                                              close call of either kind
#   4. CITY_DB unset entirely                → skip+anomaly-count (unchanged),
#                                              no close call of either kind

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

# Extract the Step 4 block verbatim from reaper.sh (it lives inside the
# indented per-DB `while` loop, not at column 0, so — unlike the Step 6
# tests — it's captured by comment markers rather than if/fi depth-counting).
STEP4=$(awk '
  /# Step 5:/{f=0}
  f{print}
  /# Step 4:/{f=1}
' "$REAPER")

if [ -z "$STEP4" ]; then
    printf 'ERROR: could not extract Step 4 block from %s\n' "$REAPER" >&2
    exit 1
fi

# run_step4_scenario <db> <city_db> <rig_lookup_result|"none">
#
#   rig_lookup_result  the rig name rig_name_for_db should report finding for
#                       $db, or "none" to simulate no binding (rc=1).
#
# Returns: <city_close_called>|<rig_close_called>|<rig_name_used>|<skipped_total>|<anomaly_called>
run_step4_scenario() {
    local db="$1"
    local city_db="$2"
    local rig_lookup="$3"
    local tmpdir city_flag rig_flag rig_name_file skipped_file anomaly_flag step4_file run_script

    tmpdir=$(mktemp -d)
    city_flag="$tmpdir/city_close_called"
    rig_flag="$tmpdir/rig_close_called"
    rig_name_file="$tmpdir/rig_name_used"
    skipped_file="$tmpdir/skipped_total"
    anomaly_flag="$tmpdir/anomaly_called"
    step4_file="$tmpdir/step4.sh"
    run_script="$tmpdir/run.sh"

    printf '%s\n' "$STEP4" > "$step4_file"

    local rig_lookup_body
    if [ "$rig_lookup" = "none" ]; then
        rig_lookup_body='return 1'
    else
        rig_lookup_body="printf '%s\n' '$rig_lookup'; return 0"
    fi

    cat > "$run_script" << RUNEOF
#!/usr/bin/env bash
set -euo pipefail

get_sql_rows() {
    local db="\$1" label="\$2"
    case "\$label" in
        "expired nudge bead with parse anomaly") SQL_ROWS_RESULT='' ;;
        "expired nudge bead") SQL_ROWS_RESULT='nudge-123' ;;
        *) SQL_ROWS_RESULT='' ;;
    esac
}
record_anomaly() { touch '$anomaly_flag'; }
sanitize_output() { printf '%s' "\$1"; }
close_city_issue() { touch '$city_flag'; return 0; }
close_rig_issue() {
    touch '$rig_flag'
    printf '%s\n' "\$3" > '$rig_name_file'
    return 0
}
rig_name_for_db() { $rig_lookup_body; }
export -f get_sql_rows record_anomaly sanitize_output close_city_issue close_rig_issue rig_name_for_db

DB='$db'
CITY_DB='$city_db'
CITY='/tmp/unused-city'
CITY_DB_ANOMALY_RECORDED=0
DRY_RUN=''
TOTAL_WOULD_EXPIRE=0
TOTAL_EXPIRED_ISSUES_SKIPPED=0
TOTAL_EXPIRED_ISSUES_CLOSED=0
DB_EXPIRED_ISSUES_CLOSED=0
DB_MUTATIONS=0

. '$step4_file'

printf '%s\n' "\$TOTAL_EXPIRED_ISSUES_SKIPPED" > '$skipped_file'
RUNEOF

    local rc=0
    bash "$run_script" 2>/dev/null || rc=$?

    local city_result rig_result rig_name_val skipped_val anomaly_result
    city_result=$([ -f "$city_flag" ] && echo yes || echo no)
    rig_result=$([ -f "$rig_flag" ] && echo yes || echo no)
    rig_name_val=$(cat "$rig_name_file" 2>/dev/null || echo "")
    skipped_val=$(cat "$skipped_file" 2>/dev/null || echo "?")
    anomaly_result=$([ -f "$anomaly_flag" ] && echo yes || echo no)
    rm -rf "$tmpdir"
    printf '%s|%s|%s|%s|%s|%s\n' "$city_result" "$rig_result" "$rig_name_val" "$skipped_val" "$anomaly_result" "$rc"
}

# ── T1: DB == CITY_DB → close_city_issue called ───────────────────────────
result=$(run_step4_scenario "city_db" "city_db" "none")
city_called=$(printf '%s' "$result" | cut -d'|' -f1)
rig_called=$(printf '%s' "$result" | cut -d'|' -f2)
skipped=$(printf '%s' "$result" | cut -d'|' -f4)
if [ "$city_called" = "yes" ] && [ "$rig_called" = "no" ] && [ "$skipped" = "0" ]; then
    pass "T1: DB == CITY_DB → close_city_issue called, no rig close, no skip"
else
    fail "T1: DB == CITY_DB → expected city=yes rig=no skipped=0; got $result"
fi

# ── T2: DB != CITY_DB, rig binding found → close_rig_issue called ────────
result=$(run_step4_scenario "rig_alpha_db" "city_db" "alpha")
city_called=$(printf '%s' "$result" | cut -d'|' -f1)
rig_called=$(printf '%s' "$result" | cut -d'|' -f2)
rig_name=$(printf '%s' "$result" | cut -d'|' -f3)
skipped=$(printf '%s' "$result" | cut -d'|' -f4)
if [ "$city_called" = "no" ] && [ "$rig_called" = "yes" ] && [ "$rig_name" = "alpha" ] && [ "$skipped" = "0" ]; then
    pass "T2: DB != CITY_DB, rig found → close_rig_issue called with rig=alpha, no skip"
else
    fail "T2: DB != CITY_DB, rig found → expected city=no rig=yes rig_name=alpha skipped=0; got $result"
fi

# ── T3: DB != CITY_DB, no rig binding → skip+anomaly (unchanged fallback) ─
result=$(run_step4_scenario "unbound_db" "city_db" "none")
city_called=$(printf '%s' "$result" | cut -d'|' -f1)
rig_called=$(printf '%s' "$result" | cut -d'|' -f2)
skipped=$(printf '%s' "$result" | cut -d'|' -f4)
if [ "$city_called" = "no" ] && [ "$rig_called" = "no" ] && [ "$skipped" = "1" ]; then
    pass "T3: DB != CITY_DB, no rig binding → falls back to skip, no close call"
else
    fail "T3: DB != CITY_DB, no rig binding → expected city=no rig=no skipped=1; got $result"
fi

# ── T4: CITY_DB unset entirely → skip+anomaly (unchanged) ────────────────
result=$(run_step4_scenario "some_db" "" "alpha")
city_called=$(printf '%s' "$result" | cut -d'|' -f1)
rig_called=$(printf '%s' "$result" | cut -d'|' -f2)
skipped=$(printf '%s' "$result" | cut -d'|' -f4)
anomaly=$(printf '%s' "$result" | cut -d'|' -f5)
if [ "$city_called" = "no" ] && [ "$rig_called" = "no" ] && [ "$skipped" = "1" ] && [ "$anomaly" = "yes" ]; then
    pass "T4: CITY_DB unset → skip+anomaly, no close call even when a rig binding exists"
else
    fail "T4: CITY_DB unset → expected city=no rig=no skipped=1 anomaly=yes; got $result"
fi

[ "$FAILED" -eq 0 ] && exit 0 || exit 1
