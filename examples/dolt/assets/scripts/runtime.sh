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

# Data lives under .beads/dolt (gc-beads-bd canonical path). Honor
# GC_DOLT_DATA_DIR first so shell pack commands target the same managed data
# directory as the Go lifecycle and doctor code.
DOLT_BEADS_DATA_DIR="${GC_DOLT_DATA_DIR:-$GC_CITY_PATH/.beads/dolt}"
if [ -n "${GC_DOLT_DATA_DIR:-}" ]; then
  DOLT_DATA_DIR="$GC_DOLT_DATA_DIR"
elif [ -d "$DOLT_BEADS_DATA_DIR" ]; then
  DOLT_DATA_DIR="$DOLT_BEADS_DATA_DIR"
else
  DOLT_DATA_DIR="$DOLT_STATE_DIR/dolt-data"
fi

DOLT_LOG_FILE="${GC_DOLT_LOG_FILE:-$DOLT_STATE_DIR/dolt.log}"
DOLT_PID_FILE="${GC_DOLT_PID_FILE:-$DOLT_STATE_DIR/dolt.pid}"
if [ -n "${GC_DOLT_STATE_FILE:-}" ]; then
  DOLT_STATE_FILE="$GC_DOLT_STATE_FILE"
elif [ -f "$DOLT_STATE_DIR/dolt-state.json" ]; then
  DOLT_STATE_FILE="$DOLT_STATE_DIR/dolt-state.json"
elif [ -f "$DOLT_STATE_DIR/dolt-provider-state.json" ]; then
  DOLT_STATE_FILE="$DOLT_STATE_DIR/dolt-provider-state.json"
else
  DOLT_STATE_FILE="$DOLT_STATE_DIR/dolt-state.json"
fi

GC_BEADS_BD_SCRIPT="$GC_CITY_PATH/.gc/system/packs/bd/assets/scripts/gc-beads-bd.sh"

read_runtime_state_flag() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  value=$(sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([^,}[:space:]]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true)
  case "$value" in
    true|false)
      printf '%s\n' "$value"
      ;;
  esac
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

canonical_path() (
  path="$1"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import os
import sys

print(os.path.realpath(sys.argv[1]))
PY
    return $?
  fi
  if command -v readlink >/dev/null 2>&1; then
    readlink -f "$path" 2>/dev/null && return 0
  fi
  printf '%s\n' "$path"
)

same_path() (
  left="$1"
  right="$2"
  [ "$left" = "$right" ] && return 0
  [ "$(canonical_path "$left")" = "$(canonical_path "$right")" ]
)

pid_is_running() (
  pid="$1"

  case "$pid" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if kill -0 "$pid" 2>/dev/null; then
    return 0
  fi

  if command -v ps >/dev/null 2>&1; then
    ps_pid=$(ps -p "$pid" -o pid= 2>/dev/null | tr -d '[:space:]')
    [ "$ps_pid" = "$pid" ] && return 0
  fi

  return 1
)

managed_runtime_listener_pid() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 0
      ;;
  esac

  if ! command -v lsof >/dev/null 2>&1; then
    return 0
  fi

  lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null \
    | while IFS= read -r holder_pid; do
        case "$holder_pid" in
          ''|*[!0-9]*)
            continue
            ;;
        esac
        if pid_is_running "$holder_pid"; then
          printf '%s\n' "$holder_pid"
          break
        fi
      done
)

managed_runtime_tcp_reachable() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if command -v nc >/dev/null 2>&1; then
    nc -z 127.0.0.1 "$port" >/dev/null 2>&1
    return $?
  fi

  if command -v python3 >/dev/null 2>&1; then
    python3 - "$port" <<'PY' >/dev/null 2>&1
import socket
import sys

sock = socket.socket()
sock.settimeout(0.25)
try:
    sock.connect(("127.0.0.1", int(sys.argv[1])))
except OSError:
    raise SystemExit(1)
finally:
    sock.close()
PY
    return $?
  fi

  return 1
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
  if ! same_path "$data_dir" "$expected_data_dir"; then
    printf 'dolt runtime: managed state data_dir=%s does not match expected data_dir=%s\n' \
      "$data_dir" "$expected_data_dir" >&2
    return 0
  fi
  pid_is_running "$pid" || return 0

  holder_pid=$(managed_runtime_listener_pid "$port" || true)
  if [ -n "$holder_pid" ]; then
    [ "$holder_pid" = "$pid" ] || return 0
    printf '%s\n' "$port"
    return 0
  fi

  if ! managed_runtime_tcp_reachable "$port"; then
    return 0
  fi

  printf '%s\n' "$port"
)

# Resolve GC_DOLT_PORT if not already set by the caller.
# Priority: env override > validated managed runtime state > default 3307.
if [ -z "$GC_DOLT_PORT" ]; then
  GC_DOLT_PORT=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR")
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

_run_bounded_warned_no_timeout=""

# run_bounded SECS CMD...  — Run CMD with a wall-clock timeout. Exits
# 124 on timeout (coreutils convention). Uses --kill-after=2 so an
# uncooperative child that ignores SIGTERM (e.g. a dolt client stuck
# in kernel socket wait) is escalated to SIGKILL rather than leaking
# zombies — which is the failure mode the bounded helper exists to
# prevent. If no bounded execution mechanism is available, fail closed rather
# than running a potentially wedged Dolt client unbounded.
run_bounded() {
  _t="$1"; shift
  if [ -n "$TIMEOUT_BIN" ]; then
    "$TIMEOUT_BIN" --kill-after=2 "$_t" "$@"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$_t" "$@" <<'PY'
import subprocess
import sys

limit = float(sys.argv[1])
cmd = sys.argv[2:]
try:
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=limit)
except subprocess.TimeoutExpired as exc:
    sys.stdout.write(exc.stdout or "")
    sys.stderr.write(exc.stderr or "")
    sys.exit(124)
sys.stdout.write(proc.stdout)
sys.stderr.write(proc.stderr)
sys.exit(proc.returncode)
PY
  else
    printf 'dolt runtime: timeout/gtimeout/python3 not found; cannot run bounded command\n' >&2
    return 124
  fi
}
