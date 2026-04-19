#!/bin/sh
# gastown status — show orchestration overview.
# Invoked as: gc gastown status [args...]
#
# Environment (set by gc):
#   GC_CITY_PATH   — absolute city root
#   GC_PACK_DIR    — absolute pack directory
#   GC_PACK_NAME   — pack name
#   GC_CITY_NAME   — city workspace name

set -e

echo "Gastown status for ${GC_CITY_NAME:-unknown}"
echo "City: ${GC_CITY_PATH:-unknown}"
echo ""

# Show agent sessions if gc is available.
if command -v gc >/dev/null 2>&1; then
    gc status 2>/dev/null || echo "(gc status unavailable)"
else
    echo "(gc binary not in PATH)"
fi
