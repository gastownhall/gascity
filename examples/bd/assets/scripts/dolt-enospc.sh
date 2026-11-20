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
# therefore counts only while free space on the probe directory (DATA_DIR,
# else the log's directory) is below the floor GC_DOLT_WARN_FREE_BYTES, read
# as decimal bytes. Unset, empty, non-digit or 0 means the default, 2 GiB
# (gc's doltDiskDefaultWarnFreeBytes): 0 tells gc not to warn about disk
# space, and silencing that warning must not remove this restart-loop
# protection.
#
# When free space cannot be read (the probe directory does not exist, df
# fails, or df prints no Available value), the evidence counts, so recovery
# stays held as it was before the free-space check (#2158), and one stderr
# line names the directory.
#
# Sets ENOSPC_PROBE_DIR, ENOSPC_FREE_BYTES (empty when unmeasured) and
# ENOSPC_FLOOR_BYTES for the caller's messages.
#
# Returns 1 (false) when no ENOSPC signature is found in the recent log tail,
# when free space is back at or above the floor, or when LOG_FILE is unset or
# unreadable. Kept as one function so gc-beads-bd.sh's inline copy stays a
# verbatim duplicate.
recovery_should_skip_due_to_enospc() {
    ENOSPC_PROBE_DIR=""
    ENOSPC_FREE_BYTES=""
    ENOSPC_FLOOR_BYTES=""
    [ -n "${LOG_FILE:-}" ] && [ -r "$LOG_FILE" ] || return 1
    # Scan the recent log tail rather than the whole file; an old transient
    # ENOSPC error from a since-resolved disk-pressure event should not block
    # recovery indefinitely.
    tail -n 1000 "$LOG_FILE" 2>/dev/null \
        | grep -qE 'no space left on device|copy_file_range:.*no space|ENOSPC' \
        || return 1
    ENOSPC_PROBE_DIR="${DATA_DIR:-}"
    [ -n "$ENOSPC_PROBE_DIR" ] || ENOSPC_PROBE_DIR=$(dirname "$LOG_FILE")
    ENOSPC_FLOOR_BYTES="${GC_DOLT_WARN_FREE_BYTES:-}"
    case "$ENOSPC_FLOOR_BYTES" in
        ''|*[!0-9]*) ENOSPC_FLOOR_BYTES=2147483648 ;;
    esac
    ENOSPC_FLOOR_BYTES=$(printf '%s' "$ENOSPC_FLOOR_BYTES" | sed 's/^0*//')
    [ -n "$ENOSPC_FLOOR_BYTES" ] || ENOSPC_FLOOR_BYTES=2147483648
    [ "${#ENOSPC_FLOOR_BYTES}" -le 18 ] || ENOSPC_FLOOR_BYTES=2147483648
    if [ -d "$ENOSPC_PROBE_DIR" ] \
        && ENOSPC_FREE_BYTES=$(df -Pk "$ENOSPC_PROBE_DIR" 2>/dev/null); then
        # Available is the field before the first NN% field, so a filesystem
        # name with spaces still parses; KiB become bytes in awk, not in shell
        # arithmetic.
        ENOSPC_FREE_BYTES=$(printf '%s\n' "$ENOSPC_FREE_BYTES" | awk '
            NR > 1 {
                for (i = 2; i <= NF; i++) {
                    if ($i ~ /^[0-9]+%$/ && $(i - 1) ~ /^-?[0-9]+$/) {
                        kb = $(i - 1) + 0
                        if (kb < 0) kb = 0
                        printf "%.0f\n", kb * 1024
                        exit
                    }
                }
            }')
    else
        ENOSPC_FREE_BYTES=""
    fi
    if [ -z "$ENOSPC_FREE_BYTES" ]; then
        echo "warning: cannot read free space on $ENOSPC_PROBE_DIR; treating the ENOSPC evidence in $LOG_FILE as current" >&2
        return 0
    fi
    awk -v free="$ENOSPC_FREE_BYTES" -v floor="$ENOSPC_FLOOR_BYTES" \
        'BEGIN { exit !(free + 0 < floor + 0) }'
}
