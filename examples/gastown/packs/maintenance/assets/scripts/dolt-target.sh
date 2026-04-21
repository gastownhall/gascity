#!/usr/bin/env sh
# Shared Dolt SQL connection setup for maintenance scripts.

GC_CITY_PATH="${GC_CITY_PATH:-${GC_CITY:-.}}"

read_runtime_state_flag() (
    state_file="$1"
    key="$2"
    [ -f "$state_file" ] || return 0
    sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\(true\\|false\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

read_runtime_state_number() (
    state_file="$1"
    key="$2"
    [ -f "$state_file" ] || return 0
    sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([0-9][0-9]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

read_runtime_state_string() (
    state_file="$1"
    key="$2"
    [ -f "$state_file" ] || return 0
    sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

managed_runtime_port() (
    state_file="$1"
    expected_data_dir="$2"

    [ -f "$state_file" ] || return 0

    running=$(read_runtime_state_flag "$state_file" running)
    pid=$(read_runtime_state_number "$state_file" pid)
    port=$(read_runtime_state_number "$state_file" port)
    data_dir=$(read_runtime_state_string "$state_file" data_dir)

    [ "$running" = "true" ] || return 0
    [ -n "$pid" ] || return 0
    [ -n "$port" ] || return 0
    [ "$data_dir" = "$expected_data_dir" ] || return 0
    kill -0 "$pid" 2>/dev/null || return 0

    if command -v lsof >/dev/null 2>&1; then
        holder_pid=$(lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -1 || true)
        [ -n "$holder_pid" ] || return 0
        [ "$holder_pid" = "$pid" ] || return 0
    else
        return 0
    fi

    printf '%s\n' "$port"
)

if [ -z "${GC_DOLT_PORT:-}" ]; then
    DOLT_STATE_FILE="${GC_DOLT_STATE_FILE:-${GC_CITY_RUNTIME_DIR:-$GC_CITY_PATH/.gc/runtime}/packs/dolt/dolt-state.json}"
    GC_DOLT_PORT="$(managed_runtime_port "$DOLT_STATE_FILE" "$GC_CITY_PATH/.beads/dolt")"
fi

: "${GC_DOLT_PORT:=3307}"

case "$GC_DOLT_PORT" in
    ''|*[!0-9]*)
        echo "maintenance: invalid GC_DOLT_PORT: $GC_DOLT_PORT" >&2
        exit 1
        ;;
esac

DOLT_HOST="${GC_DOLT_HOST:-127.0.0.1}"
DOLT_PORT="$GC_DOLT_PORT"
DOLT_USER="${GC_DOLT_USER:-root}"

# Match the Dolt pack commands, which currently use non-TLS SQL connections.
# If TLS becomes a supported GC_DOLT_* contract, add it in the Dolt pack first.
dolt_sql() {
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" dolt --host "$DOLT_HOST" --port "$DOLT_PORT" --user "$DOLT_USER" --no-tls sql "$@"
}
