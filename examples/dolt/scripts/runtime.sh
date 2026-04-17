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

GC_BEADS_BD_SCRIPT="$GC_CITY_PATH/.gc/system/packs/bd/assets/scripts/gc-beads-bd.sh"

# Resolve GC_DOLT_PORT if not already set by the caller.
# Priority: env override > port file > state file > default 3307.
if [ -z "$GC_DOLT_PORT" ]; then
  _port_file="$GC_CITY_PATH/.beads/dolt-server.port"
  if [ -f "$_port_file" ]; then
    GC_DOLT_PORT=$(cat "$_port_file" 2>/dev/null)
  fi
  if [ -z "$GC_DOLT_PORT" ] && [ -f "$DOLT_STATE_FILE" ]; then
    GC_DOLT_PORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DOLT_STATE_FILE" | head -1)
  fi
  : "${GC_DOLT_PORT:=3307}"
fi
