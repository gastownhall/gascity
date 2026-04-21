#!/usr/bin/env sh
# Shared Dolt SQL connection setup for maintenance scripts.

GC_CITY_PATH="${GC_CITY_PATH:-${GC_CITY:-.}}"

read_runtime_state_flag() (
    state_file="$1"
    key="$2"
    [ -f "$state_file" ] || return 0
    value=$(sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([^,}[:space:]]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true)
    case "$value" in
        true|false)
            printf '%s\n' "$value"
            ;;
    esac
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

pid_is_running() (
    pid="$1"

    case "$pid" in
        ''|*[!0-9]*)
            return 1
            ;;
    esac

    if kill -0 "$pid" 2>/dev/null; then
        return 0
    fi

    if command -v ps >/dev/null 2>&1; then
        ps_pid=$(ps -p "$pid" -o pid= 2>/dev/null | tr -d '[:space:]')
        [ "$ps_pid" = "$pid" ] && return 0
    fi

    return 1
)

managed_runtime_listener_pid() (
    port="$1"

    case "$port" in
        ''|*[!0-9]*)
            return 0
            ;;
    esac

    if ! command -v lsof >/dev/null 2>&1; then
        return 0
    fi

    lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null \
        | while IFS= read -r holder_pid; do
            case "$holder_pid" in
                ''|*[!0-9]*)
                    continue
                    ;;
            esac
            if pid_is_running "$holder_pid"; then
                printf '%s\n' "$holder_pid"
                break
            fi
        done
)

managed_runtime_tcp_reachable() (
    port="$1"

    case "$port" in
        ''|*[!0-9]*)
            return 1
            ;;
    esac

    if command -v nc >/dev/null 2>&1; then
        nc -z 127.0.0.1 "$port" >/dev/null 2>&1
        return $?
    fi

    if command -v python3 >/dev/null 2>&1; then
        python3 - "$port" <<'PY' >/dev/null 2>&1
import socket
import sys

sock = socket.socket()
sock.settimeout(0.25)
try:
    sock.connect(("127.0.0.1", int(sys.argv[1])))
except OSError:
    raise SystemExit(1)
finally:
    sock.close()
PY
        return $?
    fi

    return 1
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
    pid_is_running "$pid" || return 0

    holder_pid=$(managed_runtime_listener_pid "$port" || true)
    if [ -n "$holder_pid" ]; then
        [ "$holder_pid" = "$pid" ] || return 0
        printf '%s\n' "$port"
        return 0
    fi

    if ! managed_runtime_tcp_reachable "$port"; then
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
