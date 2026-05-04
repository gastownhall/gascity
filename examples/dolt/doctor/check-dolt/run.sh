#!/usr/bin/env bash
# Pack doctor check: verify Dolt binary and required tools.
#
# Exit codes: 0=OK, 1=Warning, 2=Error
# stdout: first line=message, rest=details

if ! command -v dolt >/dev/null 2>&1; then
    echo "dolt binary not found"
    echo "install dolt: https://docs.dolthub.com/introduction/installation"
    exit 2
fi

# Check flock (required for concurrent start prevention).
if ! command -v flock >/dev/null 2>&1; then
    echo "flock not found (needed for Dolt server locking)"
    echo "Install: apt install util-linux (Linux) or brew install flock (macOS)"
    exit 2
fi

# Check lsof (required for port conflict detection).
if ! command -v lsof >/dev/null 2>&1; then
    echo "lsof not found (needed for port conflict detection)"
    echo "Install: apt install lsof (Linux) or available by default (macOS)"
    exit 2
fi

timeout_bin=""
if command -v gtimeout >/dev/null 2>&1; then
    timeout_bin="gtimeout"
elif command -v timeout >/dev/null 2>&1; then
    timeout_bin="timeout"
fi

run_bounded() {
    limit="$1"
    shift
    if [ -n "$timeout_bin" ]; then
        "$timeout_bin" --kill-after=2 "$limit" "$@"
        return $?
    fi
    if command -v python3 >/dev/null 2>&1; then
        python3 - "$limit" "$@" <<'PY'
import subprocess
import sys

limit = float(sys.argv[1])
cmd = sys.argv[2:]
try:
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=limit)
except subprocess.TimeoutExpired as exc:
    sys.stdout.write(exc.stdout or "")
    sys.stderr.write(exc.stderr or "")
    sys.exit(124)
sys.stdout.write(proc.stdout)
sys.stderr.write(proc.stderr)
sys.exit(proc.returncode)
PY
        return $?
    fi
    echo "timeout/gtimeout/python3 not found; cannot run bounded command" >&2
    return 124
}

version_output=$(run_bounded 10 dolt version 2>/dev/null)
version_status=$?
if [ "$version_status" -ne 0 ]; then
    if [ "$version_status" -eq 124 ]; then
        echo "dolt version timed out after 10s"
        echo "retry after fixing local Dolt startup or PATH"
        exit 1
    fi
    echo "unable to run dolt version"
    echo "install dolt: https://docs.dolthub.com/introduction/installation"
    exit 1
fi
version=$(printf '%s\n' "$version_output" | head -1)
if [ -z "$version" ]; then
    echo "unrecognized dolt version output: $version"
    echo "install dolt: https://docs.dolthub.com/introduction/installation"
    exit 1
fi

# Require dolt >= 1.86.2 due to upstream GC/writer deadlock fix.
# Older versions hang sql-server during dolt_backup('sync', ...) under
# heavy concurrent write load; the watchdog then force-kills the server.
# See dolthub/dolt commit ccf7bde206 (PR #10876).
required="1.86.2"

parse_dolt_version() {
    local input="$1"
    local token
    local core
    local version_core
    token=$(printf '%s' "$input" | sed -E 's/^[Dd]olt[[:space:]]+[Vv]ersion[[:space:]]+//; s/[[:space:]].*$//; s/^v//')
    version_core="${token%%+*}"
    if [[ "$version_core" == *-* ]]; then
        core="${version_core%%-*}"
        if [[ ! "$core" =~ ^[0-9]+[.][0-9]+[.][0-9]+$ ]]; then
            return 1
        fi
        return 2
    fi
    token="$version_core"
    if [[ ! "$token" =~ ^[0-9]+[.][0-9]+[.][0-9]+$ ]]; then
        return 1
    fi
    printf '%s\n' "$token"
}

version_lt() {
    local a="$1"
    local b="$2"
    local IFS=.
    local a_major a_minor a_patch b_major b_minor b_patch
    read -r a_major a_minor a_patch <<<"$a"
    read -r b_major b_minor b_patch <<<"$b"
    if ((10#$a_major != 10#$b_major)); then
        ((10#$a_major < 10#$b_major))
        return $?
    fi
    if ((10#$a_minor != 10#$b_minor)); then
        ((10#$a_minor < 10#$b_minor))
        return $?
    fi
    ((10#$a_patch < 10#$b_patch))
}

parse_status=0
ver_str=$(parse_dolt_version "$version") || parse_status=$?
if [ "$parse_status" -eq 2 ]; then
    echo "$version is a pre-release build (need final >= $required) — upgrade required"
    echo "Reason: pre-release builds are not guaranteed to include dolthub/dolt commit ccf7bde206."
    echo "Install: https://github.com/dolthub/dolt/releases"
    exit 2
fi
if [ "$parse_status" -ne 0 ]; then
    echo "unrecognized dolt version output: $version"
    echo "install dolt: https://docs.dolthub.com/introduction/installation"
    exit 1
fi
if version_lt "$ver_str" "$required"; then
    echo "dolt $ver_str is too old (need >= $required) — upgrade required"
    echo "Reason: <1.86.2 has a GC/writer deadlock that hangs sql-server during dolt_backup sync under heavy commit load. See dolthub/dolt commit ccf7bde206."
    echo "Install: https://github.com/dolthub/dolt/releases"
    exit 2
fi

echo "dolt available ($version), flock ok, lsof ok"
exit 0
