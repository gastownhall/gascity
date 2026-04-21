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

read_runtime_state_flag() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\(true\\|false\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

read_runtime_state_number() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([0-9][0-9]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

read_runtime_state_string() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

managed_runtime_port() (
  state_file="$1"
  expected_data_dir="$2"

  [ -f "$state_file" ] || return 0

  running=$(read_runtime_state_flag "$state_file" running)
  pid=$(read_runtime_state_number "$state_file" pid)
  port=$(read_runtime_state_number "$state_file" port)
  data_dir=$(read_runtime_state_string "$state_file" data_dir)

  [ "$running" = "true" ] || return 0
  [ -n "$pid" ] || return 0
  [ -n "$port" ] || return 0
  [ "$data_dir" = "$expected_data_dir" ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0

  if command -v lsof >/dev/null 2>&1; then
    holder_pid=$(lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -1 || true)
    [ -n "$holder_pid" ] || return 0
    [ "$holder_pid" = "$pid" ] || return 0
  else
    return 0
  fi

  printf '%s\n' "$port"
)

# Resolve GC_DOLT_PORT if not already set by the caller.
# Priority: env override > validated managed runtime state > default 3307.
if [ -z "$GC_DOLT_PORT" ]; then
  GC_DOLT_PORT=$(managed_runtime_port "$DOLT_STATE_FILE" "$GC_CITY_PATH/.beads/dolt")
  : "${GC_DOLT_PORT:=3307}"
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
