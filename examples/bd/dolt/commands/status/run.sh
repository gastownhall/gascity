#!/bin/sh
# gc dolt status — Check if the Dolt server is running.
#
# Exits 0 if the server is reachable, 1 otherwise.
# Lightweight status probe for manual checks and scripts; the dolt-health order
# uses structured `gc dolt health --json | gc dolt health-check` diagnostics.
#
# Environment: GC_CITY_PATH
set -e

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

if [ ! -x "$GC_BEADS_BD_SCRIPT" ]; then
  echo "gc dolt status: gc-beads-bd not found" >&2
  exit 1
fi

# probe exits 0 if running, 2 if not running.
GC_CITY_PATH="$GC_CITY_PATH" "$GC_BEADS_BD_SCRIPT" probe >/dev/null 2>&1
