#!/usr/bin/env bash
# Pack doctor check: verify binaries required by maintenance orders.
#
# Exit codes: 0=OK, 1=Warning, 2=Error
# stdout: first line=message, rest=details

missing=()
for bin in jq; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        missing+=("$bin")
    fi
done

gh_available=1
if ! command -v gh >/dev/null 2>&1; then
    gh_available=0
fi

if [ ${#missing[@]} -gt 0 ]; then
    echo "${#missing[@]} required binary(ies) missing"
    for bin in "${missing[@]}"; do
        echo "$bin not found in PATH"
    done
    if [ "$gh_available" -eq 0 ]; then
        echo "gh not found in PATH; GitHub gate checks will be skipped"
    fi
    exit 2
fi

if [ "$gh_available" -eq 0 ]; then
    echo "all required binaries available (jq)"
    echo "optional gh not found in PATH; GitHub gate checks will be skipped"
    exit 0
fi

echo "all required binaries available (jq); optional gh available"
exit 0
