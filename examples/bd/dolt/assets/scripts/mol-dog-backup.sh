#!/usr/bin/env bash
# mol-dog-backup — sync Dolt databases to backup remotes and offsite storage.
#
# Converted from the former mol-dog-backup formula. All operations are deterministic:
# dolt backup sync per DB, rsync backup artifacts to offsite path. No LLM judgment needed.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
. "$PACK_DIR/assets/scripts/_notify.sh"

PORT="$GC_DOLT_PORT"
HOST="${GC_DOLT_HOST:-127.0.0.1}"
USER="${GC_DOLT_USER:-root}"
OFFSITE_PATH="${GC_BACKUP_OFFSITE_PATH:-}"
BACKUP_ARTIFACT_DIR="${GC_BACKUP_ARTIFACT_DIR:-$GC_CITY_PATH/.dolt-backup}"
SYSTEM_DBS="^(information_schema|mysql|dolt_cluster|__gc_probe|performance_schema|sys)$"
MIN_DOLT_BACKUP_VERSION="2.1.0"
BACKUP_LOCK_FILE="${GC_DOLT_BACKUP_LOCK_FILE:-$GC_CITY_PATH/.gc/runtime/packs/dolt/backup-sync.lock}"
BACKUP_LOCK_WAIT_SECONDS="${GC_DOLT_BACKUP_LOCK_WAIT_SECONDS:-5}"
# Wall-clock bound for one `dolt backup sync` attempt, and how many attempts a
# database gets before it is reported failed.
#
# Retries are here because the sync is a marginal operation rather than a
# reliably-fast one. On a busy city the managed sql-server serialises the
# backup against live agent traffic, so the same delta that takes single-digit
# seconds against an idle server can land either side of the server's own
# listener.read_timeout_millis ceiling depending on what else is running. A
# single attempt then turns a load spike into a skipped backup, and the next
# chance is a whole interval away. Measured on one city: four consecutive
# scheduled runs failed at ~30s over 12 hours, and the first manual retry
# afterwards succeeded in 28s against the same data.
BACKUP_SYNC_TIMEOUT_SECS="${GC_DOLT_BACKUP_SYNC_TIMEOUT_SECS:-120}"
BACKUP_SYNC_ATTEMPTS="${GC_DOLT_BACKUP_SYNC_ATTEMPTS:-3}"
case "$BACKUP_SYNC_TIMEOUT_SECS" in
    ''|*[!0-9]*) BACKUP_SYNC_TIMEOUT_SECS=120 ;;
    *[1-9]*) ;;
    *) BACKUP_SYNC_TIMEOUT_SECS=120 ;;
esac
case "$BACKUP_SYNC_ATTEMPTS" in
    ''|*[!0-9]*) BACKUP_SYNC_ATTEMPTS=3 ;;
    *[1-9]*) ;;
    *) BACKUP_SYNC_ATTEMPTS=3 ;;
esac

dolt_sql() {
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" \
        run_bounded 30 \
        dolt --host "$HOST" --port "$PORT" --user "$USER" --no-tls sql "$@"
}

dolt_version_at_least() {
    current="${1#v}"
    minimum="$2"
    current="${current%%+*}"
    minimum="${minimum%%+*}"
    case "$current" in
        *-*) return 1 ;;
    esac
    IFS=. read -r cur_major cur_minor cur_patch <<EOF
$current
EOF
    IFS=. read -r min_major min_minor min_patch <<EOF
$minimum
EOF
    for part in "$cur_major" "$cur_minor" "$cur_patch" "$min_major" "$min_minor" "$min_patch"; do
        case "$part" in
            ''|*[!0-9]*) return 1 ;;
        esac
    done
    cur_major=$((10#$cur_major))
    cur_minor=$((10#$cur_minor))
    cur_patch=$((10#$cur_patch))
    min_major=$((10#$min_major))
    min_minor=$((10#$min_minor))
    min_patch=$((10#$min_patch))
    if [ "$cur_major" -ne "$min_major" ]; then
        [ "$cur_major" -gt "$min_major" ]
        return $?
    fi
    if [ "$cur_minor" -ne "$min_minor" ]; then
        [ "$cur_minor" -gt "$min_minor" ]
        return $?
    fi
    [ "$cur_patch" -ge "$min_patch" ]
}

append_failed_db() {
    db_failure="$1"
    FAILED=$((FAILED + 1))
    if [ -n "$FAILED_DBS" ]; then
        FAILED_DBS="$FAILED_DBS, $db_failure"
    else
        FAILED_DBS="$db_failure"
    fi
}

# append_failed_detail <db> <detail> — record the underlying diagnostic for a
# database that failed every attempt, so the escalation body can carry a cause
# rather than only the name of the symptom.
append_failed_detail() {
    detail_db="$1"
    detail_text="$2"
    [ -n "$detail_text" ] || detail_text="no diagnostic output"
    FAILED_DETAILS="$FAILED_DETAILS
  $detail_db: $detail_text"
}

# classify_sync_failure <rc> <stderr-file> — one actionable line naming what
# went wrong, for operators reading the escalation rather than the log.
#
# `connection was closed` and `row read wait bigger than connection timeout`
# are how the managed sql-server reports killing an operation that outran
# listener.read_timeout_millis. That reads like a network fault and is not one:
# the client is healthy and waiting, and the server ends the query because its
# own read deadline elapsed. Naming the setting here is the difference between
# an operator tuning the backup and an operator hunting a phantom disconnect.
classify_sync_failure() {
    classify_rc="$1"
    classify_err_file="$2"
    classify_stderr=""
    if [ -s "$classify_err_file" ]; then
        classify_stderr=$(tr '\n' ' ' <"$classify_err_file" | sed 's/[[:space:]]\{1,\}/ /g; s/^ //; s/ $//')
    fi

    if [ "$classify_rc" -eq 124 ]; then
        printf 'timed out after %ss (raise GC_DOLT_BACKUP_SYNC_TIMEOUT_SECS)' "$BACKUP_SYNC_TIMEOUT_SECS"
        return
    fi
    case "$classify_stderr" in
        *"connection was closed"*|*"row read wait bigger than connection timeout"*)
            printf 'exit %s: %s — the managed sql-server ended the operation at its listener.read_timeout_millis ceiling; the backup outran it under live load, so raise that ceiling for this city or reduce contention during the backup window' \
                "$classify_rc" "$classify_stderr"
            return
            ;;
    esac
    if [ -n "$classify_stderr" ]; then
        printf 'exit %s: %s' "$classify_rc" "$classify_stderr"
    else
        printf 'exit %s: no diagnostic output' "$classify_rc"
    fi
}

# sync_one_database <db> <db-dir> — run `dolt backup sync` with bounded retries.
# Emits nothing on success. On total failure it echoes the classified
# diagnostic from the LAST attempt on stdout for the caller to record.
#
# Stderr is captured rather than discarded. Sending it to /dev/null is what
# reduced a real 18-hour backup outage to the single string "hq(sync failed)",
# with the actual cause visible nowhere; the sibling `gc dolt sync` in this
# same pack captures and replays it for exactly this reason.
sync_one_database() {
    sync_db="$1"
    sync_db_dir="$2"
    sync_err_tmp=$(mktemp) || {
        printf 'cannot create temp file for sync diagnostics'
        return 1
    }
    sync_attempt=1
    while [ "$sync_attempt" -le "$BACKUP_SYNC_ATTEMPTS" ]; do
        sync_rc=0
        (cd "$sync_db_dir" && run_bounded "$BACKUP_SYNC_TIMEOUT_SECS" \
            dolt backup sync "${sync_db}-backup" >/dev/null 2>"$sync_err_tmp") || sync_rc=$?
        if [ "$sync_rc" -eq 0 ]; then
            if [ "$sync_attempt" -gt 1 ]; then
                echo "backup: $sync_db: succeeded on attempt $sync_attempt/$BACKUP_SYNC_ATTEMPTS" >&2
            fi
            rm -f "$sync_err_tmp"
            return 0
        fi
        sync_detail=$(classify_sync_failure "$sync_rc" "$sync_err_tmp")
        echo "backup: $sync_db: attempt $sync_attempt/$BACKUP_SYNC_ATTEMPTS failed — $sync_detail" >&2
        sync_attempt=$((sync_attempt + 1))
    done
    rm -f "$sync_err_tmp"
    printf '%s' "$sync_detail"
    return 1
}

acquire_backup_lock() {
    case "$BACKUP_LOCK_WAIT_SECONDS" in
        ''|*[!0-9]*) BACKUP_LOCK_WAIT_SECONDS=5 ;;
    esac
    if ! command -v flock >/dev/null 2>&1; then
        SUMMARY="backup — flock-missing"
        dolt_escalate \
            "Dolt backup: flock missing for backup sync [HIGH]" \
            "Skipping backup sync because flock is unavailable; concurrent dolt backup sync can overload the shared sql-server." \
            2>/dev/null || true
        dolt_notify_done "$SUMMARY"
        echo "backup: $SUMMARY"
        exit 1
    fi

    mkdir -p "$(dirname "$BACKUP_LOCK_FILE")"
    exec 9>"$BACKUP_LOCK_FILE"
    if ! flock -w "$BACKUP_LOCK_WAIT_SECONDS" 9; then
        SUMMARY="backup — skipped: already running"
        dolt_notify_done "$SUMMARY"
        echo "backup: $SUMMARY"
        exit 0
    fi
}

# --- Step 1: Preflight Dolt version before backup sync ---

DOLT_VERSION="$(dolt version 2>/dev/null | awk 'NR == 1 {print $NF}' || true)"
if ! dolt_version_at_least "$DOLT_VERSION" "$MIN_DOLT_BACKUP_VERSION"; then
    dolt_escalate \
        "Dolt backup: dolt-too-old for backup sync [HIGH]" \
        "Skipping backup sync: dolt version ${DOLT_VERSION:-unknown} is below required ${MIN_DOLT_BACKUP_VERSION}. Gas City requires this managed Dolt floor before backup sync." \
        2>/dev/null || true
    SUMMARY="backup — dolt-too-old: ${DOLT_VERSION:-unknown}, required: $MIN_DOLT_BACKUP_VERSION"
    dolt_notify_done "$SUMMARY"
    echo "backup: $SUMMARY"
    exit 1
fi

acquire_backup_lock

# --- Step 2: Sync databases to backup remotes ---

# If GC_BACKUP_DATABASES is set, use it; otherwise auto-discover every user
# database in the data dir. Discovery used to require an existing <db>-backup
# remote, silently excluding unconfigured DBs from backup coverage — which is
# how production DBs ended up unrecoverable after journal corruption (#3176:
# beads_hq had no named remote, so it was never synced). DBs without the
# remote now get one auto-configured below.
if [ -n "${GC_BACKUP_DATABASES:-}" ]; then
    DATABASES=$(echo "$GC_BACKUP_DATABASES" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | grep -v '^$' || true)
else
    ALL_DBS=$(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2 | \
        grep -viE "$SYSTEM_DBS" || true)
    DATABASES=""
    for db in $ALL_DBS; do
        if [ -d "$DOLT_DATA_DIR/$db/.dolt" ]; then
            DATABASES="$DATABASES $db"
        fi
    done
    DATABASES=$(echo "$DATABASES" | tr ' ' '\n' | grep -v '^$' || true)
fi

if [ -z "$DATABASES" ]; then
    echo "backup: no databases found, skipping"
    exit 0
fi

# ensure_backup_remote guarantees db has a named <db>-backup remote, creating
# one under the backup artifact dir when missing. Auto-configuration is logged
# loudly so operators can see when coverage was established rather than
# assumed. Returns 1 when the remote cannot be configured.
ensure_backup_remote() {
    remote_db="$1"
    remote_db_dir="$DOLT_DATA_DIR/$remote_db"
    [ -d "$remote_db_dir/.dolt" ] || return 0 # sync loop reports not-found
    if (cd "$remote_db_dir" && run_bounded 30 dolt backup 2>/dev/null | awk '{print $1}' | grep -qx "${remote_db}-backup"); then
        return 0
    fi
    remote_url="file://$BACKUP_ARTIFACT_DIR/$remote_db"
    mkdir -p "$BACKUP_ARTIFACT_DIR/$remote_db"
    if (cd "$remote_db_dir" && run_bounded 30 dolt backup add "${remote_db}-backup" "$remote_url" >/dev/null 2>&1); then
        echo "backup: auto-configured missing backup remote ${remote_db}-backup -> $remote_url"
        return 0
    fi
    return 1
}

TOTAL=$(printf '%s\n' "$DATABASES" | awk 'NF {count++} END {print count + 0}')
SYNCED=0
FAILED=0
FAILED_DBS=""
FAILED_DETAILS=""

for db in $DATABASES; do
    if ! ensure_backup_remote "$db"; then
        append_failed_db "$db(backup add failed)"
        continue
    fi
    db_dir="$DOLT_DATA_DIR/$db"
    if [ ! -d "$db_dir/.dolt" ]; then
        append_failed_db "$db(not found)"
        continue
    fi
    sync_failure_detail=""
    if sync_failure_detail=$(sync_one_database "$db" "$db_dir"); then
        SYNCED=$((SYNCED + 1))
    else
        append_failed_db "$db(sync failed)"
        append_failed_detail "$db" "$sync_failure_detail"
    fi
done

FAILED_COUNT=$FAILED
OFFSITE_STATUS="skipped"

# --- Step 3: Rsync backup artifacts to offsite storage ---

if [ -n "$OFFSITE_PATH" ]; then
    if [ ! -d "$BACKUP_ARTIFACT_DIR" ]; then
        OFFSITE_STATUS="missing-artifacts"
    elif same_path "$BACKUP_ARTIFACT_DIR" "$DOLT_DATA_DIR"; then
        OFFSITE_STATUS="invalid-source"
    elif run_bounded 300 rsync -a --delete "$BACKUP_ARTIFACT_DIR/" "$OFFSITE_PATH/" 2>/dev/null; then
        OFFSITE_STATUS="ok"
    else
        OFFSITE_STATUS="failed (non-fatal)"
    fi
fi

# --- Step 4: Report ---

if [ "$FAILED_COUNT" -gt 0 ]; then
    dolt_escalate \
        "Dolt backup: $FAILED_COUNT/$TOTAL databases failed to sync [MEDIUM]" \
        "Failed databases:$FAILED_DBS

Each database was attempted up to $BACKUP_SYNC_ATTEMPTS times with a ${BACKUP_SYNC_TIMEOUT_SECS}s bound per attempt. Diagnostic from the final attempt:$FAILED_DETAILS

A database listed here has no backup newer than its last successful sync, so the recoverable copy is as old as that run. Check freshness per database under $BACKUP_ARTIFACT_DIR rather than trusting this message alone." \
        2>/dev/null || true
fi

SUMMARY="backup — synced: $SYNCED/$TOTAL, offsite: $OFFSITE_STATUS"
dolt_notify_done "$SUMMARY"
echo "backup: $SUMMARY"
