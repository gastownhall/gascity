#!/bin/bash
# Deterministic Ralph retry demo check.
# Passes only when the retry demo output contains the expected passing marker.

set -euo pipefail

TARGET="${GC_CITY_PATH}/ralph-retry-demo.txt"

[ -f "$TARGET" ]
grep -qx "pass" "$TARGET"
