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

# managed_runtime_pid_listener_state answers "is $pid the process listening on
# $port" from /proc, without lsof. `lsof -iTCP:<port>` walks the descriptors of
# every process on the host, which costs seconds of kernel CPU per call on a
# busy machine; this reads the kernel's TCP tables for the listening socket's
# inode and then only $pid's own fd directory.
#   0  $pid holds the listener
#   1  something else holds it ($pid's descriptors are readable and lack it)
#   2  nothing is listening on $port
#   3  cannot tell here: no /proc/net (darwin), or $pid's descriptors are
#      unreadable (exited, or another user's) -- callers fall back to lsof
managed_runtime_pid_listener_state() (
    pid="$1"
    port="$2"

    case "$pid" in
        ''|*[!0-9]*)
            return 3
            ;;
    esac
    case "$port" in
        ''|*[!0-9]*)
            return 3
            ;;
    esac

    tables=""
    for table in /proc/net/tcp /proc/net/tcp6; do
        [ -r "$table" ] && tables="$tables $table"
    done
    [ -n "$tables" ] || return 3

    # st 0A is TCP_LISTEN; the local port is the hex after the last colon of
    # column 2 and the socket inode is column 10.
    # shellcheck disable=SC2086 # $tables is a space-separated path list.
    inodes=$(awk -v port_hex="$(printf '%04X' "$port")" '
        $4 == "0A" && $10 != "0" {
            n = split($2, addr, ":")
            if (addr[n] == port_hex) print $10
        }
    ' $tables 2>/dev/null) || return 3
    [ -n "$inodes" ] || return 2

    [ -r "/proc/$pid/fd" ] && [ -x "/proc/$pid/fd" ] || return 3
    found_fd=0
    for fd in /proc/"$pid"/fd/*; do
        [ -e "$fd" ] || [ -L "$fd" ] || continue
        found_fd=1
        target=$(readlink "$fd" 2>/dev/null) || continue
        for inode in $inodes; do
            [ "$target" = "socket:[$inode]" ] && return 0
        done
    done
    [ "$found_fd" = 1 ] || return 3
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

    listener_state=0
    managed_runtime_pid_listener_state "$pid" "$port" || listener_state=$?
    case "$listener_state" in
        0)
            printf '%s\n' "$port"
            return 0
            ;;
        1)
            return 0
            ;;
        2)
            holder_pid=""
            ;;
        *)
            holder_pid=$(managed_runtime_listener_pid "$port" || true)
            ;;
    esac
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

if [ -n "${GC_DOLT_STATE_FILE:-}" ]; then
    DOLT_STATE_FILE="$GC_DOLT_STATE_FILE"
else
    DOLT_PACK_DIR="${GC_CITY_RUNTIME_DIR:-$GC_CITY_PATH/.gc/runtime}/packs/dolt"
    DOLT_STATE_FILE="$DOLT_PACK_DIR/dolt-state.json"
    DOLT_PROVIDER_STATE_FILE="$DOLT_PACK_DIR/dolt-provider-state.json"
fi

DOLT_SYSTEM_PACKS_DIR="${GC_SYSTEM_PACKS_DIR:-$GC_CITY_PATH/.gc/system/packs}"
DOLT_PORT_RESOLVE_SCRIPT="$DOLT_SYSTEM_PACKS_DIR/dolt/assets/scripts/port_resolve.sh"
if [ ! -f "$DOLT_PORT_RESOLVE_SCRIPT" ]; then
    DOLT_PORT_RESOLVE_SCRIPT="$DOLT_SYSTEM_PACKS_DIR/bd/dolt/assets/scripts/port_resolve.sh"
fi
if [ ! -f "$DOLT_PORT_RESOLVE_SCRIPT" ] && [ -n "${SCRIPT_DIR:-}" ]; then
    DOLT_SOURCE_SCRIPT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../../../../../../examples/bd/dolt/assets/scripts" 2>/dev/null && pwd || true)
    if [ -n "$DOLT_SOURCE_SCRIPT_DIR" ]; then
        DOLT_PORT_RESOLVE_SCRIPT="$DOLT_SOURCE_SCRIPT_DIR/port_resolve.sh"
    fi
fi

# Proxied-scope guard. A city on bd-owned proxied Dolt (the default for new
# cities since #6273: <city>/.beads/metadata.json dolt_mode=proxied-server)
# has no gc-managed sql-server: bd starts, supervises and stops the Dolt
# child under its own db-proxy-child, publishes no endpoint for gc to
# project, and owns every write to that database. These maintenance scripts
# speak raw SQL (DML plus DOLT_COMMIT) to a gc-resolved port and assume one
# server holding every scope's database, so they have no safe target there.
# Skip (exit 0) with a typed line instead of dying in port resolution
# (exit 78, "cannot resolve runtime port") on every cooldown. The check runs
# before the GC_DOLT_PORT shortcut below so a port projected for a proxied
# scope's external upstream cannot route raw SQL around bd's proxy either.
# bd_owns_proxied_scope is the dolt pack's sh twin of cmd/gc's
# scopeBindingIsProviderOwnedProxied (pinned by
# examples/bd/dolt/proxied_scope_test.go).
DOLT_PROXIED_SCOPE_SCRIPT="$(dirname -- "$DOLT_PORT_RESOLVE_SCRIPT")/proxied_scope.sh"
if [ -f "$DOLT_PROXIED_SCOPE_SCRIPT" ]; then
    . "$DOLT_PROXIED_SCOPE_SCRIPT"
    if bd_owns_proxied_scope "$GC_CITY_PATH"; then
        echo "core: city Dolt is bd-owned (dolt_mode=proxied-server); direct-SQL maintenance is not supported on proxied scopes; skipping ${0##*/}"
        exit 0
    fi
fi

# No-Dolt guard. Core ships these maintenance scripts to every city, but
# they only have work when the city has a Dolt target. Skip (exit 0)
# instead of failing (exit 78) when no Dolt evidence exists, so cities on
# file/postgres backends do not log a recurring OrderFailed every cooldown.
# Order dispatch projects GC_DOLT_PORT explicitly — empty when the city has
# no canonical Dolt target — so a non-empty port here is city-derived, not
# inherited operator environment.
core_city_has_dolt_target() {
    [ -n "${GC_DOLT_PORT:-}" ] && return 0
    [ -f "$DOLT_STATE_FILE" ] && return 0
    [ -n "${DOLT_PROVIDER_STATE_FILE:-}" ] && [ -f "$DOLT_PROVIDER_STATE_FILE" ] && return 0
    [ -d "$GC_CITY_PATH/.beads/dolt" ] && return 0
    return 1
}

if ! core_city_has_dolt_target; then
    echo "core: no managed dolt target for this city; skipping ${0##*/}"
    exit 0
fi

. "${DOLT_PORT_RESOLVE_SCRIPT:?port_resolve.sh not resolved}"
if [ -n "${DOLT_PROVIDER_STATE_FILE:-}" ]; then
    GC_DOLT_PORT="$(resolve_dolt_port_or_die "$DOLT_STATE_FILE" "$DOLT_PROVIDER_STATE_FILE" "$GC_CITY_PATH/.beads/dolt" "$GC_CITY_PATH")" || exit $?
else
    GC_DOLT_PORT="$(resolve_dolt_port_or_die "$DOLT_STATE_FILE" "$GC_CITY_PATH/.beads/dolt" "$GC_CITY_PATH")" || exit $?
fi

case "$GC_DOLT_PORT" in
    ''|*[!0-9]*)
        echo "core: invalid GC_DOLT_PORT: $GC_DOLT_PORT" >&2
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

# has_wisps_table reports whether $1 contains a `wisps` table. Maintenance
# scripts that iterate user databases use this as a proxy for "is this DB
# bd-managed?" — every bd-managed schema has a wisps table. Databases that
# exist on the server without bd schema (orphan CREATE DATABASEs, system
# schemas not on the is_user_database blocklist, partial migrations) have
# nothing for the maintenance scripts to do, and querying their tables just
# produces spurious "table not found" anomalies / failure-summary entries.
# See gastownhall/gascity#1816.
#
# Caller must have already validated $1 via valid_database_identifier — this
# helper does not re-quote against injection. On probe failure (dolt
# unreachable, connection dropped, etc.) returns 0 (success/has-wisps) so
# the caller falls through to its normal queries; those will fail in the
# same way and surface the dolt-side problem through the script's regular
# error-handling path.
has_wisps_table() (
    db="$1"
    if ! output=$(dolt_sql -r csv -q "SHOW TABLES FROM \`$db\` LIKE 'wisps'" 2>/dev/null); then
        return 0
    fi
    [ "$(printf '%s\n' "$output" | tail -n +2 | head -1 | tr -d '\r')" = "wisps" ]
)
