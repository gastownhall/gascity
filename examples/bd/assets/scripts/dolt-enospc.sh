#!/bin/sh

# recovery_should_skip_due_to_enospc returns 0 (true) when the recent Dolt
# server log shows disk-exhaustion (ENOSPC) signatures and the data dir is
# still short of space.
#
# Restarting Dolt under ENOSPC does not free disk space, and the recovery cycle
# itself amplifies the failure: every restart triggers a fresh conjoin that can
# drop another partial nbs_table_* file in the backup remote, accelerating the
# disk-full feedback loop. See gastownhall/gascity#2158.
#
# The log alone cannot say the disk is still full: a stopped server writes
# nothing, so its last ENOSPC line stays in the tail for good. The evidence
# therefore counts only while free space on the data dir (DATA_DIR, else the
# log's directory) is below GC_DOLT_WARN_FREE_BYTES (default 2 GiB, the level
# below which gc warns about Dolt disk space; unset, malformed or 0 means the
# default). When free space cannot be read, the evidence counts.
#
# Returns 1 (false) when no ENOSPC signature is found in the recent log tail,
# when free space is back at or above the floor, or when LOG_FILE is unset or
# unreadable.
recovery_should_skip_due_to_enospc() {
    [ -n "${LOG_FILE:-}" ] && [ -r "$LOG_FILE" ] || return 1
    # Scan the recent log tail rather than the whole file; an old transient
    # ENOSPC error from a since-resolved disk-pressure event should not block
    # recovery indefinitely.
    tail -n 1000 "$LOG_FILE" 2>/dev/null \
        | grep -qE 'no space left on device|copy_file_range:.*no space|ENOSPC' \
        || return 1
    enospc_disk_has_headroom && return 1
    return 0
}

# enospc_disk_has_headroom returns 0 (true) when free space on the data dir is
# at or above GC_DOLT_WARN_FREE_BYTES, and 1 when it is below or unreadable.
enospc_disk_has_headroom() {
    enospc_dir="${DATA_DIR:-}"
    if [ -z "$enospc_dir" ] || [ ! -d "$enospc_dir" ]; then
        enospc_dir=$(dirname "$LOG_FILE")
    fi
    enospc_floor="${GC_DOLT_WARN_FREE_BYTES:-2147483648}"
    case "$enospc_floor" in
        ''|*[!0-9]*) enospc_floor=2147483648 ;;
    esac
    # 0 means "no disk warning" to gc; it must not switch the skip off.
    [ "$enospc_floor" -gt 0 ] || enospc_floor=2147483648
    enospc_avail_kb=$(df -Pk "$enospc_dir" 2>/dev/null | awk 'NR == 2 { print $4 }')
    case "$enospc_avail_kb" in
        ''|*[!0-9]*) return 1 ;;
    esac
    [ "$enospc_avail_kb" -ge $((enospc_floor / 1024)) ]
}
