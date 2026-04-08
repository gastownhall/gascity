#!/bin/sh
# resolve-port.sh — canonical dolt port resolution for shell scripts.
#
# Source this file and call resolve_dolt_port to get the current port.
# Mirrors the priority of currentDoltPort() in Go (beads_provider_lifecycle.go):
#   port file → runtime state file → GC_DOLT_PORT env var → error.
#
# The env var is checked LAST because it is set once at session birth and
# goes stale after any dolt server restart. The port file and state file
# are written on each server start and are always current.
#
# Usage:
#   . /path/to/resolve-port.sh
#   port=$(resolve_dolt_port) || exit 1

# resolve_dolt_port — prints the current dolt port to stdout.
# Returns 1 and prints an error to stderr if no port can be determined.
resolve_dolt_port() {
    local city="${GC_CITY_ROOT:-${GC_CITY_PATH:-.}}"
    local port=""

    # 1. Port file — written on each server start, most authoritative.
    local port_file="$city/.beads/dolt-server.port"
    if [ -f "$port_file" ]; then
        port=$(cat "$port_file" 2>/dev/null | tr -d '[:space:]')
    fi

    # 2. Runtime state file (controller-managed dolt-state.json).
    if [ -z "$port" ]; then
        local runtime_dir="${GC_CITY_RUNTIME_DIR:-$city/.gc/runtime}"
        local state_file="$runtime_dir/packs/dolt/dolt-state.json"
        if [ -f "$state_file" ]; then
            port=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$state_file" | head -1)
        fi
    fi

    # 3. Env var — may be stale in long-lived sessions, lowest priority.
    if [ -z "$port" ]; then
        port="${GC_DOLT_PORT:-}"
    fi

    if [ -z "$port" ]; then
        echo "ERROR: cannot determine dolt port (no port file, no state file, no GC_DOLT_PORT)" >&2
        return 1
    fi

    echo "$port"
}
