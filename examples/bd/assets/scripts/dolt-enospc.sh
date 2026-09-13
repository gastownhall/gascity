#!/bin/sh

# recovery_should_skip_due_to_enospc returns 0 (true) when the recent Dolt
# server log shows disk-exhaustion (ENOSPC) signatures.
#
# Restarting Dolt under ENOSPC does not free disk space, and the recovery cycle
# itself amplifies the failure: every restart triggers a fresh conjoin that can
# drop another partial nbs_table_* file in the backup remote, accelerating the
# disk-full feedback loop. See gastownhall/gascity#2158.
#
# Returns 1 (false) when no ENOSPC signature is found in the recent log tail, or
# when LOG_FILE is unset or unreadable.
recovery_should_skip_due_to_enospc() {
    [ -n "${LOG_FILE:-}" ] && [ -r "$LOG_FILE" ] || return 1
    # Scan the recent log tail rather than the whole file; an old transient
    # ENOSPC error from a since-resolved disk-pressure event should not block
    # recovery indefinitely.
    tail -n 1000 "$LOG_FILE" 2>/dev/null \
        | grep -qE 'no space left on device|copy_file_range:.*no space|ENOSPC' \
        || return 1
    return 0
}
