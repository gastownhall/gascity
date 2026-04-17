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

version=$(dolt version 2>/dev/null | head -1)
echo "dolt available ($version), flock ok, lsof ok"
exit 0
