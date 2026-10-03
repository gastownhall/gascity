#!/bin/sh

# Dolt restart/recovery guard shared by the managed lifecycle commands.
#
# The guard has two independent inputs:
#   1. ENOSPC evidence must belong to the current Dolt launch. A match before
#      that launch's durable start boundary is stale and does not block
#      recovery, even after the managed process has exited.
#   2. The filesystem holding the live databases must have at least twice the
#      allocated size of the largest database available. Dolt conjoin writes a
#      replacement before deleting its inputs, so less headroom can reproduce
#      the disk-full cascade even when the log has no ENOSPC match yet.
#
# Callers inspect DOLT_ENOSPC_GUARD_REASON after a true (0) return from
# recovery_should_skip_due_to_enospc. --force remains the caller-owned,
# explicit operator override.

# shellcheck disable=SC2034 # Read by the scripts that source this helper.
DOLT_ENOSPC_GUARD_REASON=""

dolt_epoch_from_rfc3339() (
    _dolt_raw="$1"

    # GNU date accepts RFC3339 directly. Strip fractional seconds first so the
    # same normalized value can fall through to BSD date on macOS.
    _dolt_normalized=$(printf '%s\n' "$_dolt_raw" \
        | sed -E 's/\.[0-9]+(Z|[+-][0-9][0-9]:?[0-9][0-9])$/\1/')
    if LC_ALL=C date -d "$_dolt_normalized" +%s >/dev/null 2>&1; then
        LC_ALL=C date -d "$_dolt_normalized" +%s
        return 0
    fi

    _dolt_normalized=$(printf '%s\n' "$_dolt_normalized" \
        | sed -E 's/Z$/+0000/; s/([+-][0-9][0-9]):([0-9][0-9])$/\1\2/')
    LC_ALL=C date -j -f '%Y-%m-%dT%H:%M:%S%z' "$_dolt_normalized" +%s 2>/dev/null
)

dolt_epoch_from_ps_lstart() (
    _dolt_raw="$1"

    if LC_ALL=C date -d "$_dolt_raw" +%s >/dev/null 2>&1; then
        LC_ALL=C date -d "$_dolt_raw" +%s
        return 0
    fi
    LC_ALL=C date -j -f '%a %b %e %T %Y' "$_dolt_raw" +%s 2>/dev/null
)

dolt_provider_state_field() (
    [ -n "${STATE_FILE:-}" ] && [ -r "$STATE_FILE" ] || return 1
    _dolt_field="$1"
    sed -n 's/.*"'"$_dolt_field"'"[[:space:]]*:[[:space:]]*"\{0,1\}\([^",}]*\)"\{0,1\}.*/\1/p' "$STATE_FILE" \
        | head -1
)

dolt_current_launch_start_epoch() (
    [ -n "${PID_FILE:-}" ] && [ -r "$PID_FILE" ] || return 1
    IFS= read -r _dolt_pid < "$PID_FILE" || return 1
    case "$_dolt_pid" in
        ''|*[!0-9]*) return 1 ;;
    esac

    # Prefer the kernel-backed boundary while the process is alive. Recovery
    # normally runs after death, so fall back to provider state below.
    if kill -0 "$_dolt_pid" 2>/dev/null; then
        _dolt_started=$(LC_ALL=C ps -p "$_dolt_pid" -o lstart= 2>/dev/null \
            | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        if [ -n "$_dolt_started" ]; then
            dolt_epoch_from_ps_lstart "$_dolt_started" && return 0
        fi
    fi

    # dolt-provider-state.json is written with a pre-launch timestamp and
    # survives process death. Require its PID and running marker to match the
    # retained PID file so a stopped/prior launch cannot become the boundary.
    _dolt_state_pid=$(dolt_provider_state_field pid) || return 1
    _dolt_state_running=$(dolt_provider_state_field running) || return 1
    _dolt_state_started=$(dolt_provider_state_field started_at) || return 1
    [ "$_dolt_state_pid" = "$_dolt_pid" ] || return 1
    [ "$_dolt_state_running" = "true" ] || return 1
    [ -n "$_dolt_state_started" ] || return 1
    dolt_epoch_from_rfc3339 "$_dolt_state_started"
)

dolt_available_kib() (
    df -Pk "$1" 2>/dev/null \
        | awk 'NR > 1 { available = $4 } END { if (available ~ /^[0-9]+$/) print available }'
)

dolt_largest_database_kib() (
    _dolt_data_dir="$1"
    _dolt_largest=0

    for _dolt_database in "$_dolt_data_dir"/*; do
        [ -d "$_dolt_database" ] || continue
        _dolt_size=$(du -sk "$_dolt_database" 2>/dev/null | awk 'NR == 1 { print $1 }') || return 1
        case "$_dolt_size" in
            ''|*[!0-9]*) return 1 ;;
        esac
        if [ "$_dolt_size" -gt "$_dolt_largest" ]; then
            _dolt_largest="$_dolt_size"
        fi
    done
    printf '%s\n' "$_dolt_largest"
)

dolt_disk_headroom_guard_reason() (
    # No database directory means there is no existing store to conjoin.
    [ -n "${DATA_DIR:-}" ] && [ -d "$DATA_DIR" ] || return 1
    _dolt_largest=$(dolt_largest_database_kib "$DATA_DIR") || {
        printf 'cannot measure Dolt database sizes under %s\n' "$DATA_DIR"
        return 0
    }
    _dolt_available=$(dolt_available_kib "$DATA_DIR") || true
    case "$_dolt_available" in
        ''|*[!0-9]*)
            printf 'cannot determine free disk headroom for %s\n' "$DATA_DIR"
            return 0
            ;;
    esac
    _dolt_required=$((_dolt_largest * 2))
    if [ "$_dolt_available" -lt "$_dolt_required" ]; then
        printf 'insufficient Dolt disk headroom: available=%s KiB required=%s KiB (2x largest database=%s KiB) under %s\n' \
            "$_dolt_available" "$_dolt_required" "$_dolt_largest" "$DATA_DIR"
        return 0
    fi
    return 1
)

dolt_enospc_log_guard_reason() (
    [ -n "${LOG_FILE:-}" ] && [ -r "$LOG_FILE" ] || return 1
    _dolt_matches=$(tail -n 1000 "$LOG_FILE" 2>/dev/null | awk '
        {
            if (match($0, /time="[^"]*"/)) {
                context_timestamp = substr($0, RSTART + 6, RLENGTH - 7)
            } else if ($0 ~ /time=/) {
                context_timestamp = "__INVALID__"
            }
            if ($0 ~ /no space left on device|copy_file_range:.*no space|ENOSPC/) {
                timestamp = context_timestamp
                if ($0 ~ /time=/ && !match($0, /time="[^"]*"/)) {
                    timestamp = "__INVALID__"
                }
                printf "%d|%s\n", NR, timestamp
            }
        }
    ')
    [ -n "$_dolt_matches" ] || return 1

    _dolt_start_epoch=$(dolt_current_launch_start_epoch) || {
        printf 'cannot determine managed Dolt launch boundary while ENOSPC exists in the last 1000 log lines\n'
        return 0
    }

    while IFS='|' read -r _dolt_line_number _dolt_timestamp; do
        case "$_dolt_timestamp" in
            ''|__INVALID__)
                printf 'cannot parse ENOSPC log timestamp at tail line %s; refusing recovery fail-closed\n' "$_dolt_line_number"
                return 0
                ;;
        esac
        _dolt_match_epoch=$(dolt_epoch_from_rfc3339 "$_dolt_timestamp") || {
            printf 'cannot parse ENOSPC log timestamp %s at tail line %s; refusing recovery fail-closed\n' \
                "$_dolt_timestamp" "$_dolt_line_number"
            return 0
        }
        if [ "$_dolt_match_epoch" -ge "$_dolt_start_epoch" ]; then
            printf 'ENOSPC at or after managed Dolt launch: timestamp=%s tail_line=%s\n' \
                "$_dolt_timestamp" "$_dolt_line_number"
            return 0
        fi
    done <<DOLT_ENOSPC_MATCHES
$_dolt_matches
DOLT_ENOSPC_MATCHES
    return 1
)

# recovery_should_skip_due_to_enospc returns 0 (true) when recovery is unsafe.
# It returns 1 only when both the log classification and disk-headroom check are
# safe. Each blocking path sets a precise diagnostic for the caller.
recovery_should_skip_due_to_enospc() {
    # shellcheck disable=SC2034 # Read by the scripts that source this helper.
    DOLT_ENOSPC_GUARD_REASON=""
    _dolt_guard_reason=$(dolt_disk_headroom_guard_reason) && {
        DOLT_ENOSPC_GUARD_REASON="$_dolt_guard_reason"
        return 0
    }
    _dolt_guard_reason=$(dolt_enospc_log_guard_reason) && {
        DOLT_ENOSPC_GUARD_REASON="$_dolt_guard_reason"
        return 0
    }
    return 1
}
