#!/usr/bin/env bash
# Test: reaper Step 6 bd-prune backup-age guard (vp-pt73.5 T-007/T-008)
#
# Acceptance criteria:
#   1. No backup_state.json present  → bd NOT called, anomaly recorded
#   2. Fresh backup_state.json       → bd IS called, no anomaly
#   3. Stale backup_state.json       → bd NOT called, anomaly recorded

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

# Extract Step 6 block from reaper.sh using depth-counting on column-0 if/fi.
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

# run_prune_scenario <backup_age_seconds|"absent"> <max_age_seconds>
# Returns: <bd_called>|<anomaly_called>|<anomaly_msg>
run_prune_scenario() {
    local backup_age="$1"
    local max_age="${2:-86400}"
    local tmpdir bd_flag anomaly_flag anomaly_msg_file step6_file run_script
    tmpdir=$(mktemp -d)
    bd_flag="$tmpdir/bd_called"
    anomaly_flag="$tmpdir/anomaly_called"
    anomaly_msg_file="$tmpdir/anomaly_msg"
    step6_file="$tmpdir/step6.sh"
    run_script="$tmpdir/run.sh"

    mkdir -p "$tmpdir/.beads"

    if [ "$backup_age" != "absent" ]; then
        # Write a backup_state.json whose timestamp is <backup_age> seconds in the past.
        mkdir -p "$tmpdir/.beads/backup"
        # Use Python or date to compute the timestamp.
        if command -v python3 >/dev/null 2>&1; then
            TS=$(python3 -c "import datetime; print((datetime.datetime.utcnow() - datetime.timedelta(seconds=$backup_age)).strftime('%Y-%m-%dT%H:%M:%SZ'))")
        else
            TS=$(date -u -v-"${backup_age}"S '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null \
                || date -u -d "@$(($(date +%s) - backup_age))" '+%Y-%m-%dT%H:%M:%SZ')
        fi
        printf '{"last_dolt_commit":"test","timestamp":"%s"}\n' "$TS" \
            > "$tmpdir/.beads/backup/backup_state.json"
    fi

    printf '%s\n' "$STEP6" > "$step6_file"

    cat > "$run_script" << RUNEOF
#!/usr/bin/env bash
set -euo pipefail
gc()            { touch '$bd_flag'; printf '{"pruned_count":3}'; }
record_anomaly(){ touch '$anomaly_flag'; printf '%s\n' "\$*" >> '$anomaly_msg_file'; }
export -f gc record_anomaly
CITY_ABS='$tmpdir'
CITY_BEADS_DIR='$tmpdir/.beads'
SESSION_BEAD_PATTERN='gm-*'
SESSION_PURGE_AGE='720h'
DRY_RUN=''
TOTAL_SESSIONS_PRUNED=0
SESSION_PRUNE_ATTEMPTED=0
CITY_DB='test_db'
GC_BACKUP_MAX_AGE_FOR_BULK_DELETE='$max_age'
. '$step6_file'
RUNEOF

    bash "$run_script" 2>/dev/null || true

    local bd_result anomaly_result anomaly_msg_val
    bd_result=$([ -f "$bd_flag" ] && echo yes || echo no)
    anomaly_result=$([ -f "$anomaly_flag" ] && echo yes || echo no)
    anomaly_msg_val=$(cat "$anomaly_msg_file" 2>/dev/null || echo "")
    rm -rf "$tmpdir"
    printf '%s|%s|%s\n' "$bd_result" "$anomaly_result" "$anomaly_msg_val"
}

# ── T1: no backup_state.json → bd NOT called, anomaly recorded ────────────────
result=$(run_prune_scenario "absent")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ]; then
    pass "T1: absent backup_state.json → bd skipped, anomaly recorded"
else
    fail "T1: absent backup_state.json → expected bd=no anomaly=yes; got bd=$bd_called anomaly=$anomaly_called"
fi

# ── T2: fresh backup (60s old, well within 86400s) → bd IS called ────────────
result=$(run_prune_scenario "60")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
if [ "$bd_called" = "yes" ] && [ "$anomaly_called" = "no" ]; then
    pass "T2: fresh backup (60s) → bd called, no anomaly"
else
    fail "T2: fresh backup (60s) → expected bd=yes anomaly=no; got bd=$bd_called anomaly=$anomaly_called"
fi

# ── T3: stale backup (90000s old, > 86400s threshold) → bd NOT called ─────────
result=$(run_prune_scenario "90000")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f3-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ] \
        && printf '%s' "$anomaly_msg" | grep -qi "stale\|backup\|prune"; then
    pass "T3: stale backup (90000s) → bd skipped, anomaly recorded with stale/backup/prune keyword"
else
    fail "T3: stale backup (90000s) → expected bd=no anomaly=yes+keyword; got bd=$bd_called anomaly=$anomaly_called msg=$anomaly_msg"
fi

[ "$FAILED" -eq 0 ] && exit 0 || exit 1
