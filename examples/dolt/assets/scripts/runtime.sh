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

city_toml_dolt_port() {
  _city_toml="$GC_CITY_PATH/city.toml"
  [ -f "$_city_toml" ] || return 0
  awk '
    /^\[dolt\][[:space:]]*$/ { in_dolt=1; next }
    /^\[[^]]+\][[:space:]]*$/ { in_dolt=0 }
    in_dolt && /^[[:space:]]*port[[:space:]]*=/ {
      sub(/^[^=]*=[[:space:]]*/, "", $0)
      gsub(/[[:space:]"]/, "", $0)
      print
      exit
    }
  ' "$_city_toml"
}

# Resolve GC_DOLT_PORT if not already set by the caller.
# Priority: city.toml [dolt].port > env fallback > port file > state file > default 3307.
if [ -z "$GC_DOLT_PORT" ]; then
  GC_DOLT_PORT=$(city_toml_dolt_port)
  if [ -z "$GC_DOLT_PORT" ]; then
    _port_file="$GC_CITY_PATH/.beads/dolt-server.port"
    if [ -f "$_port_file" ]; then
      GC_DOLT_PORT=$(cat "$_port_file" 2>/dev/null)
    fi
  fi
  if [ -z "$GC_DOLT_PORT" ] && [ -f "$DOLT_STATE_FILE" ]; then
    GC_DOLT_PORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DOLT_STATE_FILE" | head -1)
  fi
  : "${GC_DOLT_PORT:=3307}"
fi

_city_port=$(city_toml_dolt_port)
if [ -n "$_city_port" ]; then
  GC_DOLT_PORT="$_city_port"
fi
# Resolve a bounded-execution helper. Prefer gtimeout (coreutils on
# macOS), fall back to timeout (coreutils on Linux), then to running
# the command directly if neither is installed. Running unbounded is
# still better than letting a wedged dolt client hang the caller, but
# patrol callers need a hard upper bound wherever possible.
if command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT_BIN="gtimeout"
elif command -v timeout >/dev/null 2>&1; then
  TIMEOUT_BIN="timeout"
else
  TIMEOUT_BIN=""
fi

# run_bounded SECS CMD...  — Run CMD with a wall-clock timeout. Exits
# 124 on timeout (coreutils convention). Uses --kill-after=2 so an
# uncooperative child that ignores SIGTERM (e.g. a dolt client stuck
# in kernel socket wait) is escalated to SIGKILL rather than leaking
# zombies — which is the failure mode the bounded helper exists to
# prevent. When no timeout binary is available the command runs
# unbounded; callers must still tolerate a non-zero status.
run_bounded() {
  _t="$1"; shift
  if [ -n "$TIMEOUT_BIN" ]; then
    "$TIMEOUT_BIN" --kill-after=2 "$_t" "$@"
  else
    "$@"
  fi
}
