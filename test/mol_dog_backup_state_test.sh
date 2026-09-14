#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKUP="$ROOT/examples/bd/dolt/assets/scripts/mol-dog-backup.sh"
FAILED=0

pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; FAILED=1; }

run_scenario() {
    local fail_db="${1:-}"
    local tmp="$2"
    mkdir -p "$tmp/bin" "$tmp/data/hq/.dolt" "$tmp/data/sdp/.dolt" "$tmp/city/.beads"

    cat >"$tmp/bin/flock" <<'SH'
#!/bin/sh
exit 0
SH
    cat >"$tmp/bin/dolt" <<'SH'
#!/bin/sh
case "$1" in
    version)
        echo 'dolt version 2.1.0'
        exit 0
        ;;
    backup)
        db=$(basename "$PWD")
        if [ "${2:-}" = "sync" ] && [ "$db" = "${GC_FAKE_FAIL_DB:-}" ]; then
            count=0
            [ ! -f "$GC_FAKE_COUNT" ] || count=$(cat "$GC_FAKE_COUNT")
            count=$((count + 1))
            printf '%s\n' "$count" >"$GC_FAKE_COUNT"
            if [ "$count" -eq 1 ]; then
                echo 'sync failed Password=first-secret https://user:first-url-secret@example.test/repo' >&2
            else
                echo 'later failure TOKEN=second-secret' >&2
            fi
            exit 1
        fi
        if [ -z "${2:-}" ]; then
            echo "${db}-backup file://backup"
        fi
        exit 0
        ;;
esac
exit 0
SH
    cat >"$tmp/escalate" <<'SH'
#!/bin/sh
printf '%s\n' "$@" >"$GC_FAKE_ESCALATION"
SH
    chmod +x "$tmp/bin/flock" "$tmp/bin/dolt" "$tmp/escalate"

    PATH="$tmp/bin:$PATH" \
    GC_PACK_DIR="$ROOT/examples/bd/dolt" \
    GC_CITY_PATH="$tmp/city" \
    GC_DOLT_PORT=3307 \
    DOLT_DATA_DIR="$tmp/data" \
    GC_BACKUP_DATABASES='hq,sdp' \
    GC_DOLT_BACKUP_SYNC_ATTEMPTS=2 \
    GC_DOLT_BACKUP_SYNC_TIMEOUT_SECS=5 \
    GC_DOLT_BACKUP_LOCK_FILE="$tmp/backup.lock" \
    GC_DOLT_BACKUP_STATE_FILE="$tmp/city/.beads/dolt-backup-state.json" \
    GC_DOLT_DATA_DIR="$tmp/data" \
    GC_ESCALATE_SCRIPT="$tmp/escalate" \
    GC_FAKE_FAIL_DB="$fail_db" \
    GC_FAKE_COUNT="$tmp/count" \
    GC_FAKE_ESCALATION="$tmp/escalation" \
    bash "$BACKUP" >"$tmp/stdout" 2>"$tmp/stderr"
}

success_tmp=$(mktemp -d)
if run_scenario "" "$success_tmp"; then
    state="$success_tmp/city/.beads/dolt-backup-state.json"
    if [ -f "$state" ] && grep -q '"last_sync"' "$state" \
        && grep -q '"synced":2,"total":2' "$state"; then
        pass 'all required syncs publish canonical state'
    else
        fail 'successful required syncs did not publish canonical state'
    fi
else
    fail 'successful required sync scenario returned nonzero'
fi
rm -rf "$success_tmp"

failure_tmp=$(mktemp -d)
if run_scenario "hq" "$failure_tmp"; then
    fail 'failed required sync returned zero'
else
    state="$failure_tmp/city/.beads/dolt-backup-state.json"
    if [ -e "$state" ]; then
        fail 'failed required sync published fresh state'
    else
        pass 'failed required sync returns nonzero without publishing state'
    fi
    escalation=$(cat "$failure_tmp/escalation" 2>/dev/null || true)
    if printf '%s' "$escalation" | grep -q 'first-secret\|first-url-secret\|second-secret'; then
        fail 'incident leaked sync credentials'
    elif printf '%s' "$escalation" | grep -q 'sync failed Password=\[redacted\]' \
        && ! printf '%s' "$escalation" | grep -q 'later failure'; then
        pass 'incident retains the first sanitized sync error'
    else
        fail 'incident did not retain sanitized first-failure evidence'
    fi
fi
rm -rf "$failure_tmp"

[ "$FAILED" -eq 0 ]
