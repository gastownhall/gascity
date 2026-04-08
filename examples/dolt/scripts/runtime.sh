#!/bin/sh

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"

CITY_RUNTIME_DIR="${GC_CITY_RUNTIME_DIR:-$GC_CITY_PATH/.gc/runtime}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-$CITY_RUNTIME_DIR/packs/dolt}"
LEGACY_GC_DIR="$GC_CITY_PATH/.gc"

if [ -d "$PACK_STATE_DIR" ] || [ ! -d "$LEGACY_GC_DIR/dolt-data" ]; then
  DOLT_STATE_DIR="$PACK_STATE_DIR"
else
  DOLT_STATE_DIR="$LEGACY_GC_DIR"
fi

# Data lives under .beads/dolt (gc-beads-bd canonical path).
# Fall back to $DOLT_STATE_DIR/dolt-data for legacy cities that haven't migrated.
DOLT_BEADS_DATA_DIR="$GC_CITY_PATH/.beads/dolt"
if [ -d "$DOLT_BEADS_DATA_DIR" ]; then
  DOLT_DATA_DIR="$DOLT_BEADS_DATA_DIR"
else
  DOLT_DATA_DIR="$DOLT_STATE_DIR/dolt-data"
fi

DOLT_LOG_FILE="$DOLT_STATE_DIR/dolt.log"
DOLT_PID_FILE="$DOLT_STATE_DIR/dolt.pid"
DOLT_STATE_FILE="$DOLT_STATE_DIR/dolt-state.json"

GC_BEADS_BD_SCRIPT="$GC_CITY_PATH/.gc/system/bin/gc-beads-bd"

# Resolve dolt port via the shared helper.
# Priority: port file > state file > GC_DOLT_PORT env > error.
# Always re-resolve — the env var may be stale after a dolt restart.
_resolve_port_script="$(cd "$(dirname "$0")" && pwd)/resolve-port.sh"
if [ -f "$_resolve_port_script" ]; then
  . "$_resolve_port_script"
  GC_DOLT_PORT=$(resolve_dolt_port) || { echo "runtime.sh: failed to resolve dolt port" >&2; exit 1; }
else
  echo "runtime.sh: resolve-port.sh not found at $_resolve_port_script" >&2
  exit 1
fi
