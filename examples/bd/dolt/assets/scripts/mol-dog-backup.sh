#!/usr/bin/env bash
# mol-dog-backup — back up every bead scope with `bd backup` and publish the
# backup artifacts offsite.
#
# Deterministic exec order (no LLM, no agent, no wisp). Every bead scope (the
# city and each rig) is reached through `gc bd --city <city> [--rig <rig>]`,
# so bd picks the transport and owns the backup: `bd backup status` says
# whether a destination is registered, `bd backup init` registers one under
# the artifact dir when it is missing, and `bd backup sync` pushes the scope's
# database (all branches, full history) to it. The artifact dir is then
# rsynced to GC_BACKUP_OFFSITE_PATH when that is set.
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"
# shellcheck disable=SC1091
. "$PACK_DIR/assets/scripts/_bounded.sh"
# shellcheck disable=SC1091
. "$PACK_DIR/assets/scripts/_notify.sh"

CITY_ABS="$(cd "$GC_CITY_PATH" 2>/dev/null && pwd -P || printf '%s\n' "$GC_CITY_PATH")"
OFFSITE_PATH="${GC_BACKUP_OFFSITE_PATH:-}"
BACKUP_ARTIFACT_DIR="${GC_BACKUP_ARTIFACT_DIR:-$CITY_ABS/.dolt-backup}"
BACKUP_LOCK_FILE="${GC_DOLT_BACKUP_LOCK_FILE:-$CITY_ABS/.gc/runtime/packs/dolt/backup-sync.lock}"
BACKUP_LOCK_WAIT_SECONDS="${GC_DOLT_BACKUP_LOCK_WAIT_SECONDS:-5}"
# Wall-clock bound for one `bd backup sync` attempt, and how many attempts a
# scope gets before it is reported failed.
#
# Retries are here because the sync is a marginal operation rather than a
# reliably-fast one. On a busy city the sql-server serialises the backup
# against live agent traffic, so the same delta that takes single-digit
# seconds against an idle server can land either side of the server's own
# listener.read_timeout_millis ceiling depending on what else is running. A
# single attempt then turns a load spike into a skipped backup, and the next
# chance is a whole interval away.
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
# Bound for the short bd calls (status, init, database lookup).
BACKUP_BD_TIMEOUT_SECS="${GC_DOLT_BACKUP_BD_TIMEOUT_SECS:-60}"
case "$BACKUP_BD_TIMEOUT_SECS" in ''|*[!0-9]*|0) BACKUP_BD_TIMEOUT_SECS=60 ;; esac

if ! command -v jq >/dev/null 2>&1; then
    echo "backup: jq is required but not found in PATH" >&2
    exit 1
fi

# ── typed skipped/partial outcome (see core order_outcome.sh) ────────────────
OUTCOME_SCOPES_JSON="[]"
outcome_scope_skipped() {
    OUTCOME_SCOPES_JSON=$(printf '%s' "$OUTCOME_SCOPES_JSON" |
        jq -c --arg scope "$1" --arg reason "$2" '. + [{scope: $scope, reason: $reason}]')
}
outcome_write() {
    local kind="$1"
    local reason="$2"
    [ -n "${GC_ORDER_OUTCOME_FILE:-}" ] || return 0
    jq -n -c --arg outcome "$kind" --arg reason "$reason" --argjson scopes "$OUTCOME_SCOPES_JSON" \
        '{outcome: $outcome, reason: $reason, scopes: $scopes}' >"$GC_ORDER_OUTCOME_FILE"
}

# ── bead scopes through gc bd ────────────────────────────────────────────────
SCOPE_RIG=""
scope_bd() {
    if [ -n "$SCOPE_RIG" ]; then
        (cd "$CITY_ABS" && gc bd --city "$CITY_ABS" --rig "$SCOPE_RIG" "$@")
    else
        (cd "$CITY_ABS" && gc bd --city "$CITY_ABS" "$@")
    fi
}

scope_bd_bounded() {
    local secs="$1"
    shift
    if [ -n "$SCOPE_RIG" ]; then
        (cd "$CITY_ABS" && run_bounded "$secs" gc bd --city "$CITY_ABS" --rig "$SCOPE_RIG" "$@")
    else
        (cd "$CITY_ABS" && run_bounded "$secs" gc bd --city "$CITY_ABS" "$@")
    fi
}

scope_database() {
    local out
    local db
    local err_file
    err_file=$(mktemp)
    out=$(scope_bd_bounded "$BACKUP_BD_TIMEOUT_SECS" sql --csv "SELECT DATABASE()" 2>"$err_file") || {
        printf '%s %s' "$(cat "$err_file" 2>/dev/null)" "$out"
        rm -f "$err_file"
        return 1
    }
    rm -f "$err_file"
    db=$(printf '%s\n' "$out" | tail -n 1 | tr -d '\r')
    case "$db" in
        '' | -* | *[!A-Za-z0-9_-]*)
            printf 'bd reported an unusable database name: %s' "$db"
            return 1
            ;;
    esac
    printf '%s' "$db"
}

same_path() {
    local left="$1"
    local right="$2"
    [ "$left" = "$right" ] && return 0
    [ "$(cd "$left" 2>/dev/null && pwd -P)" = "$(cd "$right" 2>/dev/null && pwd -P)" ] &&
        [ -n "$(cd "$left" 2>/dev/null && pwd -P)" ]
}

append_failed_db() {
    FAILED=$((FAILED + 1))
    if [ -n "$FAILED_DBS" ]; then
        FAILED_DBS="$FAILED_DBS, $1"
    else
        FAILED_DBS="$1"
    fi
}

append_failed_detail() {
    local detail_text="$2"
    [ -n "$detail_text" ] || detail_text="no diagnostic output"
    FAILED_DETAILS="$FAILED_DETAILS
  $1: $detail_text"
}

one_line() {
    tr '\n' ' ' <"$1" | sed 's/[[:space:]]\{1,\}/ /g; s/^ //; s/ $//'
}

classify_sync_failure() {
    local classify_rc="$1"
    local classify_err_file="$2"
    local classify_stderr=""
    if [ -s "$classify_err_file" ]; then
        classify_stderr=$(one_line "$classify_err_file")
    fi

    if [ "$classify_rc" -eq 124 ]; then
        printf 'timed out after %ss (raise GC_DOLT_BACKUP_SYNC_TIMEOUT_SECS)' "$BACKUP_SYNC_TIMEOUT_SECS"
        return
    fi
    case "$classify_stderr" in
        *"connection was closed"*|*"row read wait bigger than connection timeout"*)
            printf 'exit %s: %s — the sql-server ended the operation at its listener.read_timeout_millis ceiling; the backup outran it under live load, so raise that ceiling for this city or reduce contention during the backup window' \
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

# backup_unsupported reports whether bd refused backup for this scope's
# transport (bd v1.3.0 refuses `bd backup` on the proxied-server path with the
# typed code proxy.backup.unsupported).
backup_unsupported() {
    grep -q '"proxy\.backup\.unsupported"\|not supported in proxied-server mode' "$1" 2>/dev/null
}

# ensure_backup_destination registers <artifact-dir>/<db> as the scope's bd
# backup destination when bd reports none. An existing destination is left
# alone (operators may point it elsewhere). Prints a diagnostic and returns 1
# on failure, 2 when bd refuses backup for the scope.
ensure_backup_destination() {
    local db="$1"
    local err_file
    local status
    local url

    err_file=$(mktemp)
    if ! status=$(scope_bd_bounded "$BACKUP_BD_TIMEOUT_SECS" backup status --json 2>"$err_file"); then
        printf '%s\n' "$status" >>"$err_file"
        if backup_unsupported "$err_file"; then
            rm -f "$err_file"
            return 2
        fi
        printf 'backup status failed: %s' "$(one_line "$err_file")"
        rm -f "$err_file"
        return 1
    fi
    if [ "$(printf '%s' "$status" | jq -r '.dolt.configured // false' 2>/dev/null)" = "true" ]; then
        rm -f "$err_file"
        return 0
    fi
    url="file://$BACKUP_ARTIFACT_DIR/$db"
    mkdir -p "$BACKUP_ARTIFACT_DIR/$db"
    if ! scope_bd_bounded "$BACKUP_BD_TIMEOUT_SECS" backup init "$url" --json >/dev/null 2>"$err_file"; then
        if backup_unsupported "$err_file"; then
            rm -f "$err_file"
            return 2
        fi
        printf 'backup init failed: %s' "$(one_line "$err_file")"
        rm -f "$err_file"
        return 1
    fi
    rm -f "$err_file"
    echo "backup: $SCOPE_LABEL: auto-configured missing backup destination -> $url" >&2
}

sync_scope() {
    local sync_err_tmp
    local sync_attempt=1
    local sync_rc
    local sync_detail=""

    sync_err_tmp=$(mktemp) || {
        printf 'cannot create temp file for sync diagnostics'
        return 1
    }
    while [ "$sync_attempt" -le "$BACKUP_SYNC_ATTEMPTS" ]; do
        sync_rc=0
        scope_bd_bounded "$BACKUP_SYNC_TIMEOUT_SECS" backup sync --json >/dev/null 2>"$sync_err_tmp" || sync_rc=$?
        if [ "$sync_rc" -eq 0 ]; then
            if [ "$sync_attempt" -gt 1 ]; then
                echo "backup: $SCOPE_LABEL: succeeded on attempt $sync_attempt/$BACKUP_SYNC_ATTEMPTS" >&2
            fi
            rm -f "$sync_err_tmp"
            return 0
        fi
        sync_detail=$(classify_sync_failure "$sync_rc" "$sync_err_tmp")
        echo "backup: $SCOPE_LABEL: attempt $sync_attempt/$BACKUP_SYNC_ATTEMPTS failed — $sync_detail" >&2
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
            "Skipping backup sync because flock is unavailable; concurrent backup syncs can overload the sql-server." \
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

acquire_backup_lock

# GC_BACKUP_DATABASES optionally limits the run to the named scope databases
# (comma separated).
DATABASE_FILTER=""
if [ -n "${GC_BACKUP_DATABASES:-}" ]; then
    DATABASE_FILTER=$(printf '%s\n' "$GC_BACKUP_DATABASES" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | grep -v '^$' || true)
fi
database_selected() {
    [ -z "$DATABASE_FILTER" ] && return 0
    printf '%s\n' "$DATABASE_FILTER" | grep -qxF "$1"
}

SCOPE_SPECS="city"
if RIG_LIST=$(cd "$CITY_ABS" && run_bounded "$BACKUP_BD_TIMEOUT_SECS" gc rig list --json 2>/dev/null); then
    while IFS= read -r rig_name; do
        [ -n "$rig_name" ] || continue
        SCOPE_SPECS="$SCOPE_SPECS
rig $rig_name"
    # Suspended rigs are left cold: any bd call restarts a suspended scope's
    # retired proxy and Dolt, and nothing writes to it while it is suspended,
    # so its backup waits for it to resume.
    done < <(printf '%s\n' "$RIG_LIST" | jq -r '.rigs[]? | select((.hq // false) == false and (.suspended // false) == false) | .name // empty' 2>/dev/null)
else
    echo "backup: gc rig list failed; backing up the city scope only" >&2
    outcome_scope_skipped "rigs" "rig list unavailable"
fi

TOTAL=0
SYNCED=0
FAILED=0
UNSUPPORTED=0
FAILED_DBS=""
FAILED_DETAILS=""
VISITED_DBS=""

while IFS= read -r SCOPE_SPEC; do
    [ -n "$SCOPE_SPEC" ] || continue
    SCOPE_RIG=""
    SCOPE_LABEL="city"
    case "$SCOPE_SPEC" in
        "rig "*)
            SCOPE_RIG="${SCOPE_SPEC#rig }"
            SCOPE_LABEL="rig:$SCOPE_RIG"
            ;;
    esac
    if ! db=$(scope_database); then
        case "$db" in
            *"only supported for bd-backed beads providers"*)
                # Not a bd store (file- or exec-backed): nothing to back up here.
                echo "backup: $SCOPE_LABEL is not a bd bead store; skipped" >&2
                outcome_scope_skipped "$SCOPE_LABEL" "not a bd bead store"
                continue
                ;;
        esac
        TOTAL=$((TOTAL + 1))
        append_failed_db "$SCOPE_LABEL(unreachable)"
        append_failed_detail "$SCOPE_LABEL" "$db"
        outcome_scope_skipped "$SCOPE_LABEL" "bead store unreachable"
        continue
    fi
    database_selected "$db" || continue
    case "
$VISITED_DBS
" in
        *"
$db
"*) continue ;;
    esac
    VISITED_DBS="${VISITED_DBS}${db}
"
    TOTAL=$((TOTAL + 1))

    dest_rc=0
    dest_detail=$(ensure_backup_destination "$db") || dest_rc=$?
    if [ "$dest_rc" -eq 2 ]; then
        UNSUPPORTED=$((UNSUPPORTED + 1))
        echo "backup: $SCOPE_LABEL ($db): bd does not support backup for this scope's transport; skipped" >&2
        outcome_scope_skipped "$SCOPE_LABEL" "bd-backup-unsupported"
        continue
    elif [ "$dest_rc" -ne 0 ]; then
        append_failed_db "$db(backup init failed)"
        append_failed_detail "$db" "$dest_detail"
        outcome_scope_skipped "$SCOPE_LABEL" "backup destination failed"
        continue
    fi

    sync_failure_detail=""
    if sync_failure_detail=$(sync_scope); then
        SYNCED=$((SYNCED + 1))
    else
        append_failed_db "$db(sync failed)"
        append_failed_detail "$db" "$sync_failure_detail"
        outcome_scope_skipped "$SCOPE_LABEL" "backup sync failed"
    fi
done <<EOF
$SCOPE_SPECS
EOF

FAILED_COUNT=$FAILED
OFFSITE_STATUS="skipped"

# Offsite publication. Bound by GC_BACKUP_OFFSITE_TIMEOUT (default 300s) so a
# wedged NFS/SSH mount cannot eat the whole order timeout.
OFFSITE_TIMEOUT="${GC_BACKUP_OFFSITE_TIMEOUT:-300}"
case "$OFFSITE_TIMEOUT" in
    ''|*[!0-9]*|0) OFFSITE_TIMEOUT=300 ;;
esac

if [ -n "$OFFSITE_PATH" ]; then
    if [ ! -d "$BACKUP_ARTIFACT_DIR" ]; then
        OFFSITE_STATUS="missing-artifacts"
    elif same_path "$BACKUP_ARTIFACT_DIR" "$CITY_ABS/.beads/dolt"; then
        # rsync --delete from a live store's data dir would mirror (and on a
        # misconfigured offsite path, destroy) the wrong tree.
        OFFSITE_STATUS="invalid-source"
    elif run_bounded "$OFFSITE_TIMEOUT" rsync -a --delete "$BACKUP_ARTIFACT_DIR/" "$OFFSITE_PATH/" 2>/dev/null; then
        OFFSITE_STATUS="ok"
    else
        OFFSITE_STATUS="failed"
    fi
fi

if [ "$FAILED_COUNT" -gt 0 ]; then
    dolt_escalate \
        "Dolt backup: $FAILED_COUNT/$TOTAL databases failed to sync [MEDIUM]" \
        "Failed databases:$FAILED_DBS

Each scope was attempted up to $BACKUP_SYNC_ATTEMPTS times with a ${BACKUP_SYNC_TIMEOUT_SECS}s bound per attempt. Diagnostic from the final attempt:$FAILED_DETAILS

A scope listed here has no backup newer than its last successful sync, so the recoverable copy is as old as that run. Check freshness per scope with gc bd backup status." \
        2>/dev/null || true
fi

case "$OFFSITE_STATUS" in
    ok|skipped) ;;
    *)
        dolt_escalate \
            "Dolt backup: offsite publication $OFFSITE_STATUS [MEDIUM]" \
            "Local backup succeeded ($SYNCED/$TOTAL scopes) but publication to $OFFSITE_PATH did not.
Status: $OFFSITE_STATUS. Bound: ${OFFSITE_TIMEOUT}s (raise with GC_BACKUP_OFFSITE_TIMEOUT).
Raising it past the run's remaining budget also needs timeout raised in
examples/bd/dolt/orders/mol-dog-backup.toml, or the controller kills this run
mid-rsync and this escalation never fires.
Until this clears, the only copy of these databases is on this host." \
            2>/dev/null || true
        ;;
esac

if [ "$SYNCED" -eq 0 ]; then
    outcome_write skipped "no bead scope was backed up"
elif [ "$OUTCOME_SCOPES_JSON" != "[]" ] || [ "$FAILED_COUNT" -gt 0 ]; then
    outcome_write partial "some bead scopes were not backed up"
fi

SUMMARY="backup — synced: $SYNCED/$TOTAL, unsupported: $UNSUPPORTED, offsite: $OFFSITE_STATUS"
dolt_notify_done "$SUMMARY"
echo "backup: $SUMMARY"
