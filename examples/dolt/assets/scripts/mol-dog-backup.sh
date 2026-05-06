#!/usr/bin/env bash
# mol-dog-backup — sync Dolt databases to backup remotes and offsite storage.
#
# Replaces mol-dog-backup formula. All operations are deterministic:
# dolt backup sync per DB, rsync to offsite path. No LLM judgment needed.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

PORT="${GC_DOLT_PORT:-3307}"
HOST="${GC_DOLT_HOST:-127.0.0.1}"
USER="${GC_DOLT_USER:-root}"
OFFSITE_PATH="${GC_BACKUP_OFFSITE_PATH:-}"
SYSTEM_DBS="^information_schema$\|^mysql$\|^dolt_cluster$\|^__gc_probe$"

dolt_sql() {
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" \
        run_bounded 30 \
        dolt --host "$HOST" --port "$PORT" --user "$USER" --no-tls sql "$@"
}

# --- Step 1: Sync databases to backup remotes ---

# If GC_BACKUP_DATABASES is set, use it; otherwise auto-discover DBs that
# have a <db>-backup remote configured.
if [ -n "${GC_BACKUP_DATABASES:-}" ]; then
    DATABASES=$(echo "$GC_BACKUP_DATABASES" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | grep -v '^$' || true)
else
    # Auto-discover: find databases that have a backup remote named <db>-backup.
    ALL_DBS=$(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2 | \
        grep -vi "$SYSTEM_DBS" || true)
    DATABASES=""
    for db in $ALL_DBS; do
        db_dir="$DOLT_DATA_DIR/$db"
        if [ -d "$db_dir/.dolt" ]; then
            if (cd "$db_dir" && dolt remote 2>/dev/null | grep -q "^${db}-backup$"); then
                DATABASES="$DATABASES $db"
            fi
        fi
    done
    DATABASES=$(echo "$DATABASES" | tr ' ' '\n' | grep -v '^$' || true)
fi

if [ -z "$DATABASES" ]; then
    echo "backup: no databases with backup remotes found, skipping"
    exit 0
fi

TOTAL=$(echo "$DATABASES" | grep -c . || echo "0")
SYNCED=0
FAILED_DBS=""

for db in $DATABASES; do
    db_dir="$DOLT_DATA_DIR/$db"
    if [ ! -d "$db_dir" ]; then
        FAILED_DBS="$FAILED_DBS $db(not found)"
        continue
    fi
    if (cd "$db_dir" && run_bounded 120 dolt backup sync "${db}-backup" 2>/dev/null); then
        SYNCED=$((SYNCED + 1))
    else
        FAILED_DBS="$FAILED_DBS $db(sync failed)"
    fi
done

FAILED_COUNT=$(echo "$FAILED_DBS" | tr ' ' '\n' | grep -c . 2>/dev/null || echo "0")
OFFSITE_STATUS="skipped"

# --- Step 2: Rsync to offsite storage ---

if [ -n "$OFFSITE_PATH" ] && [ -d "$DOLT_DATA_DIR" ]; then
    if run_bounded 300 rsync -a --delete "$DOLT_DATA_DIR/" "$OFFSITE_PATH/" 2>/dev/null; then
        OFFSITE_STATUS="ok"
    else
        OFFSITE_STATUS="failed (non-fatal)"
    fi
fi

# --- Step 3: Report ---

if [ "$FAILED_COUNT" -gt 0 ]; then
    gc mail send mayor/ \
        -s "Backup dog: $FAILED_COUNT/$TOTAL databases failed to sync [MEDIUM]" \
        -m "Failed databases:$FAILED_DBS" \
        2>/dev/null || true
fi

SUMMARY="backup — synced: $SYNCED/$TOTAL, offsite: $OFFSITE_STATUS"
gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "backup: $SUMMARY"
