#!/usr/bin/env bash
# Tests for resolve-port.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "$TMPDIR_ROOT"' EXIT

fail() { echo "FAIL: $1"; FAIL=$((FAIL + 1)); }
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }

# Each test runs in a subshell to isolate env vars from the real session.

# --- Test 1: port file takes priority over env var ---
test_port_file_beats_env() {
    local city="$TMPDIR_ROOT/t1"
    mkdir -p "$city/.beads"
    echo "4200" > "$city/.beads/dolt-server.port"

    result=$(
        export GC_CITY_ROOT="$city" GC_CITY_PATH="$city" GC_DOLT_PORT="9999"
        unset GC_CITY_RUNTIME_DIR
        . "$SCRIPT_DIR/resolve-port.sh"
        resolve_dolt_port
    )

    if [ "$result" = "4200" ]; then
        pass "port file beats env var"
    else
        fail "port file beats env var (got '$result', want '4200')"
    fi
}

# --- Test 2: state file used when no port file ---
test_state_file_fallback() {
    local city="$TMPDIR_ROOT/t2"
    mkdir -p "$city/.gc/runtime/packs/dolt"
    echo '{"port": 5555, "running": true}' > "$city/.gc/runtime/packs/dolt/dolt-state.json"

    result=$(
        export GC_CITY_ROOT="$city" GC_CITY_PATH="$city" GC_DOLT_PORT=""
        unset GC_CITY_RUNTIME_DIR
        . "$SCRIPT_DIR/resolve-port.sh"
        resolve_dolt_port
    )

    if [ "$result" = "5555" ]; then
        pass "state file fallback works"
    else
        fail "state file fallback (got '$result', want '5555')"
    fi
}

# --- Test 3: env var used as last resort ---
test_env_var_last_resort() {
    local city="$TMPDIR_ROOT/t3"
    mkdir -p "$city"

    result=$(
        export GC_CITY_ROOT="$city" GC_CITY_PATH="$city" GC_DOLT_PORT="7777"
        unset GC_CITY_RUNTIME_DIR
        . "$SCRIPT_DIR/resolve-port.sh"
        resolve_dolt_port
    )

    if [ "$result" = "7777" ]; then
        pass "env var used as last resort"
    else
        fail "env var last resort (got '$result', want '7777')"
    fi
}

# --- Test 4: error when no source available ---
test_error_when_no_port() {
    local city="$TMPDIR_ROOT/t4"
    mkdir -p "$city"

    if (
        export GC_CITY_ROOT="$city" GC_CITY_PATH="$city" GC_DOLT_PORT=""
        unset GC_CITY_RUNTIME_DIR
        . "$SCRIPT_DIR/resolve-port.sh"
        resolve_dolt_port
    ) >/dev/null 2>&1; then
        fail "should error when no port source available"
    else
        pass "errors when no port source available"
    fi
}

# --- Test 5: port file with whitespace is trimmed ---
test_port_file_whitespace() {
    local city="$TMPDIR_ROOT/t5"
    mkdir -p "$city/.beads"
    printf "  4201\n  " > "$city/.beads/dolt-server.port"

    result=$(
        export GC_CITY_ROOT="$city" GC_CITY_PATH="$city" GC_DOLT_PORT=""
        unset GC_CITY_RUNTIME_DIR
        . "$SCRIPT_DIR/resolve-port.sh"
        resolve_dolt_port
    )

    if [ "$result" = "4201" ]; then
        pass "port file whitespace trimmed"
    else
        fail "port file whitespace (got '$result', want '4201')"
    fi
}

# --- Test 6: state file beats env var ---
test_state_file_beats_env() {
    local city="$TMPDIR_ROOT/t6"
    mkdir -p "$city/.gc/runtime/packs/dolt"
    echo '{"port": 6666}' > "$city/.gc/runtime/packs/dolt/dolt-state.json"

    result=$(
        export GC_CITY_ROOT="$city" GC_CITY_PATH="$city" GC_DOLT_PORT="8888"
        unset GC_CITY_RUNTIME_DIR
        . "$SCRIPT_DIR/resolve-port.sh"
        resolve_dolt_port
    )

    if [ "$result" = "6666" ]; then
        pass "state file beats env var"
    else
        fail "state file beats env var (got '$result', want '6666')"
    fi
}

# Run all tests
test_port_file_beats_env
test_state_file_fallback
test_env_var_last_resort
test_error_when_no_port
test_port_file_whitespace
test_state_file_beats_env

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
