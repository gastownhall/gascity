#!/usr/bin/env bash
# Pack doctor check: verify binaries required by maintenance orders.
#
# Exit codes: 0=OK, 1=Warning, 2=Error
# stdout: first line=message, rest=details
#
# NOTE on `bd batch` (beads#6): doctor check — no `bd` calls.

missing=()
for bin in jq gh; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        missing+=("$bin")
    fi
done

if [ ${#missing[@]} -eq 0 ]; then
    echo "all required binaries available (jq, gh)"
    exit 0
fi

echo "${#missing[@]} required binary(ies) missing"
for bin in "${missing[@]}"; do
    echo "$bin not found in PATH"
done
exit 2
