#!/bin/bash
# Deterministic Ralph demo check.
# Passes only when the demo output file contains the expected marker text.

set -euo pipefail

TARGET="${GC_CITY_PATH}/ralph-demo.txt"

[ -f "$TARGET" ]
grep -q "hello from ralph" "$TARGET"
