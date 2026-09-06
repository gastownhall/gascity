#!/usr/bin/env bash
# Test: reaper Step 6 bd-prune backup-age guard
#
# Acceptance criteria:
#   1. No backup state present       → bd NOT called, anomaly recorded
#   2. Fresh backup state            → bd IS called, no anomaly
#   3. Stale backup state            → bd NOT called, anomaly recorded
#   4. RFC3339Nano fresh timestamp   → bd IS called, no anomaly
#   5. Dolt registered + fresh sync  → bd IS called even when the legacy file is stale
#   6. Dolt registered, never synced → bd NOT called even when the legacy file is fresh
#   7. Malformed backup state        → bd NOT called, anomaly recorded
#   8. Dolt-native file:// backup, fresh, no bd state → bd IS called
#   9. Dolt-native file:// backup, stale             → bd NOT called, anomaly names dolt_backups
#  10. Dolt-native remote (non-file) destination     → bd NOT called, primary verdict stands
#  11. Fresh legacy state + stale Dolt-native backup → bd IS called (second opinion never overrides)
#  12. Dolt-native destination registered but empty  → bd NOT called, primary verdict stands
#  13. Two destinations, remote listed first + file:// → the datable one is used

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

# ts_ago <seconds_in_past> [fractional_suffix]
# Prints an RFC3339 UTC timestamp <seconds_in_past> seconds ago. The optional
# second argument is inserted as fractional seconds (e.g. ".765205448") to
# produce the RFC3339Nano form that actually appears on disk.
ts_ago() {
    local age="$1" frac="${2:-}" base
    if command -v python3 >/dev/null 2>&1; then
        base=$(python3 -c "import datetime; print((datetime.datetime.utcnow() - datetime.timedelta(seconds=$age)).strftime('%Y-%m-%dT%H:%M:%S'))")
    else
        base=$(date -u -v-"${age}"S '+%Y-%m-%dT%H:%M:%S' 2>/dev/null \
            || date -u -d "@$(($(date +%s) - age))" '+%Y-%m-%dT%H:%M:%S')
    fi
    printf '%s%sZ\n' "$base" "$frac"
}

# touch_ago <path> <seconds_in_past>
# Backdates a file's mtime. The Dolt-native branch dates a backup by the newest
# object inside it, so the fixture has to be a real mtime, not a JSON string.
touch_ago() {
    local path="$1" age="$2" epoch
    epoch=$(( $(date -u '+%s') - age ))
    touch -d "@$epoch" "$path" 2>/dev/null \
        || touch -t "$(date -u -r "$epoch" '+%Y%m%d%H%M.%S' 2>/dev/null)" "$path"
}

# run_prune_scenario <backup_age_seconds|"absent"|"malformed"> [max_age_seconds] [pipeline] [legacy_age] [frac] [dolt_native]
#
#   pipeline    "legacy" (default) writes .beads/backup/backup_state.json;
#               "dolt" registers .beads/dolt-backup.json and writes
#               .beads/dolt-backup-state.json with a last_sync field.
#   legacy_age  only meaningful for pipeline=dolt: age of an ADDITIONAL legacy
#               backup_state.json, used to prove the guard consults the active
#               pipeline and does not fall back. "absent" (default) writes none.
#   frac        optional fractional-seconds suffix for the active state file.
#   dolt_native what the `dolt_backups` table reports for the city database:
#               "absent" (default) — no row, and dolt_sql is left undefined,
#               exactly as on a city with no Dolt-layer backup;
#               "<seconds>" — a file:// destination whose newest object was
#               written that many seconds ago;
#               "empty" — a file:// destination that exists but holds nothing;
#               "remote" — a DoltHub-style https destination, which carries no
#               locally observable timestamp;
#               "mixed:<seconds>" — two rows, an undatable remote listed FIRST
#               and a file:// destination second.
#
# Returns: <bd_called>|<anomaly_called>|<exit_status>|<anomaly_msg>
run_prune_scenario() {
    local backup_age="$1"
    local max_age="${2:-86400}"
    local pipeline="${3:-legacy}"
    local legacy_age="${4:-absent}"
    local frac="${5:-}"
    local dolt_native="${6:-absent}"
    local tmpdir bd_flag anomaly_flag anomaly_msg_file step6_file run_script
    tmpdir=$(mktemp -d)
    bd_flag="$tmpdir/bd_called"
    anomaly_flag="$tmpdir/anomaly_called"
    anomaly_msg_file="$tmpdir/anomaly_msg"
    step6_file="$tmpdir/step6.sh"
    run_script="$tmpdir/run.sh"

    mkdir -p "$tmpdir/.beads"

    local state_file state_field
    if [ "$pipeline" = "dolt" ]; then
        # A registered destination is what flips the guard to the Dolt pipeline.
        printf '{"destination":"test-remote"}\n' > "$tmpdir/.beads/dolt-backup.json"
        state_file="$tmpdir/.beads/dolt-backup-state.json"
        state_field="last_sync"
        if [ "$legacy_age" != "absent" ]; then
            mkdir -p "$tmpdir/.beads/backup"
            printf '{"last_dolt_commit":"test","timestamp":"%s"}\n' "$(ts_ago "$legacy_age")" \
                > "$tmpdir/.beads/backup/backup_state.json"
        fi
    else
        mkdir -p "$tmpdir/.beads/backup"
        state_file="$tmpdir/.beads/backup/backup_state.json"
        state_field="timestamp"
    fi

    case "$backup_age" in
        absent)
            ;;
        malformed)
            # Truncated JSON: the key is present but the value never is.
            printf '{"%s":\n' "$state_field" > "$state_file"
            ;;
        *)
            printf '{"last_dolt_commit":"test","%s":"%s"}\n' \
                "$state_field" "$(ts_ago "$backup_age" "$frac")" > "$state_file"
            ;;
    esac

    # Dolt-native destination: a `dolt_backups` row for the city database.
    # dolt_sql stays UNDEFINED for "absent" so the guard sees what it sees on a
    # city that has no Dolt-layer backup at all.
    local dolt_stub=""
    if [ "$dolt_native" != "absent" ]; then
        local dest_rows="file://$tmpdir/dolt-backup"
        case "$dolt_native" in
            remote)
                dest_rows="https://doltremoteapi.dolthub.com/example/city"
                ;;
            empty)
                mkdir -p "$tmpdir/dolt-backup"
                ;;
            mixed:*)
                mkdir -p "$tmpdir/dolt-backup"
                : > "$tmpdir/dolt-backup/manifest"
                touch_ago "$tmpdir/dolt-backup/manifest" "${dolt_native#mixed:}"
                dest_rows="https://doltremoteapi.dolthub.com/example/city
file://$tmpdir/dolt-backup"
                ;;
            *)
                mkdir -p "$tmpdir/dolt-backup"
                : > "$tmpdir/dolt-backup/manifest"
                touch_ago "$tmpdir/dolt-backup/manifest" "$dolt_native"
                ;;
        esac
        dolt_stub="dolt_sql() { printf 'url\n%s\n' '$dest_rows'; }
export -f dolt_sql"
    fi

    printf '%s\n' "$STEP6" > "$step6_file"

    cat > "$run_script" << RUNEOF
#!/usr/bin/env bash
set -euo pipefail
gc()            { touch '$bd_flag'; printf '{"pruned_count":3}'; }
record_anomaly(){ touch '$anomaly_flag'; printf '%s\n' "\$*" >> '$anomaly_msg_file'; }
export -f gc record_anomaly
$dolt_stub
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

    # The stubbed Step 6 environment can legitimately exit nonzero, so this is
    # surfaced in the tuple for diagnosis rather than asserted on.
    local rc=0
    bash "$run_script" 2>/dev/null || rc=$?

    local bd_result anomaly_result anomaly_msg_val
    bd_result=$([ -f "$bd_flag" ] && echo yes || echo no)
    anomaly_result=$([ -f "$anomaly_flag" ] && echo yes || echo no)
    anomaly_msg_val=$(cat "$anomaly_msg_file" 2>/dev/null || echo "")
    rm -rf "$tmpdir"
    printf '%s|%s|%s|%s\n' "$bd_result" "$anomaly_result" "$rc" "$anomaly_msg_val"
}

# ── T1: no backup_state.json → bd NOT called, anomaly recorded ────────────────
result=$(run_prune_scenario "absent")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ]; then
    pass "T1: absent backup_state.json → bd skipped, anomaly recorded"
else
    fail "T1: absent backup_state.json → expected bd=no anomaly=yes; got bd=$bd_called anomaly=$anomaly_called rc=$rc"
fi

# ── T2: fresh backup (60s old, well within 86400s) → bd IS called ────────────
result=$(run_prune_scenario "60")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
if [ "$bd_called" = "yes" ] && [ "$anomaly_called" = "no" ]; then
    pass "T2: fresh backup (60s) → bd called, no anomaly"
else
    fail "T2: fresh backup (60s) → expected bd=yes anomaly=no; got bd=$bd_called anomaly=$anomaly_called rc=$rc"
fi

# ── T3: stale backup (90000s old, > 86400s threshold) → bd NOT called ─────────
result=$(run_prune_scenario "90000")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ] \
        && printf '%s' "$anomaly_msg" | grep -qi "stale\|backup\|prune"; then
    pass "T3: stale backup (90000s) → bd skipped, anomaly recorded with stale/backup/prune keyword"
else
    fail "T3: stale backup (90000s) → expected bd=no anomaly=yes+keyword; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T4: fresh backup with RFC3339Nano timestamp → bd IS called ───────────────
# Real on-disk timestamps carry nanoseconds; the strptime fallback rejects them
# outright, so the guard must truncate before parsing.
result=$(run_prune_scenario "60" "86400" "legacy" "absent" ".765205448")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "yes" ] && [ "$anomaly_called" = "no" ]; then
    pass "T4: fresh RFC3339Nano backup (60s) → bd called, no anomaly"
else
    fail "T4: fresh RFC3339Nano backup (60s) → expected bd=yes anomaly=no; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T5: Dolt registered + fresh last_sync + STALE legacy file → bd IS called ──
# The fleet-breaking case: `bd backup sync` only ever advances
# dolt-backup-state.json, so a migrated scope's legacy file is frozen at
# whatever the retired writer last recorded. Reading it would latch the guard
# closed forever.
result=$(run_prune_scenario "60" "86400" "dolt" "9000000")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "yes" ] && [ "$anomaly_called" = "no" ]; then
    pass "T5: dolt registered, fresh last_sync, stale legacy file → bd called, no anomaly"
else
    fail "T5: dolt registered, fresh last_sync, stale legacy file → expected bd=yes anomaly=no; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T6: Dolt registered but never synced → bd NOT called ─────────────────────
# A fresh legacy file is present precisely so that falling back to it would
# wrongly permit the prune. The registered-but-never-synced scope stays closed.
result=$(run_prune_scenario "absent" "86400" "dolt" "60")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ]; then
    pass "T6: dolt registered, never synced (fresh legacy present) → bd skipped, anomaly recorded"
else
    fail "T6: dolt registered, never synced (fresh legacy present) → expected bd=no anomaly=yes; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T7: malformed backup_state.json → bd NOT called, anomaly recorded ────────
result=$(run_prune_scenario "malformed")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ]; then
    pass "T7: malformed backup_state.json → bd skipped, anomaly recorded"
else
    fail "T7: malformed backup_state.json → expected bd=no anomaly=yes; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T8: Dolt-native file:// backup, fresh, no bd state at all → bd IS called ──
# The reported bug (ga-am89b): backups registered in `dolt_backups` never pass
# through bd, so neither state file is ever written and the guard escalated on
# EVERY tick against a backup minutes old — 70+ MEDIUM escalations in 24h.
result=$(run_prune_scenario "absent" "86400" "legacy" "absent" "" "60")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "yes" ] && [ "$anomaly_called" = "no" ]; then
    pass "T8: dolt-native file:// backup 60s old, no bd state → bd called, no anomaly"
else
    fail "T8: dolt-native fresh → expected bd=yes anomaly=no; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T9: Dolt-native backup present but stale → bd NOT called ─────────────────
# The second opinion must age out on its own, or a stopped backup order would be
# silenced permanently — worse than the noise it replaces.
result=$(run_prune_scenario "absent" "86400" "legacy" "absent" "" "90000")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ] \
        && printf '%s' "$anomaly_msg" | grep -q "dolt_backups="; then
    pass "T9: dolt-native backup 90000s old → bd skipped, anomaly names dolt_backups age"
else
    fail "T9: dolt-native stale → expected bd=no anomaly=yes+dolt_backups=; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T10: Dolt-native destination is remote → primary verdict stands ──────────
# A DoltHub destination has no locally observable timestamp. It is no evidence
# either way, so it must not clear the gate.
result=$(run_prune_scenario "absent" "86400" "legacy" "absent" "" "remote")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ]; then
    pass "T10: dolt-native remote destination → bd skipped, primary verdict stands"
else
    fail "T10: dolt-native remote → expected bd=no anomaly=yes; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T11: fresh legacy state + stale Dolt-native backup → bd IS called ────────
# The Dolt layer is consulted only after the primary evidence has already
# failed, so a stale one can never veto a passing gate.
result=$(run_prune_scenario "60" "86400" "legacy" "absent" "" "90000")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "yes" ] && [ "$anomaly_called" = "no" ]; then
    pass "T11: fresh legacy state + stale dolt-native backup → bd called, no anomaly"
else
    fail "T11: fresh legacy + stale dolt-native → expected bd=yes anomaly=no; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T12: Dolt-native destination registered but empty → primary stands ──────
# A registered destination that has never received an object is not a backup.
result=$(run_prune_scenario "absent" "86400" "legacy" "absent" "" "empty")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "no" ] && [ "$anomaly_called" = "yes" ]; then
    pass "T12: dolt-native destination empty → bd skipped, primary verdict stands"
else
    fail "T12: dolt-native empty → expected bd=no anomaly=yes; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

# ── T13: remote row listed first, file:// row second → the datable one wins ──
# `dolt_backups` can hold several destinations. Stopping at the first row would
# hand back "no evidence" whenever a remote happens to sort first.
result=$(run_prune_scenario "absent" "86400" "legacy" "absent" "" "mixed:60")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
anomaly_called=$(printf '%s' "$result" | cut -d'|' -f2)
rc=$(printf '%s' "$result" | cut -d'|' -f3)
anomaly_msg=$(printf '%s' "$result" | cut -d'|' -f4-)
if [ "$bd_called" = "yes" ] && [ "$anomaly_called" = "no" ]; then
    pass "T13: remote row first + fresh file:// row → bd called, no anomaly"
else
    fail "T13: mixed destinations → expected bd=yes anomaly=no; got bd=$bd_called anomaly=$anomaly_called rc=$rc msg=$anomaly_msg"
fi

[ "$FAILED" -eq 0 ] && exit 0 || exit 1
