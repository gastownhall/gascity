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

echo "dolt available ($version), flock ok, lsof ok"
exit 0
