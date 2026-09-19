#!/usr/bin/env bash
# log-rotate.sh — Rotate large log files in /tmp/ to prevent disk exhaustion
#
# Runs on cron: 0 0 * * * (daily at midnight)
# Rotates files > 10MB, keeps 2 old copies (.1, .2), compresses with gzip.
# Only targets known GT-related log files — does not touch arbitrary /tmp files.

set -euo pipefail

LOG="/home/pratham2/gt/.mesh-activity.log"
MAX_SIZE_BYTES=$(( 10 * 1024 * 1024 ))  # 10MB
KEEP_ROTATIONS=2

log() { echo "$(date +%Y-%m-%dT%H:%M:%S) [log-rotate] $*" >> "$LOG"; }

# Known log file patterns to manage
LOG_PATTERNS=(
    "/tmp/dolt*.log"
    "/tmp/victoria-bot*.log"
    "/tmp/tunnel*.log"
    "/tmp/gt-mesh-sync.log"
    "/tmp/dolthub-sync.log"
    "/tmp/mayor-dispatcher.log"
    "/tmp/mesh-improve.log"
    "/tmp/mesh-autosync.log"
    "/tmp/cron-audit.log"
    "/tmp/mesh-mayor-daemon.log"
    "/tmp/mesh-pack-updater.log"
    "/tmp/mesh-inbox.log"
    "/tmp/mesh-watchdog.log"
    "/tmp/process-guardian.log"
    "/tmp/worker-flywheel.log"
    "/tmp/tg-approval-poll.log"
    "/tmp/linkedin-engage.log"
    "/tmp/hot-take-*.log"
    "/tmp/yolo-training*.log"
    "/tmp/dolt-hang-*.log"
)

rotate_file() {
    local filepath="$1"
    local basename
    basename=$(basename "$filepath")

    # Remove oldest rotation
    if [ -f "${filepath}.${KEEP_ROTATIONS}.gz" ]; then
        rm -f "${filepath}.${KEEP_ROTATIONS}.gz"
    fi

    # Shift existing rotations up
    local i
    for (( i=KEEP_ROTATIONS; i>1; i-- )); do
        local prev=$(( i - 1 ))
        if [ -f "${filepath}.${prev}.gz" ]; then
            mv "${filepath}.${prev}.gz" "${filepath}.${i}.gz"
        fi
    done

    # Rotate current: copy + truncate (keeps file descriptor valid for writing processes)
    cp "$filepath" "${filepath}.1"
    truncate -s 0 "$filepath"
    gzip "${filepath}.1"

    local old_size
    old_size=$(stat -c %s "${filepath}.1.gz" 2>/dev/null || echo "?")
    log "Rotated $basename (compressed to ${old_size} bytes)"
}

rotated=0
skipped=0
total_freed=0

for pattern in "${LOG_PATTERNS[@]}"; do
    # Expand glob — may match multiple files
    for filepath in $pattern; do
        [ -f "$filepath" ] || continue

        # Skip already-rotated files (.1, .2, .gz)
        case "$filepath" in
            *.gz|*.[0-9]) continue ;;
        esac

        # Skip files we don't own (can't rotate other users' logs)
        [ -O "$filepath" ] || continue
        # Skip files we can't write to
        [ -w "$filepath" ] || continue

        size=$(stat -c %s "$filepath" 2>/dev/null || echo 0)
        if [ "$size" -ge "$MAX_SIZE_BYTES" ]; then
            size_mb=$(( size / 1024 / 1024 ))
            log "$(basename "$filepath") is ${size_mb}MB — rotating"
            if rotate_file "$filepath"; then
                rotated=$(( rotated + 1 ))
                total_freed=$(( total_freed + size ))
            else
                log "Failed to rotate $(basename "$filepath")"
            fi
        else
            skipped=$(( skipped + 1 ))
        fi
    done
done

# Also clean up the mesh-activity log itself if it's huge
MESH_LOG="/home/pratham2/gt/.mesh-activity.log"
if [ -f "$MESH_LOG" ]; then
    mesh_size=$(stat -c %s "$MESH_LOG" 2>/dev/null || echo 0)
    if [ "$mesh_size" -ge "$MAX_SIZE_BYTES" ]; then
        mesh_mb=$(( mesh_size / 1024 / 1024 ))
        log ".mesh-activity.log is ${mesh_mb}MB — rotating"
        rotate_file "$MESH_LOG"
        rotated=$(( rotated + 1 ))
        total_freed=$(( total_freed + mesh_size ))
    fi
fi

freed_mb=$(( total_freed / 1024 / 1024 ))
log "Log rotation complete: $rotated rotated, $skipped under threshold, ~${freed_mb}MB freed"
exit 0
