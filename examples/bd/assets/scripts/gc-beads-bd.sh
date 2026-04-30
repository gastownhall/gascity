#!/bin/sh
# gc-beads-bd — exec: beads provider for Dolt-backed beads (bd).
#
# Implements the exec beads lifecycle protocol:
#   gc-beads-bd <operation> [args...]
#
# Operations: start, ensure-ready, stop, shutdown, init, health, recover, probe
# Exit codes: 0 = success, 1 = error, 2 = not needed / not running
#
# Environment:
#   GC_CITY_PATH  — city root directory (required for all operations)
#   GC_CITY_RUNTIME_DIR — canonical hidden runtime root (optional)
#   GC_PACK_STATE_DIR — canonical pack runtime root for dolt (optional)
#   GC_DOLT       — set to "skip" to no-op all operations (exit 2)
#   GC_DOLT_HOST  — dolt server host (empty = local server)
#   GC_DOLT_PORT  — dolt server port (default: ephemeral, hashed from city path)
#   GC_DOLT_USER  — dolt user (default: root)
#   GC_DOLT_PASSWORD — dolt password (default: empty)
#   GC_DOLT_CONCURRENT_START_READY_TIMEOUT_MS — concurrent-start wait budget in milliseconds (default: 45000)

set -e

# --- Configuration ---

# DOLT_PORT is set after derived paths are resolved (see allocate_port below).
DOLT_HOST="${GC_DOLT_HOST:-0.0.0.0}"
DOLT_USER="${GC_DOLT_USER:-root}"
DOLT_PASSWORD="${GC_DOLT_PASSWORD:-}"
DOLT_LOGLEVEL="${GC_DOLT_LOGLEVEL:-warning}"
LSOF_TIMEOUT_SECONDS="${GC_LSOF_TIMEOUT_SECONDS:-2}"
CONCURRENT_START_READY_TIMEOUT_MS="${GC_DOLT_CONCURRENT_START_READY_TIMEOUT_MS:-45000}"

# Derived paths (set after GC_CITY_PATH validation).
GC_DIR=""
PACK_STATE_DIR=""
DATA_DIR=""
LOG_FILE=""
STATE_FILE=""
PID_FILE=""
LOCK_FILE=""
CONFIG_FILE=""

# --- Helpers ---

die() {
    echo "$@" >&2
    exit 1
}

resolve_gc_helper_bin() {
    if [ -n "${GC_BIN:-}" ]; then
        printf '%s\n' "$GC_BIN"
    fi
    return 0
}

resolve_gc_bin() {
    if [ -n "${GC_BIN:-}" ]; then
        printf '%s\n' "$GC_BIN"
        return 0
    fi
    command -v gc 2>/dev/null || true
}

# is_remote returns 0 (true) when GC_DOLT_HOST explicitly names a target.
# Only the empty/default bind host means GC owns a local managed server.
is_remote() {
    [ -n "$GC_DOLT_HOST" ] && [ "$GC_DOLT_HOST" != "0.0.0.0" ]
}

# connect_host returns the host to connect to (loopback IPv4 for local servers).
# Using 127.0.0.1 avoids localhost -> ::1 resolution mismatches when the
# managed Dolt server is only listening on IPv4.
connect_host() {
    if is_remote; then
        echo "$GC_DOLT_HOST"
    else
        echo "127.0.0.1"
    fi
}

# tcp_check_port returns 0 if the given port is reachable.
tcp_check_port() {
    local port="$1"
    local host
    host=$(connect_host)
    if command -v nc >/dev/null 2>&1; then
        nc -z -w2 "$host" "$port" 2>/dev/null
    elif command -v bash >/dev/null 2>&1; then
        bash -c "echo >/dev/tcp/$host/$port" 2>/dev/null
    else
        return 1
    fi
}

# tcp_check returns 0 if the dolt port is reachable.
tcp_check() {
    tcp_check_port "$DOLT_PORT"
}

run_with_timeout() {
    local timeout_seconds="$1"
    shift
    "$@" &
    local cmd_pid=$!
    (
        sleep "$timeout_seconds" 2>/dev/null || sleep 1
        kill "$cmd_pid" 2>/dev/null || true
    ) &
    local watchdog_pid=$!
    local status=0
    wait "$cmd_pid" || status=$?
    kill "$watchdog_pid" 2>/dev/null || true
    wait "$watchdog_pid" 2>/dev/null || true
    return "$status"
}

run_lsof() {
    command -v lsof >/dev/null 2>&1 || return 127
    run_with_timeout "$LSOF_TIMEOUT_SECONDS" lsof "$@"
}

lsof_reports_open() {
    local status
    run_lsof "$@" >/dev/null 2>&1
    status=$?
    case "$status" in
        0) return 0 ;;
        1) return 1 ;;
        *) return 2 ;;
    esac
}

canonical_dir() {
    local dir="$1"
    (cd "$dir" 2>/dev/null && pwd -P) || printf '%s\n' "$dir"
}

same_dir_path() {
    local left="$1" right="$2" abs_left abs_right
    [ "$left" = "$right" ] && return 0
    abs_left=$(canonical_dir "$left")
    abs_right=$(canonical_dir "$right")
    [ "$abs_left" = "$abs_right" ]
}

path_under_data_dir() {
    local path="$1" abs_data
    abs_data=$(canonical_dir "$DATA_DIR")
    case "$path" in
        "$DATA_DIR"|"$DATA_DIR"/*|"$abs_data"|"$abs_data"/*)
            return 0
            ;;
    esac
    return 1
}

# do_query_probe runs a SELECT active_branch() query against the dolt server.
# active_branch() is lightweight and won't block behind queued queries,
# unlike SELECT 1 which goes through the full query executor (per Tim Sehn, Dolt CEO).
do_query_probe() {
    local host gc_bin
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        "$gc_bin" dolt-state query-probe --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" >/dev/null 2>&1
        return $?
    fi
    dolt --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --password "${DOLT_PASSWORD:-}" --no-tls         sql -q "SELECT active_branch()" >/dev/null 2>&1
}

# server_sql runs a SQL query against the running dolt server.
# Returns 0 on success, 1 on failure. Stdout contains query output,
# stderr contains error messages (callers must redirect as needed).
server_sql() {
    local host
    host=$(connect_host)
    dolt --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --password "${DOLT_PASSWORD:-}" --no-tls \
        sql -q "$1"
}

# is_retryable_error checks if an error message is a transient Dolt failure worth retrying.
# Matches the 7 patterns from upstream isDoltRetryableError().
is_retryable_error() {
    case "$1" in
        *"database is read only"*) return 0 ;;
        *"cannot update manifest"*) return 0 ;;
        *"optimistic lock"*) return 0 ;;
        *"serialization failure"*) return 0 ;;
        *"lock wait timeout"*) return 0 ;;
        *"try restarting transaction"*) return 0 ;;
        *"Unknown database"*) return 0 ;;
    esac
    return 1
}

sleep_ms() {
    local ms="$1"
    local seconds remainder
    seconds=$((ms / 1000))
    remainder=$((ms % 1000))
    if [ "$remainder" -eq 0 ]; then
        sleep "$seconds"
    else
        sleep "$seconds.$(printf '%03d' "$remainder")"
    fi
}

# server_sql_retry wraps server_sql with exponential backoff on transient errors.
# 5 attempts, backoff 500ms→1s→2s→4s→8s (capped at 15s).
server_sql_retry() {
    local query="$1"
    local attempt=1
    local max_attempts=5
    local backoff_ms=500
    local max_backoff_ms=15000
    local output

    while [ "$attempt" -le "$max_attempts" ]; do
        output=$(server_sql "$query" 2>&1) && return 0

        if ! is_retryable_error "$output"; then
            echo "$output" >&2
            return 1
        fi

        if [ "$attempt" -lt "$max_attempts" ]; then
            sleep_ms "$backoff_ms" 2>/dev/null || sleep 1
            backoff_ms=$((backoff_ms * 2))
            if [ "$backoff_ms" -gt "$max_backoff_ms" ]; then
                backoff_ms=$max_backoff_ms
            fi
        fi
        attempt=$((attempt + 1))
    done

    echo "after $max_attempts retries: $output" >&2
    return 1
}

# ensure_database_registered creates the database on the running server if
# it doesn't already exist. Dolt's CREATE DATABASE both creates the on-disk
# directory and registers it in the server's in-memory catalog. If the
# directory already exists (from bd init), CREATE DATABASE IF NOT EXISTS
# adopts it. Without this, databases created by bd init on disk are
# invisible to the running server.
#
# After CREATE DATABASE, polls with USE <db> to wait for catalog propagation
# (CREATE DATABASE returns before the catalog is fully updated).
ensure_database_registered() {
    local db="$1"

    # Validate database name before SQL interpolation (upstream 38f7b380).
    if ! valid_sql_name "$db"; then
        echo "error: invalid database name: $db" >&2
        return 1
    fi

    # Check if already visible.
    if server_sql "USE \`$db\`" >/dev/null 2>&1; then
        return 0
    fi

    # Register with the server (use retry for lock contention).
    local reg_err
    if ! reg_err=$(server_sql_retry "CREATE DATABASE IF NOT EXISTS \`$db\`" 2>&1 >/dev/null); then
        echo "warning: CREATE DATABASE $db failed: $reg_err" >&2
        return 1
    fi

    # Wait for catalog propagation (exponential backoff: 100ms → 200ms → 400ms → 800ms → 1.6s).
    local attempt backoff_ms
    backoff_ms=100
    for attempt in 1 2 3 4 5; do
        if server_sql "USE \`$db\`" >/dev/null 2>&1; then
            return 0
        fi
        sleep_ms "$backoff_ms" 2>/dev/null || sleep 1
        backoff_ms=$((backoff_ms * 2))
    done

    echo "warning: database $db not visible after 5 catalog probes" >&2
    return 1
}


database_exists() {
    local db="$1"
    [ -n "$db" ] || return 1

    if ! valid_sql_name "$db"; then
        return 1
    fi

    server_sql "USE \`$db\`" >/dev/null 2>&1
}

read_existing_dolt_database() {
    local meta_file="$1"
    [ -f "$meta_file" ] || return 0

    if command -v jq >/dev/null 2>&1; then
        jq -r '.dolt_database // empty' "$meta_file" 2>/dev/null || true
        return 0
    fi

    grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta_file" 2>/dev/null |         sed 's/.*"dolt_database"[[:space:]]*:[[:space:]]*"//;s/"//' || true
}

metadata_has_project_id() {
    local meta_file="$1"
    [ -f "$meta_file" ] || return 1
    grep -q '"project_id"[[:space:]]*:' "$meta_file" 2>/dev/null
}

backfill_project_id_if_missing() {
    local dir="$1" meta_file gc_bin dolt_database host
    meta_file="$dir/.beads/metadata.json"
    if metadata_has_project_id "$meta_file"; then
        return 0
    fi
    run_bd_pinned "$dir" migrate --update-repo-id 2>/dev/null || true
    if metadata_has_project_id "$meta_file"; then
        return 0
    fi
    gc_bin=$(resolve_gc_helper_bin)
    if [ -z "$gc_bin" ]; then
        return 0
    fi
    dolt_database=$(read_existing_dolt_database "$meta_file")
    if [ -z "$dolt_database" ]; then
        return 0
    fi
    host=$(connect_host)
    "$gc_bin" dolt-state ensure-project-id         --metadata "$meta_file"         --host "$host"         --port "$DOLT_PORT"         --user "$DOLT_USER"         --database "$dolt_database" >/dev/null || die "failed to ensure project identity for $dir"
}

ensure_bd_runtime_issue_prefix() {
    local db="$1"
    local prefix="$2"
    ensure_bd_runtime_config_value "$db" "issue_prefix" "$prefix"
}

valid_custom_types_value() {
    local types="$1" old_ifs typ
    [ -n "$types" ] || return 1
    old_ifs=$IFS
    IFS=','
    for typ in $types; do
        [ -n "$typ" ] || { IFS=$old_ifs; return 1; }
        valid_sql_name "$typ" || { IFS=$old_ifs; return 1; }
    done
    IFS=$old_ifs
    return 0
}

ensure_bd_runtime_custom_types() {
    local db="$1"
    local types="$2"
    ensure_bd_runtime_config_value "$db" "types.custom" "$types"
}

ensure_bd_runtime_config_value() {
    local db="$1"
    local key="$2"
    local value="$3"
    [ -n "$db" ] || return 0
    [ -n "$value" ] || return 0
    valid_sql_name "$db" || die "invalid dolt database name: $db"
    case "$key" in
        issue_prefix)
            valid_sql_name "$value" || die "invalid beads prefix: $value"
            ;;
        types.custom)
            valid_custom_types_value "$value" || die "invalid custom bead types: $value"
            ;;
        *)
            die "unsupported bd runtime config key: $key"
            ;;
    esac

    # bd v1.0.3 rejects `bd config set issue_prefix`; GC still needs raw
    # bd commands to see GC's config in the DB-backed config table.
    server_sql_retry "USE \`$db\`; INSERT INTO config (\`key\`, value) VALUES ('$key', '$value') ON DUPLICATE KEY UPDATE value = VALUES(value)" >/dev/null || die "failed to set bd runtime $key for $db"
}

bd_runtime_schema_ready() {
    local db="$1"
    [ -n "$db" ] || return 1
    valid_sql_name "$db" || return 1
    server_sql "USE \`$db\`; SELECT 1 FROM config LIMIT 1" >/dev/null 2>&1
}

wait_for_bd_runtime_schema() {
    local db="$1"
    local attempt backoff_ms
    [ -n "$db" ] || return 1
    valid_sql_name "$db" || return 1

    backoff_ms=100
    for attempt in 1 2 3 4 5 6 7 8; do
        if bd_runtime_schema_ready "$db"; then
            return 0
        fi
        if [ "$attempt" -lt 8 ]; then
            sleep_ms "$backoff_ms" 2>/dev/null || sleep 1
            if [ "$backoff_ms" -lt 1000 ]; then
                backoff_ms=$((backoff_ms * 2))
                if [ "$backoff_ms" -gt 1000 ]; then
                    backoff_ms=1000
                fi
            fi
        fi
    done

    return 1
}

# --- Robustness Helpers ---

# save_state writes the private provider runtime state atomically (no jq dependency).
save_state() {
    local pid="$1" running="$2" gc_bin
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        "$gc_bin" dolt-state write-provider \
            --file "$STATE_FILE" \
            --pid "$pid" \
            --running "$running" \
            --port "$DOLT_PORT" \
            --data-dir "$DATA_DIR" || die "failed to write provider state via gc helper $gc_bin"
        return 0
    fi
    mkdir -p "$(dirname "$STATE_FILE")"
    local tmp
    tmp=$(mktemp "$STATE_FILE.tmp.XXXXXX")
    printf '{"running":%s,"pid":%s,"port":%s,"data_dir":"%s","started_at":"%s"}\n' \
        "$running" "$pid" "$DOLT_PORT" "$DATA_DIR" \
        "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" > "$tmp"
    mv "$tmp" "$STATE_FILE"
}

# load_state_field extracts a field from the private provider runtime state (no jq dependency).
load_state_field() {
    [ -f "$STATE_FILE" ] || return 0
    local gc_bin
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        "$gc_bin" dolt-state read-provider --file "$STATE_FILE" --field "$1" 2>/dev/null || true
        return 0
    fi
    sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\{0,1\}\([^",}]*\)"\{0,1\}.*/\1/p' "$STATE_FILE" | head -1
}
load_runtime_layout_from_gc() {
    local gc_bin output key value
    gc_bin=$(resolve_gc_helper_bin)
    [ -n "$gc_bin" ] || return 1
    output=$("$gc_bin" dolt-state runtime-layout --city "$GC_CITY_PATH" </dev/null 2>/dev/null) || return 1
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            GC_PACK_STATE_DIR) PACK_STATE_DIR="$value" ;;
            GC_DOLT_DATA_DIR) DATA_DIR="$value" ;;
            GC_DOLT_LOG_FILE) LOG_FILE="$value" ;;
            GC_DOLT_STATE_FILE) STATE_FILE="$value" ;;
            GC_DOLT_PID_FILE) PID_FILE="$value" ;;
            GC_DOLT_LOCK_FILE) LOCK_FILE="$value" ;;
            GC_DOLT_CONFIG_FILE) CONFIG_FILE="$value" ;;
        esac
    done <<EOF
$output
EOF
    [ -n "$PACK_STATE_DIR" ] && [ -n "$DATA_DIR" ] && [ -n "$LOG_FILE" ] && [ -n "$STATE_FILE" ] && [ -n "$PID_FILE" ] && [ -n "$LOCK_FILE" ] && [ -n "$CONFIG_FILE" ]
}

load_managed_process_inspection_from_gc() {
    local gc_bin output key value
    gc_bin=$(resolve_gc_helper_bin)
    [ -n "$gc_bin" ] || return 1
    output=$("$gc_bin" dolt-state inspect-managed --city "$GC_CITY_PATH" --port "$DOLT_PORT" </dev/null 2>/dev/null) || return 1
    GC_MANAGED_PID=""
    GC_MANAGED_OWNED="false"
    GC_MANAGED_DELETED="false"
    GC_PORT_HOLDER_PID=""
    GC_PORT_HOLDER_OWNED="false"
    GC_PORT_HOLDER_DELETED="false"
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            managed_pid)
                [ "$value" != "0" ] && GC_MANAGED_PID="$value"
                ;;
            managed_owned)
                GC_MANAGED_OWNED="$value"
                ;;
            managed_deleted_inodes)
                GC_MANAGED_DELETED="$value"
                ;;
            port_holder_pid)
                [ "$value" != "0" ] && GC_PORT_HOLDER_PID="$value"
                ;;
            port_holder_owned)
                GC_PORT_HOLDER_OWNED="$value"
                ;;
            port_holder_deleted_inodes)
                GC_PORT_HOLDER_DELETED="$value"
                ;;
        esac
    done <<EOF
$output
EOF
    return 0
}

load_probe_managed_from_gc() {
    local gc_bin host output key value status parsed=false
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    GC_PROBE_USED="false"
    GC_PROBE_RUNNING="false"
    GC_PROBE_PORT_HOLDER_PID=""
    GC_PROBE_PORT_HOLDER_OWNED="false"
    GC_PROBE_PORT_HOLDER_DELETED="false"
    GC_PROBE_TCP_REACHABLE="false"
    [ -n "$gc_bin" ] || return 1
    GC_PROBE_USED="true"
    output=$("$gc_bin" dolt-state probe-managed --city "$GC_CITY_PATH" --host "$host" --port "$DOLT_PORT" </dev/null 2>/dev/null)
    status=$?
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            running)
                GC_PROBE_RUNNING="$value"
                parsed=true
                ;;
            port_holder_pid)
                [ "$value" != "0" ] && GC_PROBE_PORT_HOLDER_PID="$value"
                parsed=true
                ;;
            port_holder_owned)
                GC_PROBE_PORT_HOLDER_OWNED="$value"
                parsed=true
                ;;
            port_holder_deleted_inodes)
                GC_PROBE_PORT_HOLDER_DELETED="$value"
                parsed=true
                ;;
            tcp_reachable)
                GC_PROBE_TCP_REACHABLE="$value"
                parsed=true
                ;;
        esac
    done <<EOF
$output
EOF
    if [ "$status" -ne 0 ] && [ "$parsed" != "true" ]; then
        GC_PROBE_USED="false"
        return 1
    fi
    [ "$status" -eq 0 ]
}

load_existing_managed_from_gc() {
    local gc_bin host output key value status parsed=false timeout_ms="${1:-30000}"
    case "$timeout_ms" in
        ''|*[!0-9]*)
            timeout_ms=30000
            ;;
    esac
    if [ "$timeout_ms" -lt 1 ]; then
        timeout_ms=1
    fi
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    GC_EXISTING_USED="false"
    GC_EXISTING_MANAGED_PID=""
    GC_EXISTING_MANAGED_OWNED="false"
    GC_EXISTING_DELETED_INODES="false"
    GC_EXISTING_STATE_PORT=""
    GC_EXISTING_READY="false"
    GC_EXISTING_REUSABLE="false"
    [ -n "$gc_bin" ] || return 1
    GC_EXISTING_USED="true"
    output=$("$gc_bin" dolt-state existing-managed --city "$GC_CITY_PATH" --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --timeout-ms "$timeout_ms" </dev/null 2>/dev/null)
    status=$?
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            managed_pid)
                [ "$value" != "0" ] && GC_EXISTING_MANAGED_PID="$value"
                parsed=true
                ;;
            managed_owned)
                GC_EXISTING_MANAGED_OWNED="$value"
                parsed=true
                ;;
            deleted_inodes)
                GC_EXISTING_DELETED_INODES="$value"
                parsed=true
                ;;
            state_port)
                [ "$value" != "0" ] && GC_EXISTING_STATE_PORT="$value"
                parsed=true
                ;;
            ready)
                GC_EXISTING_READY="$value"
                parsed=true
                ;;
            reusable)
                GC_EXISTING_REUSABLE="$value"
                parsed=true
                ;;
        esac
    done <<EOF
$output
EOF
    if [ "$status" -ne 0 ] && [ "$parsed" != "true" ]; then
        GC_EXISTING_USED="false"
        return 1
    fi
    [ "$status" -eq 0 ]
}

current_time_ms() {
    local gc_bin now
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        now=$("$gc_bin" dolt-state now-ms </dev/null 2>/dev/null) || now=""
        case "$now" in
            ''|*[!0-9]*)
                now=""
                ;;
        esac
        if [ -n "$now" ]; then
            printf '%s\n' "$now"
            return 0
        fi
    fi
    now=$(date +%s 2>/dev/null) || return 1
    case "$now" in
        ''|*[!0-9]*)
            return 1
            ;;
    esac
    printf '%s000\n' "$now"
}

run_preflight_cleanup() {
    local gc_bin
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        if "$gc_bin" dolt-state preflight-clean --city "$GC_CITY_PATH" </dev/null 2>/dev/null; then
            return 0
        fi
    fi
    clean_stale_sockets
    quarantine_phantom_dbs
    cleanup_stale_locks
}

# find_port_holder returns the PID of the process listening on DOLT_PORT.

find_port_holder() {
    run_lsof -nP -t -iTCP:"$DOLT_PORT" -sTCP:LISTEN 2>/dev/null | head -1
}

# verify_our_server checks if a PID belongs to our server (matching data-dir).
# Returns 0 if ours, 1 if imposter or unknown.
verify_our_server() {
    local pid="$1"
    [ -n "$pid" ] || return 1

    # Layer 1: State file data-dir comparison.
    local state_dir
    state_dir=$(load_state_field data_dir)
    if [ -n "$state_dir" ] && ! same_dir_path "$state_dir" "$DATA_DIR"; then
        return 1
    fi

    # Layer 2: Process args from ps — check --config or --data-dir.
    local proc_args
    proc_args=$(ps -p "$pid" -o args= 2>/dev/null) || return 1
    case "$proc_args" in
        *"--config $CONFIG_FILE"*|*"--config=$CONFIG_FILE"*)
            return 0
            ;;
        *"--config"*)
            # --config present but doesn't match our CONFIG_FILE — imposter.
            return 1
            ;;
        *"--data-dir"*)
            local proc_dir
            proc_dir=$(echo "$proc_args" | sed -n 's/.*--data-dir[= ]*\([^ ]*\).*/\1/p')
            if [ -n "$proc_dir" ]; then
                if same_dir_path "$proc_dir" "$DATA_DIR"; then
                    return 0
                fi
                return 1
            fi
            ;;
    esac

    # Layer 3: /proc/PID/cwd fallback (Linux only).
    if [ -d "/proc/$pid" ]; then
        local cwd
        cwd=$(readlink "/proc/$pid/cwd" 2>/dev/null) || true
        if [ -n "$cwd" ] && same_dir_path "$cwd" "$DATA_DIR"; then
            return 0
        fi
    fi

    # State file said it's ours (or no state file) and we couldn't disprove it.
    if [ -n "$state_dir" ] && same_dir_path "$state_dir" "$DATA_DIR"; then
        return 0
    fi

    # Cannot verify — treat as unknown (not ours).
    return 1
}

has_deleted_data_inodes() {
    local pid="$1"
    [ -n "$pid" ] || return 1

    local checked_proc=false
    if [ -d "/proc/$pid" ]; then
        checked_proc=true
        local cwd
        cwd=$(readlink "/proc/$pid/cwd" 2>/dev/null) || true
        case "$cwd" in
            *" (deleted)")
                return 0
                ;;
        esac
    fi

    if [ -d "/proc/$pid/fd" ]; then
        checked_proc=true
        local fd target
        for fd in /proc/"$pid"/fd/*; do
            [ -e "$fd" ] || [ -L "$fd" ] || continue
            target=$(readlink "$fd" 2>/dev/null) || continue
            case "$target" in
                *" (deleted)")
                    target=${target% (deleted)}
                    if path_under_data_dir "$target"; then
                        return 0
                    fi
                    ;;
            esac
        done
    fi

    if [ "$checked_proc" = "true" ]; then
        return 1
    fi

    if command -v lsof >/dev/null 2>&1; then
        local abs_data
        abs_data=$(canonical_dir "$DATA_DIR")
        if run_lsof -p "$pid" 2>/dev/null | grep ' (deleted)' | grep -F -e "$DATA_DIR" -e "$abs_data" >/dev/null 2>&1; then
            return 0
        fi
    fi

    return 1
}

wait_deleted_data_inodes() {
    local pid="$1" attempt=0
    while [ "$attempt" -lt 6 ]; do
        if has_deleted_data_inodes "$pid"; then
            return 0
        fi
        sleep 0.05 2>/dev/null || sleep 1
        attempt=$((attempt + 1))
    done
    return 1
}

# kill_imposter kills a process that isn't our dolt server.
kill_imposter() {
    local pid="$1"
    [ -n "$pid" ] || return 0

    echo "killing imposter dolt server (PID $pid) on port $DOLT_PORT" >&2
    kill "$pid" 2>/dev/null || return 0

    # Wait up to 5s for graceful shutdown.
    local waited=0
    while [ "$waited" -lt 5 ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done

    # Force kill.
    kill -9 "$pid" 2>/dev/null || true
    sleep 1
}

retired_replacement_db_name() {
    case "$1" in
        ?*.replaced-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z)
            return 0
            ;;
        *)
            return 1
            ;;
    esac
}

# quarantine_phantom_dbs moves unservable database dirs to quarantine.
# This includes missing-manifest phantom dirs and Dolt-retired replacement
# dirs that still have manifests but are no longer the active database.
quarantine_phantom_dbs() {
    [ -d "$DATA_DIR" ] || return 0
    local dir
    for dir in "$DATA_DIR"/*/; do
        [ -d "$dir" ] || continue
        [ -d "$dir/.dolt" ] || continue

        local name reason
        name=$(basename "$dir")
        if retired_replacement_db_name "$name"; then
            reason="retired replacement"
        elif [ ! -f "$dir/.dolt/noms/manifest" ]; then
            reason="missing noms/manifest"
        else
            continue
        fi

        local quarantine_dir="$DATA_DIR/.quarantine/$(date +%Y%m%dT%H%M%S)-$name"
        mkdir -p "$DATA_DIR/.quarantine"
        echo "quarantining unservable database: $name ($reason) -> $quarantine_dir" >&2
        mv -f "$dir" "$quarantine_dir"
    done
}

# cleanup_stale_locks removes .dolt/noms/LOCK files not held by any process.
cleanup_stale_locks() {
    [ -d "$DATA_DIR" ] || return 0
    local dir
    for dir in "$DATA_DIR"/*/; do
        [ -d "$dir" ] || continue
        local lock_file="$dir/.dolt/noms/LOCK"
        if [ -f "$lock_file" ]; then
            local open_status
            set +e
            lsof_reports_open "$lock_file"
            open_status=$?
            set -e
            case "$open_status" in
                0)
                    ;;
                1)
                    echo "removing stale LOCK: $lock_file" >&2
                    rm -f "$lock_file"
                    ;;
                *)
                    echo "preserving LOCK with unknown open-file state: $lock_file" >&2
                    ;;
            esac
        fi
    done
}

# write_config_yaml generates a managed dolt-config.yaml with timeouts and GC settings.
# Overwritten on each server start. Without read/write timeouts, CLOSE_WAIT connections
# accumulate and the server enters unrecoverable read-only mode.
write_config_yaml() {
    local gc_bin
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        "$gc_bin" dolt-config write-managed \
            --file "$CONFIG_FILE" \
            --host "$DOLT_HOST" \
            --port "$DOLT_PORT" \
            --data-dir "$DATA_DIR" \
            --log-level "$DOLT_LOGLEVEL" || die "failed to write managed dolt config via gc helper $gc_bin"
        return 0
    fi
    local tmp
    tmp=$(mktemp "$CONFIG_FILE.tmp.XXXXXX")
    cat > "$tmp" <<YAML
# Dolt SQL server configuration — managed by gc-beads-bd
# Do not edit manually; changes are overwritten on each server start.
# To customize, set environment variables:
#   GC_DOLT_PORT, GC_DOLT_HOST, GC_DOLT_USER, GC_DOLT_PASSWORD, GC_DOLT_LOGLEVEL

log_level: $DOLT_LOGLEVEL

listener:
  port: $DOLT_PORT
  host: $DOLT_HOST
  max_connections: 1000
  back_log: 50
  max_connections_timeout_millis: 5000
  read_timeout_millis: 300000
  write_timeout_millis: 300000

data_dir: "$DATA_DIR"

behavior:
  auto_gc_behavior:
    enable: true
    archive_level: 1
YAML
    mv "$tmp" "$CONFIG_FILE"
}

# get_connection_count queries the active connection count from the dolt server.
# Prints the count to stdout. Returns 1 on failure.
get_connection_count() {
    local host output
    host=$(connect_host)
    output=$(dolt --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --password "${DOLT_PASSWORD:-}" --no-tls \
        sql -r csv -q "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST" 2>/dev/null) || return 1
    # Parse CSV: "cnt\n5\n" — take last non-empty line.
    echo "$output" | tail -1 | tr -d '[:space:]'
}

# check_read_only tests if the dolt server is in read-only mode.
# Returns 0 if read-only, 1 if writable.
check_read_only() {
    local host gc_bin
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        "$gc_bin" dolt-state read-only-check --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" >/dev/null 2>&1
        case $? in
            0) return 0 ;;
            *) return 1 ;;
        esac
    fi
    local output
    # Keep __gc_probe stable. Dropping Dolt databases leaves
    # .dolt_dropped_databases backups behind.
    output=$(dolt --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --password "${DOLT_PASSWORD:-}" --no-tls         sql -q "CREATE DATABASE IF NOT EXISTS __gc_probe; CREATE TABLE IF NOT EXISTS __gc_probe.__probe (k INT PRIMARY KEY); REPLACE INTO __gc_probe.__probe VALUES (1);" 2>&1) || true
    case "$output" in
        *"read only"*|*"READ ONLY"*|*"Read-only"*)
            return 0  # Is read-only.
            ;;
    esac
    return 1  # Writable.
}

load_health_check_from_gc() {
    local gc_bin host output key value
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    [ -n "$gc_bin" ] || return 1
    output=$("$gc_bin" dolt-state health-check --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --check-read-only </dev/null 2>/dev/null) || return 1
    GC_HEALTH_QUERY_READY="false"
    GC_HEALTH_READ_ONLY=""
    GC_HEALTH_CONNECTION_COUNT=""
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            query_ready)
                GC_HEALTH_QUERY_READY="$value"
                ;;
            read_only)
                GC_HEALTH_READ_ONLY="$value"
                ;;
            connection_count)
                GC_HEALTH_CONNECTION_COUNT="$value"
                ;;
        esac
    done <<EOF
$output
EOF
    return 0
}

load_wait_ready_from_gc() {
    local pid="$1" timeout_ms="$2" check_deleted="${3:-false}"
    local gc_bin host output key value status parsed=false
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    GC_WAIT_READY_USED="false"
    GC_WAIT_READY="false"
    GC_WAIT_PID_ALIVE="false"
    GC_WAIT_DELETED_INODES="false"
    [ -n "$gc_bin" ] || return 1
    GC_WAIT_READY_USED="true"
    if [ "$check_deleted" = "true" ]; then
        output=$("$gc_bin" dolt-state wait-ready --city "$GC_CITY_PATH" --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --pid "$pid" --timeout-ms "$timeout_ms" --check-deleted </dev/null 2>/dev/null)
        status=$?
    else
        output=$("$gc_bin" dolt-state wait-ready --city "$GC_CITY_PATH" --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --pid "$pid" --timeout-ms "$timeout_ms" </dev/null 2>/dev/null)
        status=$?
    fi
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            ready)
                GC_WAIT_READY="$value"
                parsed=true
                ;;
            pid_alive)
                GC_WAIT_PID_ALIVE="$value"
                parsed=true
                ;;
            deleted_inodes)
                GC_WAIT_DELETED_INODES="$value"
                parsed=true
                ;;
        esac
    done <<EOF
$output
EOF
    if [ "$status" -ne 0 ] && [ "$parsed" != "true" ]; then
        GC_WAIT_READY_USED="false"
        return 1
    fi
    [ "$status" -eq 0 ]
}

wait_for_managed_pid_ready() {
    local pid="$1" port="$2" timeout_ms="${3:-30000}" check_deleted="${4:-false}"
    local attempt=0 max_attempts=1
    [ -n "$pid" ] || return 1
    [ -n "$port" ] || port="$DOLT_PORT"
    DOLT_PORT="$port"

    if load_wait_ready_from_gc "$pid" "$timeout_ms" "$check_deleted"; then
        [ "$GC_WAIT_READY" = "true" ] || return 1
        if [ "$check_deleted" = "true" ] && [ "$GC_WAIT_DELETED_INODES" = "true" ]; then
            return 1
        fi
        return 0
    elif [ "$GC_WAIT_READY_USED" = "true" ]; then
        [ "$GC_WAIT_READY" = "true" ] || return 1
        if [ "$check_deleted" = "true" ] && [ "$GC_WAIT_DELETED_INODES" = "true" ]; then
            return 1
        fi
        return 0
    fi

    if [ "$timeout_ms" -gt 0 ] 2>/dev/null; then
        max_attempts=$((timeout_ms / 500))
        if [ "$max_attempts" -lt 1 ]; then
            max_attempts=1
        fi
    fi

    while [ "$attempt" -lt "$max_attempts" ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            return 1
        fi
        if [ "$check_deleted" = "true" ] && wait_deleted_data_inodes "$pid"; then
            return 1
        fi
        if tcp_check_port "$port" && do_query_probe; then
            if [ "$check_deleted" = "true" ] && wait_deleted_data_inodes "$pid"; then
                return 1
            fi
            return 0
        fi
        sleep 0.5 2>/dev/null || sleep 1
        attempt=$((attempt + 1))
    done
    return 1
}

load_start_managed_from_gc() {
    local gc_bin host output key value status parsed=false
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    GC_START_MANAGED_USED="false"
    GC_START_READY="false"
    GC_START_PID=""
    GC_START_PORT="$DOLT_PORT"
    GC_START_ADDRESS_IN_USE="false"
    [ -n "$gc_bin" ] || return 1
    GC_START_MANAGED_USED="true"
    output=$("$gc_bin" dolt-state start-managed --city "$GC_CITY_PATH" --host "$DOLT_HOST" --port "$DOLT_PORT" --user "$DOLT_USER" --log-level "$DOLT_LOGLEVEL" --timeout-ms 30000 9>&- </dev/null 2>/dev/null)
    status=$?
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            ready)
                GC_START_READY="$value"
                parsed=true
                ;;
            pid)
                [ "$value" != "0" ] && GC_START_PID="$value"
                parsed=true
                ;;
            port)
                [ -n "$value" ] && GC_START_PORT="$value"
                parsed=true
                ;;
            address_in_use)
                GC_START_ADDRESS_IN_USE="$value"
                parsed=true
                ;;
        esac
    done <<EOF
$output
EOF
    if [ "$status" -ne 0 ] && [ "$parsed" != "true" ]; then
        GC_START_MANAGED_USED="false"
        return 1
    fi
    [ "$status" -eq 0 ]
}

wait_for_concurrent_start_ready() {
    local existing_pid="" existing_port="" holder="" timeout_ms deadline_ms now_ms remaining_ms wait_ms
    timeout_ms="$CONCURRENT_START_READY_TIMEOUT_MS"
    case "$timeout_ms" in
        ''|*[!0-9]*)
            timeout_ms=45000
            ;;
    esac
    if [ "$timeout_ms" -lt 500 ]; then
        timeout_ms=500
    fi
    now_ms=$(current_time_ms) || return 1
    deadline_ms=$((now_ms + timeout_ms))
    while :; do
        now_ms=$(current_time_ms) || return 1
        remaining_ms=$((deadline_ms - now_ms))
        if [ "$remaining_ms" -le 0 ]; then
            return 1
        fi
        if load_existing_managed_from_gc "$remaining_ms"; then
            existing_pid="$GC_EXISTING_MANAGED_PID"
            if [ "$GC_EXISTING_REUSABLE" = "true" ] && [ -n "$GC_EXISTING_STATE_PORT" ] && [ -n "$existing_pid" ]; then
                DOLT_PORT="$GC_EXISTING_STATE_PORT"
                echo "$existing_pid" > "$PID_FILE"
                save_state "$existing_pid" true
                return 0
            fi
        fi
        if load_probe_managed_from_gc; then
            holder="$GC_PROBE_PORT_HOLDER_PID"
            if [ "$GC_PROBE_RUNNING" = "true" ] && [ -n "$holder" ]; then
                if do_query_probe; then
                    echo "$holder" > "$PID_FILE"
                    save_state "$holder" true
                    return 0
                fi
            fi
        fi
        if [ "$GC_EXISTING_USED" != "true" ] && [ "$GC_PROBE_USED" != "true" ]; then
            existing_port=$(load_state_field port)
            if [ -n "$existing_port" ]; then
                existing_pid=$(find_dolt_pid)
                if [ -n "$existing_pid" ] && verify_our_server "$existing_pid"; then
                    DOLT_PORT="$existing_port"
                    if tcp_check_port "$existing_port" && do_query_probe; then
                        echo "$existing_pid" > "$PID_FILE"
                        save_state "$existing_pid" true
                        return 0
                    fi
                fi
            fi
        fi
        now_ms=$(current_time_ms) || return 1
        remaining_ms=$((deadline_ms - now_ms))
        if [ "$remaining_ms" -le 0 ]; then
            return 1
        fi
        wait_ms=500
        if [ "$remaining_ms" -lt "$wait_ms" ]; then
            wait_ms="$remaining_ms"
        fi
        if [ "$wait_ms" -le 0 ]; then
            return 1
        fi
        sleep_ms "$wait_ms" 2>/dev/null || sleep 1
    done
}

load_stop_managed_from_gc() {
    local gc_bin output key value status parsed=false
    gc_bin=$(resolve_gc_helper_bin)
    GC_STOP_MANAGED_USED="false"
    GC_STOP_HAD_PID="false"
    GC_STOP_PID=""
    GC_STOP_FORCED="false"
    [ -n "$gc_bin" ] || return 1
    GC_STOP_MANAGED_USED="true"
    output=$("$gc_bin" dolt-state stop-managed --city "$GC_CITY_PATH" --port "$DOLT_PORT" </dev/null 2>/dev/null)
    status=$?
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            had_pid)
                GC_STOP_HAD_PID="$value"
                parsed=true
                ;;
            pid)
                [ "$value" != "0" ] && GC_STOP_PID="$value"
                parsed=true
                ;;
            forced)
                GC_STOP_FORCED="$value"
                parsed=true
                ;;
        esac
    done <<EOF
$output
EOF
    if [ "$status" -ne 0 ] && [ "$parsed" != "true" ]; then
        GC_STOP_MANAGED_USED="false"
        return 1
    fi
    [ "$status" -eq 0 ]
}

load_recover_managed_from_gc() {
    local gc_bin output key value status parsed=false
    gc_bin=$(resolve_gc_helper_bin)
    GC_RECOVER_MANAGED_USED="false"
    GC_RECOVER_DIAGNOSED_READ_ONLY="false"
    GC_RECOVER_HAD_PID="false"
    GC_RECOVER_FORCED="false"
    GC_RECOVER_READY="false"
    GC_RECOVER_PID=""
    GC_RECOVER_PORT="$DOLT_PORT"
    GC_RECOVER_HEALTHY="false"
    [ -n "$gc_bin" ] || return 1
    GC_RECOVER_MANAGED_USED="true"
    output=$("$gc_bin" dolt-state recover-managed --city "$GC_CITY_PATH" --host "$DOLT_HOST" --port "$DOLT_PORT" --user "$DOLT_USER" --log-level "$DOLT_LOGLEVEL" --timeout-ms 30000 </dev/null 2>/dev/null)
    status=$?
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            diagnosed_read_only)
                GC_RECOVER_DIAGNOSED_READ_ONLY="$value"
                parsed=true
                ;;
            had_pid)
                GC_RECOVER_HAD_PID="$value"
                parsed=true
                ;;
            forced)
                GC_RECOVER_FORCED="$value"
                parsed=true
                ;;
            ready)
                GC_RECOVER_READY="$value"
                parsed=true
                ;;
            pid)
                [ "$value" != "0" ] && GC_RECOVER_PID="$value"
                parsed=true
                ;;
            port)
                [ -n "$value" ] && GC_RECOVER_PORT="$value"
                parsed=true
                ;;
            healthy)
                GC_RECOVER_HEALTHY="$value"
                parsed=true
                ;;
        esac
    done <<EOF
$output
EOF
    if [ "$status" -ne 0 ] && [ "$parsed" != "true" ]; then
        GC_RECOVER_MANAGED_USED="false"
        return 1
    fi
    [ "$status" -eq 0 ]
}

# find_dolt_pid finds the dolt sql-server process.
# Priority: PID file → lsof port holder → ps grep fallback.
find_dolt_pid() {
    if [ -z "$DATA_DIR" ]; then
        return
    fi

    # 1. PID file (most reliable if we wrote it).
    if [ -f "$PID_FILE" ]; then
        local file_pid
        file_pid=$(cat "$PID_FILE" 2>/dev/null)
        if [ -n "$file_pid" ] && kill -0 "$file_pid" 2>/dev/null; then
            echo "$file_pid"
            return
        fi
        # Stale PID file — clean up.
        rm -f "$PID_FILE"
    fi

    # 2. lsof port holder.
    local holder
    holder=$(find_port_holder)
    if [ -n "$holder" ]; then
        echo "$holder"
        return
    fi

    # 3. ps grep fallback (least reliable) — try --config first, then --data-dir.
    if [ -n "$CONFIG_FILE" ]; then
        local config_pid
        config_pid=$(ps ax -o pid,args 2>/dev/null | grep "dolt sql-server" | grep -- "--config.*$CONFIG_FILE" | grep -v grep | awk '{print $1}' | head -1)
        if [ -n "$config_pid" ]; then
            echo "$config_pid"
            return
        fi
    fi
    ps ax -o pid,args 2>/dev/null | grep "dolt sql-server" | grep -- "--data-dir.*$(basename "$DATA_DIR")" | grep -v grep | awk '{print $1}' | head -1
}

# allocate_port determines the dolt server port.
# Resolution order:
#   1. GC_DOLT_PORT env var (explicit override) → use it
#   2. State file has a port + PID is alive → reuse it (stable across restarts)
#   3. Hash GC_CITY_PATH into range 10000–60000, probe with lsof, increment until free
allocate_port() {
    local gc_bin helper_port
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        helper_port=$("$gc_bin" dolt-state allocate-port --city "$GC_CITY_PATH" --state-file "$STATE_FILE" </dev/null 2>/dev/null || true)
        if [ -n "$helper_port" ]; then
            echo "$helper_port"
            return
        fi
    fi

    # 1. Explicit override.
    if [ -n "$GC_DOLT_PORT" ]; then
        echo "$GC_DOLT_PORT"
        return
    fi

    # 2. Provider state port with live PID.
    if [ -f "$STATE_FILE" ]; then
        local state_port state_pid
        state_port=$(load_state_field port)
        state_pid=$(load_state_field pid)
        if [ -n "$state_port" ] && [ -n "$state_pid" ] && kill -0 "$state_pid" 2>/dev/null; then
            echo "$state_port"
            return
        fi
    fi

    # 3. Deterministic hash of city path, probe until free.
    local hash_val
    hash_val=$(printf '%s' "$GC_CITY_PATH" | cksum | awk '{print $1 % 50000 + 10000}')
    local port="$hash_val"
    local attempts=0
    while [ "$attempts" -lt 100 ]; do
        if ! run_lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
            echo "$port"
            return
        fi
        port=$((port + 1))
        if [ "$port" -gt 60000 ]; then
            port=10000
        fi
        attempts=$((attempts + 1))
    done

    # Exhausted probes — fall back to the hash value and hope for the best.
    echo "$hash_val"
}

next_available_port() {
    local port="${1:-10000}"
    local attempts=0
    while [ "$attempts" -lt 1000 ]; do
        if [ "$port" -gt 60000 ]; then
            port=10000
        fi
        if ! run_lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
            echo "$port"
            return
        fi
        port=$((port + 1))
        attempts=$((attempts + 1))
    done
    echo "${1:-10000}"
}

# --- Operations ---

# valid_sql_name checks that a name is safe for backtick-quoted SQL identifiers.
# Allows alphanumeric, hyphens, underscores. Rejects empty names and names
# containing backticks, quotes, or other special characters (upstream 38f7b380).
valid_sql_name() {
    case "$1" in
        "") return 1 ;;
        *[!a-zA-Z0-9_-]*) return 1 ;;
    esac
    return 0
}

is_reserved_dolt_database_name() {
    case "$(printf '%s' "$1" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')" in
        __gc_probe) return 0 ;;
        *) return 1 ;;
    esac
}

# clean_stale_sockets removes stale Unix domain sockets left by a crashed
# dolt server. Without cleanup, "unix socket set up failed: file already in
# use" prevents clean restarts (upstream 2e058fa1).
clean_stale_sockets() {
    local sock
    for sock in /tmp/dolt*.sock; do
        [ -S "$sock" ] || continue
        local open_status
        set +e
        lsof_reports_open "$sock"
        open_status=$?
        set -e
        case "$open_status" in
            0)
                ;;
            1)
                echo "removing stale socket: $sock" >&2
                rm -f "$sock"
                ;;
            *)
                echo "preserving socket with unknown open-file state: $sock" >&2
                ;;
        esac
    done
}

# ensure_beads_role ensures beads.role is set in global git config.
# bd exits non-zero with "beads.role not configured" (gastownhall/beads#2950)
# when this key is absent. That non-zero exit causes the `run_bd_pinned … ||
# true` calls in op_init to fail silently, leaving issue_prefix and
# types.custom unset in the Dolt database and making every subsequent
# bd-create call fail with "database not initialized". Defaulting to
# "maintainer" matches the role that gc-managed agents use to create beads.
ensure_beads_role() {
    if git config --global beads.role >/dev/null 2>&1; then
        return 0
    fi
    echo "gc-beads-bd: setting git config --global beads.role maintainer" >&2
    git config --global beads.role maintainer || die "failed to set git config beads.role"
}

# ensure_dolt_identity ensures dolt has user.name and user.email configured.
ensure_dolt_identity() {
    # Check if already configured.
    if dolt config --global --get user.name >/dev/null 2>&1 && \
       dolt config --global --get user.email >/dev/null 2>&1; then
        return 0
    fi

    # Copy from git config.
    local name email
    name=$(git config --global user.name 2>/dev/null || true)
    email=$(git config --global user.email 2>/dev/null || true)

    if [ -z "$name" ]; then
        die "dolt identity not configured and git user.name not available; run: dolt config --global --add user.name \"Your Name\""
    fi
    if [ -z "$email" ]; then
        die "dolt identity not configured and git user.email not available; run: dolt config --global --add user.email \"you@example.com\""
    fi

    # Set missing fields.
    if ! dolt config --global --get user.name >/dev/null 2>&1; then
        dolt config --global --add user.name "$name" || die "failed to set dolt user.name"
    fi
    if ! dolt config --global --get user.email >/dev/null 2>&1; then
        dolt config --global --add user.email "$email" || die "failed to set dolt user.email"
    fi
}

# op_start starts the dolt server if not already running.
op_start() {
    if is_remote; then
        # Remote server — nothing to start locally.
        exit 2
    fi

    local gc_helper_bin
    gc_helper_bin=$(resolve_gc_helper_bin)

    ensure_dolt_identity

    # Check for required tools before attempting anything.
    if ! command -v flock >/dev/null 2>&1; then
        die "flock is required but not installed. Install: brew install flock (macOS) or apt install util-linux (Linux)"
    fi
    if ! command -v dolt >/dev/null 2>&1; then
        die "dolt is required but not installed. Install: https://github.com/dolthub/dolt/releases"
    fi

    # Create data dir and runtime state dir if needed.
    mkdir -p "$DATA_DIR" "$(dirname "$LOCK_FILE")"

    # Acquire exclusive start lock (prevents concurrent starts).
    # Use fd 9 for the lock and keep retrying on the same inode. Deleting and
    # recreating the lock file after retry exhaustion is unsafe because flock
    # attaches to the inode, not the pathname, so a second starter could bypass
    # a live holder by acquiring a brand-new file.
    exec 9>"$LOCK_FILE"
    local lock_acquired=false
    local attempt=0
    while [ "$attempt" -lt 6 ]; do
        if flock -n 9 2>/dev/null; then
            lock_acquired=true
            break
        fi
        sleep 0.5 2>/dev/null || sleep 1
        attempt=$((attempt + 1))
    done
    if [ "$lock_acquired" = "false" ]; then
        if wait_for_concurrent_start_ready; then
            exit 0
        fi
        die "could not acquire dolt start lock ($LOCK_FILE)"
    fi

    # Check if a dolt process is already serving our data dir (any port).
    # This prevents starting a second server that dies on database locks.
    # If the server is ours, wait for it to become ready before restarting.
    local existing_pid holder
    if load_existing_managed_from_gc; then
        existing_pid="$GC_EXISTING_MANAGED_PID"
        if [ "$GC_EXISTING_REUSABLE" = "true" ] && [ -n "$GC_EXISTING_STATE_PORT" ]; then
            DOLT_PORT="$GC_EXISTING_STATE_PORT"
            echo "$existing_pid" > "$PID_FILE"
            save_state "$existing_pid" true
            exit 0
        fi
        if [ -n "$existing_pid" ] && [ "$GC_EXISTING_MANAGED_OWNED" = "true" ]; then
            kill -9 "$existing_pid" 2>/dev/null || true
            local waited=0
            while [ "$waited" -lt 20 ] && kill -0 "$existing_pid" 2>/dev/null; do
                sleep 0.5 2>/dev/null || sleep 1
                waited=$((waited + 1))
            done
        fi
    else
        if ! load_managed_process_inspection_from_gc; then
            existing_pid=$(find_dolt_pid)
        else
            existing_pid="$GC_MANAGED_PID"
            holder="$GC_PORT_HOLDER_PID"
        fi
        if [ -n "$existing_pid" ] && kill -0 "$existing_pid" 2>/dev/null; then
            local existing_owned=true
            local existing_deleted=false
            if [ -n "${GC_MANAGED_PID:-}" ] && [ "$existing_pid" = "$GC_MANAGED_PID" ]; then
                existing_owned="$GC_MANAGED_OWNED"
                existing_deleted="$GC_MANAGED_DELETED"
            else
                if verify_our_server "$existing_pid"; then
                    existing_owned=true
                else
                    existing_owned=false
                fi
                if wait_deleted_data_inodes "$existing_pid"; then
                    existing_deleted=true
                fi
            fi
            if [ "$existing_owned" = true ]; then
                local existing_port
                existing_port=$(load_state_field port)
                if [ -n "$existing_port" ]; then
                    DOLT_PORT="$existing_port"
                    if wait_for_managed_pid_ready "$existing_pid" "$existing_port" 30000 true; then
                        echo "$existing_pid" > "$PID_FILE"
                        save_state "$existing_pid" true
                        exit 0
                    fi
                fi

                # Our server exists but never became ready — restart it.
                kill -9 "$existing_pid" 2>/dev/null || true
                local waited=0
                while [ "$waited" -lt 20 ] && kill -0 "$existing_pid" 2>/dev/null; do
                    sleep 0.5 2>/dev/null || sleep 1
                    waited=$((waited + 1))
                done
            fi
        fi
    fi

    # Check if a process already holds the port.
    if load_probe_managed_from_gc; then
        holder="$GC_PROBE_PORT_HOLDER_PID"
        if [ "$GC_PROBE_RUNNING" = "true" ] && [ -n "$holder" ]; then
            # Our server is already running — update state and exit success.
            echo "$holder" > "$PID_FILE"
            save_state "$holder" true
            exit 0
        fi
        if [ -n "$holder" ]; then
            if [ "$GC_PROBE_PORT_HOLDER_OWNED" = "true" ]; then
                kill -9 "$holder" 2>/dev/null || true
                local waited=0
                while [ "$waited" -lt 20 ] && kill -0 "$holder" 2>/dev/null; do
                    sleep 0.5 2>/dev/null || sleep 1
                    waited=$((waited + 1))
                done
            else
                if [ -z "$gc_helper_bin" ]; then
                    kill_imposter "$holder"
                    sleep 1
                fi
            fi
        fi
    else
        if [ -z "$holder" ]; then
            holder=$(find_port_holder)
        fi
        if [ -n "$holder" ]; then
            local holder_owned=false
            local holder_deleted=false
            if [ -n "${GC_PORT_HOLDER_PID:-}" ] && [ "$holder" = "$GC_PORT_HOLDER_PID" ]; then
                holder_owned="$GC_PORT_HOLDER_OWNED"
                holder_deleted="$GC_PORT_HOLDER_DELETED"
            else
                if verify_our_server "$holder"; then
                    holder_owned=true
                fi
                if wait_deleted_data_inodes "$holder"; then
                    holder_deleted=true
                fi
            fi
            if [ "$holder_owned" = true ] && [ "$holder_deleted" != true ]; then
                # Our server is already running — update state and exit success.
                echo "$holder" > "$PID_FILE"
                save_state "$holder" true
                exit 0
            else
                # Imposter or stale local server on our port — kill it.
                if [ -z "$gc_helper_bin" ]; then
                    kill_imposter "$holder"
                    sleep 1
                fi
            fi
        fi
    fi

    if load_start_managed_from_gc; then
        DOLT_PORT="$GC_START_PORT"
        return 0
    elif [ "$GC_START_MANAGED_USED" = "true" ]; then
        DOLT_PORT="$GC_START_PORT"
        rm -f "$PID_FILE"
        save_state 0 false
        die "dolt server could not start via gc helper (check $LOG_FILE)"
    fi

    local launch_attempt=0
    while [ "$launch_attempt" -lt 5 ]; do
        # Pre-launch cleanup.
        run_preflight_cleanup

        # Write managed config.yaml with timeouts and GC settings.
        write_config_yaml

        local log_offset=0
        if [ -f "$LOG_FILE" ]; then
            log_offset=$(wc -c < "$LOG_FILE" 2>/dev/null || echo 0)
        fi

        # Disable Dolt's load-average auto-GC scheduler. Dolt 1.86.0+
        # ships a loadAvgGCScheduler whose threshold formula scales
        # inversely with CPU count (10/CPUs), so on multi-core hosts the
        # gate is essentially always tripped and CALL DOLT_GC() is
        # queued but never executed; auto_gc_behavior.enable: true in
        # config.yaml has no effect. See
        # https://github.com/dolthub/dolt/issues/10944. Users who
        # explicitly set DOLT_GC_SCHEDULER are respected.
        : "${DOLT_GC_SCHEDULER=NONE}"
        export DOLT_GC_SCHEDULER

        # Start dolt sql-server with config file. Close the startup lock fd in
        # the child so the flock is released when this starter exits.
        nohup sh -c 'exec 9>&-; exec dolt sql-server --config "$1"' sh "$CONFIG_FILE" >> "$LOG_FILE" 2>&1 &
        local server_pid=$!

        # Write PID file.
        echo "$server_pid" > "$PID_FILE"

        # Save state.
        save_state "$server_pid" true

        # Wait for server: combined PID alive + TCP reachable + query-ready check.
        # 60 iterations × 500ms = 30s max. Large data dirs with many databases
        # can take 10-20s to start, and the TCP listener can come up before
        # the SQL layer is ready to answer queries.
        local ready=false
        if load_wait_ready_from_gc "$server_pid" 30000 false; then
            ready="$GC_WAIT_READY"
        elif [ "$GC_WAIT_READY_USED" != "true" ]; then
            attempt=0
            while [ "$attempt" -lt 60 ]; do
                # Fail fast if process crashed during startup.
                if ! kill -0 "$server_pid" 2>/dev/null; then
                    break
                fi

                # Check TCP reachability and a lightweight query probe.
                if tcp_check && do_query_probe; then
                    ready=true
                    break
                fi
                sleep 0.5 2>/dev/null || sleep 1
                attempt=$((attempt + 1))
            done
        fi

        if [ "$ready" = true ]; then
            return 0
        fi

        if kill -0 "$server_pid" 2>/dev/null; then
            # Clean up: kill the stuck server and reset state to prevent double-launch.
            kill "$server_pid" 2>/dev/null || true
            rm -f "$PID_FILE"
            save_state 0 false
            die "dolt server started (PID $server_pid) but did not become query-ready after 30s (check $LOG_FILE)"
        fi

        rm -f "$PID_FILE"
        save_state 0 false

        local startup_output=""
        if [ -f "$LOG_FILE" ]; then
            startup_output=$(tail -c +$((log_offset + 1)) "$LOG_FILE" 2>/dev/null || true)
        fi
        if printf '%s' "$startup_output" | grep -qi 'address already in use'; then
            launch_attempt=$((launch_attempt + 1))
            DOLT_PORT=$(next_available_port $((DOLT_PORT + 1)))
            continue
        fi

        die "dolt server exited during startup (check $LOG_FILE)"
    done

    rm -f "$PID_FILE"
    save_state 0 false
    die "dolt server could not find a free port after repeated address-in-use failures (check $LOG_FILE)"

}

# op_ensure_ready is a legacy alias for start.
op_ensure_ready() {
    if ! is_remote; then
        local pid state_port
        pid=$(find_dolt_pid)
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && verify_our_server "$pid"; then
            state_port=$(load_state_field port)
            if [ -z "$state_port" ]; then
                state_port="$DOLT_PORT"
            fi
            DOLT_PORT="$state_port"
            if wait_for_managed_pid_ready "$pid" "$state_port" 30000 true; then
                echo "$pid" > "$PID_FILE"
                save_state "$pid" true
                return 0
            fi
        fi
    fi
    op_start
}

# run_bd_pinned executes bd against the already-selected Dolt backend for this
# provider operation. Without these exports, plain bd commands can rediscover
# or auto-start a different local server mid-init.
run_bd_pinned() {
    local dir="$1"
    shift
    local beads_dir="$dir/.beads"
    local host
    host=$(connect_host)
    (
        cd "$dir" || exit 1
        export BEADS_DIR="$beads_dir"
        export GC_DOLT_HOST="$host"
        export BEADS_DOLT_SERVER_HOST="$host"
        export GC_DOLT_PORT="$DOLT_PORT"
        export BEADS_DOLT_SERVER_PORT="$DOLT_PORT"
        export GC_DOLT_USER="$DOLT_USER"
        export GC_DOLT_PASSWORD="$DOLT_PASSWORD"
        export BEADS_DOLT_SERVER_USER="$DOLT_USER"
        export BEADS_DOLT_PASSWORD="$DOLT_PASSWORD"
        bd "$@"
    )
}

run_bd_init_pinned() {
    local dir="$1"
    local prefix="$2"
    local dolt_database="$3"
    local host="$4"
    local force_init="${5:-false}"
    if [ "$force_init" = "true" ]; then
        run_bd_pinned "$dir" init --force --quiet --server -p "$prefix" --database "$dolt_database" --skip-hooks --skip-agents \
            --server-host "$host" --server-port "$DOLT_PORT" "$dir" || die "bd init failed for $dir"
        return 0
    fi

    run_bd_pinned "$dir" init --quiet --server -p "$prefix" --database "$dolt_database" --skip-hooks --skip-agents \
        --server-host "$host" --server-port "$DOLT_PORT" "$dir" || die "bd init failed for $dir"
}

ensure_beads_dir_permissions() {
    local dir="$1"
    local beads_dir="$dir/.beads"
    mkdir -p "$beads_dir" || die "failed to create $beads_dir"
    chmod 700 "$beads_dir" || die "failed to set $beads_dir permissions to 700"
}

normalize_scope_after_init() {
    local dir="$1"
    local prefix="$2"
    local dolt_database="$3"
    local gc_bin
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        if [ -n "$dolt_database" ]; then
            "$gc_bin" dolt-config normalize-scope --city "$GC_CITY_PATH" --dir "$dir" --prefix "$prefix" --dolt-database "$dolt_database" || die "failed to normalize canonical scope state for $dir"
        else
            "$gc_bin" dolt-config normalize-scope --city "$GC_CITY_PATH" --dir "$dir" --prefix "$prefix" || die "failed to normalize canonical scope state for $dir"
        fi
        return 0
    fi
    rm -f "$dir/.beads/dolt-server.pid" "$dir/.beads/dolt-server.lock" "$dir/.beads/dolt-server.log" "$dir/.beads/dolt-server.port"
}

# op_init initializes beads in a directory.
# Args: <dir> <prefix> [dolt_database]
op_init() {
    local dir="$1"
    local prefix="$2"
    local dolt_database="${3:-}"
    local metadata_path="$dir/.beads/metadata.json"
    local existing_db=""
    local allow_reserved_existing=false
    local bd_init_force=""
    if [ -z "$dir" ] || [ -z "$prefix" ]; then
        die "usage: gc-beads-bd init <dir> <prefix> [dolt_database]"
    fi

    if [ -f "$metadata_path" ]; then
        existing_db=$(read_existing_dolt_database "$metadata_path")
        if [ -n "$existing_db" ] && is_reserved_dolt_database_name "$existing_db"; then
            allow_reserved_existing=true
        fi
    fi

    # Validate prefix before SQL interpolation (upstream 38f7b380).
    if ! valid_sql_name "$prefix"; then
        die "invalid beads prefix: $prefix (must be alphanumeric, hyphens, underscores)"
    fi
    if [ -n "$dolt_database" ]; then
        if is_reserved_dolt_database_name "$dolt_database"; then
            if [ "$allow_reserved_existing" = true ]; then
                dolt_database="$existing_db"
            else
                die "reserved dolt database name: $dolt_database (used internally by gc)"
            fi
        fi
        if ! valid_sql_name "$dolt_database"; then
            die "invalid dolt database name: $dolt_database (must be alphanumeric, hyphens, underscores)"
        fi
    fi
    # Filter BEADS_DIR from inherited environment to prevent bd from
    # finding a parent directory's .beads/ database (upstream parity).
    local beads_dir="$dir/.beads"
    unset BEADS_DIR
    export BEADS_DIR="$beads_dir"
    ensure_beads_dir_permissions "$dir"
    ensure_beads_role

    if [ -z "$dolt_database" ]; then
        # Compatibility fallback for direct gc-beads-bd invocations.
        # GC's canonical path passes dolt_database explicitly.
        if [ -n "$existing_db" ]; then
            if is_reserved_dolt_database_name "$existing_db"; then
                if [ "$allow_reserved_existing" = true ]; then
                    # Preserve legacy probe metadata for already-initialized
                    # scopes so startup can recover them into the canonical
                    # migration flow. Fresh init still rejects this name.
                    dolt_database="$existing_db"
                else
                    die "reserved dolt database name: $existing_db (used internally by gc)"
                fi
            elif ! valid_sql_name "$existing_db"; then
                die "invalid existing dolt database name: $existing_db"
            else
                dolt_database="$existing_db"
            fi
        else
            dolt_database="$prefix"
        fi
    fi
    if is_reserved_dolt_database_name "$dolt_database" && [ "$allow_reserved_existing" != true ]; then
        die "reserved dolt database name: $dolt_database (used internally by gc)"
    fi

    # Custom bead types for bd (extracted from beads core in v0.46.0).
    # GC_BEADS_CUSTOM_TYPES overrides the default SDK set.
    # "convergence" is required because gc's convergence handler creates
    # beads with that type — must match doctor.RequiredCustomTypes.
    local custom_types="${GC_BEADS_CUSTOM_TYPES:-molecule,convoy,message,event,gate,merge-request,agent,role,rig,session,spec,convergence}"

    # If already initialized on disk, ensure the database is also registered
    # with the running server. This covers the case where bd init created the
    # directory but the server was restarted (or the database was quarantined).
    if [ -f "$dir/.beads/metadata.json" ]; then
        if ensure_database_registered "$dolt_database"; then
            if bd_runtime_schema_ready "$dolt_database"; then
                # GC owns canonical metadata/config normalization after this backend
                # bridge returns. Keep the backend focused on database registration
                # and bd-specific bootstrap only.
                ensure_beads_dir_permissions "$dir"
                normalize_scope_after_init "$dir" "$prefix" "$dolt_database"
                run_bd_pinned "$dir" config set types.custom "$custom_types" 2>/dev/null || true
                ensure_bd_runtime_custom_types "$dolt_database" "$custom_types"
                ensure_bd_runtime_issue_prefix "$dolt_database" "$prefix"
                backfill_project_id_if_missing "$dir"
                exit 0
            fi
            echo "warning: database '$dolt_database' missing bd schema; re-initializing" >&2
            bd_init_force="--force"
        else
            echo "warning: database '$dolt_database' not registered; re-initializing" >&2
            bd_init_force="--force"
        fi
    fi

    local host
    host=$(connect_host)

    # Register the database with the running server first. CREATE DATABASE
    # IF NOT EXISTS both creates the on-disk directory and registers it in
    # the server's catalog. This is the upstream gastown pattern — when the
    # server is running, always go through SQL rather than dolt init on disk.
    ensure_database_registered "$dolt_database" || true

    # Run bd init in server mode through the pinned wrapper so the fallback
    # path uses the same authenticated Dolt target as the rest of init.
    # Metadata-only scopes already look initialized to bd, so schema-repair
    # fallback must force reinit to seed the missing tables into the pinned DB.
    # Always pass the pinned server database explicitly; `-p` controls the
    # visible issue prefix, while `--database` tells bd which existing Dolt
    # database to initialize. Without `--database`, bd can seed beads_<prefix>
    # and leave the pinned database schema-less.
    run_bd_init_pinned "$dir" "$prefix" "$dolt_database" "$host" "${bd_init_force:+true}"

    ensure_database_registered "$dolt_database" || true

    # GC owns canonical metadata/config normalization after this backend
    # bridge returns. Keep bd-specific config/migration here only.
    ensure_beads_dir_permissions "$dir"
    if ! wait_for_bd_runtime_schema "$dolt_database"; then
        if [ "${GC_BD_INIT_RETRY:-0}" != "1" ]; then
            if [ -n "$bd_init_force" ]; then
                # Metadata-only scopes can still confuse bd's first forced server init.
                # Drop the preseeded metadata and retry through a fresh top-level
                # invocation, matching the successful manual recovery path.
                rm -f "$dir/.beads/metadata.json"
            fi
            echo "warning: bd schema for '$dolt_database' not visible after init; retrying init" >&2
            GC_BD_INIT_RETRY=1 exec "$0" init "$dir" "$prefix" "$dolt_database"
            die "failed to re-exec init for $dir"
        fi
        die "bd schema not visible for $dolt_database after init"
    fi

    # Configure custom bead types (required since beads v0.46.0).
    run_bd_pinned "$dir" config set types.custom "$custom_types" 2>/dev/null || true
    ensure_bd_runtime_custom_types "$dolt_database" "$custom_types"

    # Keep bd's runtime config in sync with GC's canonical prefix. This is
    # compatibility state for raw bd operations, not a second GC authority.
    ensure_bd_runtime_issue_prefix "$dolt_database" "$prefix"

    # Ensure database has repository fingerprint (upstream GH #25).
    # Fresh bd init already writes project_id on current upstream; only pay the
    # migration cost when metadata still lacks it.
    backfill_project_id_if_missing "$dir"

    # Drop orphan database created by bd init (upstream gt-sv1h) only after
    # the pinned database schema is visible. Some bd builds appear to stage
    # schema work before the pinned catalog entry is fully adopted; deleting
    # beads_<prefix> too early can discard the only initialized schema.
    local orphan_db="beads_${prefix}"
    if [ "$orphan_db" != "$dolt_database" ]; then
        server_sql "DROP DATABASE IF EXISTS \`$orphan_db\`" >/dev/null 2>&1 || true
    fi

    normalize_scope_after_init "$dir" "$prefix" "$dolt_database"
}


scope_store_dir() {
    if [ -n "${GC_STORE_ROOT:-}" ]; then
        printf '%s\n' "$GC_STORE_ROOT"
        return
    fi
    printf '%s\n' "$GC_CITY_PATH"
}

op_store_bridge() {
    local scope_dir host gc_bin
    scope_dir=$(scope_store_dir)
    host=$(connect_host)

    gc_bin=$(resolve_gc_bin)
    if [ -z "$gc_bin" ]; then
        die "gc binary not found for exec store operations"
    fi

    GC_DOLT_PASSWORD="$DOLT_PASSWORD"     BEADS_DOLT_PASSWORD="$DOLT_PASSWORD"     "$gc_bin" bd-store-bridge         --dir "$scope_dir"         --host "$host"         --port "$DOLT_PORT"         --user "$DOLT_USER"         "$@"
    return $?
}
op_health() {
    local conn_count=""

    # TCP check.
    if ! tcp_check; then
        die "dolt server not reachable on $(connect_host):$DOLT_PORT"
    fi

    if load_health_check_from_gc; then
        if [ "$GC_HEALTH_QUERY_READY" != "true" ]; then
            die "dolt query probe failed (SELECT active_branch())"
        fi
        if ! is_remote && [ "$GC_HEALTH_READ_ONLY" = "true" ]; then
            die "dolt server is in read-only mode"
        fi
        conn_count="$GC_HEALTH_CONNECTION_COUNT"
    else
        # Query probe.
        if ! do_query_probe; then
            die "dolt query probe failed (SELECT active_branch())"
        fi

        # Imposter detection disabled: TCP + query probe passed, server is
        # healthy. False-positive imposter kills (caused by inherited supervisor
        # fds and stale state files) were actively harmful — killing the
        # managed dolt server and losing all in-flight work.

        # Read-only detection (local only).
        if ! is_remote; then
            if check_read_only; then
                die "dolt server is in read-only mode"
            fi
        fi

        # Connection capacity warning (non-fatal, single query).
        conn_count=$(get_connection_count 2>/dev/null) || conn_count=""
    fi

    if [ -n "$conn_count" ] && [ "$conn_count" -ge 800 ] 2>/dev/null; then
        echo "warning: connection count ($conn_count) near capacity (80% of 1000)" >&2
    fi
}

# op_probe checks if the dolt server is available.
# Exit 0 = running, exit 2 = not running.
op_probe() {
    if is_remote; then
        # Remote server — check TCP.
        if tcp_check; then
            exit 0
        else
            exit 2
        fi
    fi

    if load_probe_managed_from_gc; then
        if [ "$GC_PROBE_RUNNING" = "true" ]; then
            exit 0
        fi
        exit 2
    fi

    # Local server — check port holder and verify identity.
    local holder
    if load_managed_process_inspection_from_gc; then
        holder="$GC_PORT_HOLDER_PID"
        if [ -n "$holder" ]; then
            if [ "$GC_PORT_HOLDER_OWNED" = true ] && tcp_check; then
                exit 0
            fi
            exit 2
        fi
    fi
    holder=$(find_port_holder)
    if [ -z "$holder" ]; then
        exit 2
    fi

    # Verify it's our server.
    if verify_our_server "$holder" && tcp_check; then
        exit 0
    fi

    # Imposter or unreachable.
    exit 2
}

# op_recover stops the dolt server, restarts it, and verifies health.
op_recover() {
    if is_remote; then
        die "recovery not supported for remote dolt servers"
    fi

    if load_recover_managed_from_gc; then
        if [ "$GC_RECOVER_DIAGNOSED_READ_ONLY" = "true" ]; then
            echo "detected read-only dolt server — restarting" >&2
        fi
        DOLT_PORT="$GC_RECOVER_PORT"
        return 0
    elif [ "$GC_RECOVER_MANAGED_USED" = "true" ]; then
        if [ "$GC_RECOVER_DIAGNOSED_READ_ONLY" = "true" ]; then
            echo "detected read-only dolt server — restarting" >&2
        fi
        DOLT_PORT="$GC_RECOVER_PORT"
        die "dolt recovery via gc helper failed"
    fi

    # Diagnose: check for read-only before stopping.
    if tcp_check; then
        if load_health_check_from_gc; then
            if [ "$GC_HEALTH_READ_ONLY" = "true" ]; then
                echo "detected read-only dolt server — restarting" >&2
            fi
        elif check_read_only; then
            echo "detected read-only dolt server — restarting" >&2
        fi
    fi

    # Stop.
    op_stop_impl || true

    # Clean startup artifacts before restart.
    run_preflight_cleanup

    # Wait a moment for cleanup.
    sleep 1

    # Restart.
    op_start

    # Verify health.
    op_health
}

# op_stop_impl is the internal stop implementation (no exit on "not running").
op_stop_impl() {
    GC_STOP_HAD_PID="false"
    if is_remote; then
        return 0
    fi

    if load_stop_managed_from_gc; then
        return 0
    elif [ "$GC_STOP_MANAGED_USED" = "true" ]; then
        return 1
    fi

    local pid owned holder
    owned="false"
    if ! load_managed_process_inspection_from_gc; then
        pid=$(find_dolt_pid)
        if [ -n "$pid" ] && verify_our_server "$pid"; then
            owned="true"
        else
            holder=$(find_port_holder)
            if [ -n "$holder" ] && verify_our_server "$holder"; then
                pid="$holder"
                owned="true"
            else
                pid=""
            fi
        fi
    else
        if [ -n "${GC_MANAGED_PID:-}" ] && [ "${GC_MANAGED_OWNED:-false}" = "true" ]; then
            pid="$GC_MANAGED_PID"
            owned="true"
        elif [ -n "${GC_PORT_HOLDER_PID:-}" ] && [ "${GC_PORT_HOLDER_OWNED:-false}" = "true" ]; then
            pid="$GC_PORT_HOLDER_PID"
            owned="true"
        else
            pid=""
        fi
    fi
    if [ -z "$pid" ] || [ "$owned" != "true" ]; then
        # No process found — clean up state files.
        save_state 0 false
        rm -f "$PID_FILE"
        return 0
    fi
    GC_STOP_HAD_PID="true"

    # SIGTERM and wait (10 × 500ms = 5s grace, matches upstream).
    kill "$pid" 2>/dev/null || true
    local waited=0
    while [ "$waited" -lt 10 ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            # Clean up state files.
            save_state 0 false
            rm -f "$PID_FILE"
            return 0
        fi
        sleep 0.5 2>/dev/null || sleep 1
        waited=$((waited + 1))
    done

    # Force kill if still running.
    kill -9 "$pid" 2>/dev/null || true
    sleep 1

    # Clean up state files.
    save_state 0 false
    rm -f "$PID_FILE"
}

# op_stop stops the dolt server.
op_stop() {
    if is_remote; then
        exit 2
    fi

    if ! op_stop_impl; then
        die "failed to stop managed dolt server"
    fi
    if [ "$GC_STOP_HAD_PID" != "true" ]; then
        exit 2
    fi
}

# op_shutdown is a legacy alias for stop.
op_shutdown() {
    op_stop
}

# --- Main ---

# GC_DOLT=skip → no-op for all operations.
if [ "$GC_DOLT" = "skip" ]; then
    exit 2
fi

op="$1"
shift || true

# Validate GC_CITY_PATH.
if [ -z "$GC_CITY_PATH" ]; then
    die "GC_CITY_PATH not set"
fi

# Set derived paths.
GC_DIR="$GC_CITY_PATH/.gc"
BEADS_DIR_ROOT="$GC_CITY_PATH/.beads"

# Prefer GC-owned runtime layout derivation when the current gc binary is
# available. Fall back to the legacy shell derivation for compatibility.
if ! load_runtime_layout_from_gc; then
    if [ -n "$GC_PACK_STATE_DIR" ]; then
        PACK_STATE_DIR="$GC_PACK_STATE_DIR"
    elif [ -n "$GC_CITY_RUNTIME_DIR" ]; then
        PACK_STATE_DIR="$GC_CITY_RUNTIME_DIR/packs/dolt"
    else
        PACK_STATE_DIR="$GC_DIR/runtime/packs/dolt"
    fi

    # All data lives under .beads/dolt by default. Runtime state (logs, PID,
    # lock) lives under PACK_STATE_DIR. GC may project the fully resolved paths so
    # this backend bridge does not have to own runtime layout policy.
    DATA_DIR="${GC_DOLT_DATA_DIR:-$BEADS_DIR_ROOT/dolt}"
    LOG_FILE="${GC_DOLT_LOG_FILE:-$PACK_STATE_DIR/dolt.log}"
    STATE_FILE="${GC_DOLT_STATE_FILE:-$PACK_STATE_DIR/dolt-provider-state.json}"
    PID_FILE="${GC_DOLT_PID_FILE:-$PACK_STATE_DIR/dolt.pid}"
    LOCK_FILE="${GC_DOLT_LOCK_FILE:-$PACK_STATE_DIR/dolt.lock}"
    CONFIG_FILE="${GC_DOLT_CONFIG_FILE:-$PACK_STATE_DIR/dolt-config.yaml}"
fi
mkdir -p "$DATA_DIR" "$PACK_STATE_DIR"

# Resolve DOLT_PORT now that STATE_FILE is set.
DOLT_PORT=$(allocate_port)

case "$op" in
    start)        op_start ;;
    ensure-ready) op_ensure_ready ;;
    init)         op_init "$@" ;;
    create|get|update|close|list|ready|children|list-by-label|set-metadata|delete|dep-add|dep-remove|dep-list)
                  op_store_bridge "$op" "$@" ;;
    health)       op_health ;;
    probe)        op_probe ;;
    recover)      op_recover ;;
    stop)         op_stop ;;
    shutdown)     op_shutdown ;;
    *)            exit 2 ;;  # Unknown operation — forward compatible.
esac
