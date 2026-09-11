#!/bin/sh
# gc dolt status — Report whether the Dolt server is available.
#
# Prints a one-line human-readable status and exits 0 when the server is
# reachable, 1 otherwise. For a configured external Dolt endpoint (non-local
# GC_DOLT_HOST) the message names the remote endpoint rather than a managed
# local process, so operators are not told a reachable remote server is "not
# running" (su-deol8). The dolt-health order uses the structured
# `gc dolt health --json | gc dolt health-check` diagnostics.
#
# Environment: GC_CITY_PATH, GC_DOLT_HOST, GC_DOLT_PORT
set -e

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

if [ ! -x "$GC_BEADS_BD_SCRIPT" ]; then
  echo "gc dolt status: gc-beads-bd not found" >&2
  exit 1
fi

host="${GC_DOLT_HOST:-127.0.0.1}"

# probe exits 0 if the server is reachable, non-zero otherwise. Capture the
# result via `if` so `set -e` does not abort before we print status text.
if GC_CITY_PATH="$GC_CITY_PATH" "$GC_BEADS_BD_SCRIPT" probe >/dev/null 2>&1; then
  if is_local_dolt_host "$host"; then
    echo "Dolt server: running (managed, 127.0.0.1:$GC_DOLT_PORT)"
  else
    echo "Dolt server: reachable (external endpoint $host:$GC_DOLT_PORT)"
  fi
  exit 0
fi

if is_local_dolt_host "$host"; then
  echo "Dolt server: not running (managed, 127.0.0.1:$GC_DOLT_PORT)"
else
  echo "Dolt server: unreachable (external endpoint $host:$GC_DOLT_PORT)"
fi
exit 1
