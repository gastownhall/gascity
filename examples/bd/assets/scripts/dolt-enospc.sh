#!/bin/sh

# Dolt restart/recovery guard shared by the managed lifecycle commands.
#
# ENOSPC evidence must belong to the current Dolt launch. A match before that
# launch's durable start boundary is stale and does not block recovery, even
# after the managed process has exited. Fresh or unparseable evidence fails
# closed.
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

    # dolt-provider-state.json is written with a pre-launch timestamp and
    # survives process death. Require its PID and running marker to match the
    # retained PID file so a stopped/prior launch cannot become the boundary.
    _dolt_state_epoch=""
    _dolt_state_pid=$(dolt_provider_state_field pid) || _dolt_state_pid=""
    _dolt_state_running=$(dolt_provider_state_field running) || _dolt_state_running=""
    _dolt_state_started=$(dolt_provider_state_field started_at) || _dolt_state_started=""
    if [ "$_dolt_state_pid" = "$_dolt_pid" ] && [ "$_dolt_state_running" = "true" ] && \
        [ -n "$_dolt_state_started" ]; then
        _dolt_state_epoch=$(dolt_epoch_from_rfc3339 "$_dolt_state_started") || _dolt_state_epoch=""
    fi

    # A live PID only contributes a kernel-backed boundary when it is still a
    # Dolt process. A crashed launch leaves its PID file behind, and a recycled
    # PID must never move the boundary past the recorded pre-launch time.
    if kill -0 "$_dolt_pid" 2>/dev/null; then
        _dolt_comm=$(LC_ALL=C ps -p "$_dolt_pid" -o comm= 2>/dev/null \
            | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        case "$_dolt_comm" in
            *dolt*)
                _dolt_started=$(LC_ALL=C ps -p "$_dolt_pid" -o lstart= 2>/dev/null \
                    | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
                _dolt_ps_epoch=""
                if [ -n "$_dolt_started" ]; then
                    _dolt_ps_epoch=$(dolt_epoch_from_ps_lstart "$_dolt_started") || _dolt_ps_epoch=""
                fi
                if [ -n "$_dolt_ps_epoch" ]; then
                    if [ -n "$_dolt_state_epoch" ] && [ "$_dolt_state_epoch" -lt "$_dolt_ps_epoch" ]; then
                        printf '%s\n' "$_dolt_state_epoch"
                    else
                        printf '%s\n' "$_dolt_ps_epoch"
                    fi
                    return 0
                fi
                ;;
        esac
    fi

    [ -n "$_dolt_state_epoch" ] || return 1
    printf '%s\n' "$_dolt_state_epoch"
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
# It returns 1 when the log has no ENOSPC evidence from the current launch. Each
# blocking path sets a precise diagnostic for the caller.
recovery_should_skip_due_to_enospc() {
    # shellcheck disable=SC2034 # Read by the scripts that source this helper.
    DOLT_ENOSPC_GUARD_REASON=""
    _dolt_guard_reason=$(dolt_enospc_log_guard_reason) && {
        DOLT_ENOSPC_GUARD_REASON="$_dolt_guard_reason"
        return 0
    }
    return 1
}
