#!/bin/sh
# gc dolt start — Start the Dolt server if not already running.
#
# Delegates to the gc-beads-bd exec: provider's start operation.
# Operator command for explicit lifecycle recovery; the dolt-health order is
# diagnostic-only and does not restart the server automatically.
#
# Environment: GC_CITY_PATH
set -e

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

if [ ! -x "$GC_BEADS_BD_SCRIPT" ]; then
  echo "gc dolt start: gc-beads-bd not found" >&2
  exit 1
fi

# start exits 0 if started or already running, 2 if remote (no-op).
GC_CITY_PATH="$GC_CITY_PATH" "$GC_BEADS_BD_SCRIPT" start
