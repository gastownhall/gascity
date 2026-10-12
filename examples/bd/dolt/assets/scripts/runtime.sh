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
else
  DOLT_STATE_FILE="$DOLT_STATE_DIR/dolt-state.json"
fi
DOLT_PROVIDER_STATE_FILE="$DOLT_STATE_DIR/dolt-provider-state.json"

GC_BEADS_BD_SCRIPT="$GC_CITY_PATH/.gc/scripts/gc-beads-bd.sh"

# is_local_dolt_host returns 0 (true) when the argument names the local managed
# Dolt server — loopback, the unspecified address, or an unset/empty host — and
# 1 (false) for a configured external endpoint. The health, status, and logs
# commands share it so they agree on whether GC owns a local managed process or
# is merely pointed at a remote server it cannot inspect on-disk. Mirrors the
# gc-beads-bd `is_remote` classification (gastownhall/gascity su-deol8).
is_local_dolt_host() {
  case "$1" in
    ""|127.0.0.1|0.0.0.0|localhost|::1|"[::1]") return 0 ;;
    *) return 1 ;;
  esac
}

# beads_config_value <config.yaml> <section.field> prints a two-part dotted
# bd config key's value in either spelling bd reads: the flat top-level
# `section.field: v` gc writes, or the nested `section:` / `  field: v` that
# bd >= 1.3.1 writes on `bd config set`. The flat spelling wins when both are
# present, as it does in viper. Trailing comments and surrounding quotes are
# stripped; an absent file or key prints nothing. Mirrors findConfigValue in
# internal/beads/contract/files.go and beads_config_value in the bd pack's
# gc-beads-bd.sh.
beads_config_value() {
  [ -f "$1" ] || return 0
  awk -v key="$2" '
    function clean(v) {
      sub(/^[[:space:]]+/, "", v)
      if (v ~ /^#/) v = ""
      sub(/[[:space:]]+#.*$/, "", v)
      sub(/[[:space:]]+$/, "", v)
      if (v ~ /^".*"$/ || v ~ /^\047.*\047$/) v = substr(v, 2, length(v) - 2)
      return v
    }
    BEGIN {
      dot = index(key, ".")
      section = substr(key, 1, dot - 1)
      field = substr(key, dot + 1)
    }
    { sub(/\r$/, "") }
    /^[[:space:]]*(#.*)?$/ { next }
    /^[^[:space:]]/ {
      in_section = 0
      child_indent = -1
      if (!flat_seen && index($0, key ":") == 1) {
        flat_seen = 1
        flat = clean(substr($0, length(key) + 2))
      }
      if (index($0, section ":") == 1 && clean(substr($0, length(section) + 2)) == "") in_section = 1
      next
    }
    in_section {
      match($0, /^[[:space:]]+/)
      if (child_indent < 0) child_indent = RLENGTH
      if (RLENGTH == child_indent && nested == "" && index(substr($0, RLENGTH + 1), field ":") == 1)
        nested = clean(substr($0, RLENGTH + length(field) + 2))
    }
    END {
      if (flat_seen) print flat
      else if (nested != "") print nested
    }
  ' "$1"
}

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

. "${GC_PACK_DIR:-${PACK_DIR:-${GC_SYSTEM_PACKS_DIR:-$GC_CITY_PATH/.gc/system/packs}/dolt}}/assets/scripts/port_resolve.sh"
. "${GC_PACK_DIR:-${PACK_DIR:-${GC_SYSTEM_PACKS_DIR:-$GC_CITY_PATH/.gc/system/packs}/dolt}}/assets/scripts/proxied_scope.sh"

# On a bd-owned proxied scope there is no gc-managed Dolt server: bd starts,
# supervises and stops it. Every command in this pack is a typed no-op there,
# and there is no managed runtime port to resolve. A command that shapes its
# own output (health, cleanup) sets GC_DOLT_PROXIED_HANDLED=1 before sourcing
# this file and emits its own skip document instead.
#
# The guard runs before the commands parse their own flags, so it has to
# honour --json itself: status, cleanup, compact and sync all have machine
# consumers, and a plain sentence on stdout would break every one of them.
if bd_owns_proxied_scope; then
  GC_DOLT_SCOPE_BD_PROXIED=1
  if [ "${GC_DOLT_PROXIED_HANDLED:-0}" != "1" ]; then
    case " $* " in
      *" --json "*) print_proxied_skip_json ;;
      *) printf '%s\n' "$GC_DOLT_PROXIED_NOOP_MESSAGE" ;;
    esac
    exit 0
  fi
  GC_DOLT_PORT=""
else
  GC_DOLT_SCOPE_BD_PROXIED=0
  # Resolve GC_DOLT_PORT. The shared helper prefers validated live managed
  # runtime state over stale inherited env, then falls back to GC_DOLT_PORT as
  # an operator seed, and exits 78 if neither yields a port.
  GC_DOLT_PORT=$(resolve_dolt_port_or_die "$DOLT_STATE_FILE" "$DOLT_PROVIDER_STATE_FILE" "$DOLT_DATA_DIR" "$GC_CITY_PATH") || exit $?
fi

. "${GC_PACK_DIR:-${PACK_DIR:-${GC_SYSTEM_PACKS_DIR:-$GC_CITY_PATH/.gc/system/packs}/dolt}}/assets/scripts/_bounded.sh"

# Wall-clock bound (seconds) for `gc rig list --json` rig discovery, shared
# by the compact and health commands and tunable via
# GC_DOLT_RIG_LIST_TIMEOUT_SECS. The bound must absorb a slow-but-healthy gc
# on a busy host (~16s observed): discovery callers degrade to a city-only
# filesystem scan on timeout, which silently drops external rig databases
# (gascity#2740).
GC_DOLT_RIG_LIST_TIMEOUT_SECS="${GC_DOLT_RIG_LIST_TIMEOUT_SECS:-30}"

