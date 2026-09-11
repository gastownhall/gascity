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

# Resolve GC_DOLT_PORT. The shared helper prefers validated live managed
# runtime state over stale inherited env, then falls back to GC_DOLT_PORT as an
# operator seed, and exits 78 if neither yields a port.
. "${GC_PACK_DIR:-${PACK_DIR:-${GC_SYSTEM_PACKS_DIR:-$GC_CITY_PATH/.gc/system/packs}/dolt}}/assets/scripts/port_resolve.sh"
GC_DOLT_PORT=$(resolve_dolt_port_or_die "$DOLT_STATE_FILE" "$DOLT_PROVIDER_STATE_FILE" "$DOLT_DATA_DIR" "$GC_CITY_PATH") || exit $?

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

# Wall-clock bound (seconds) for `gc rig list --json` rig discovery, shared
# by the compact and health commands and tunable via
# GC_DOLT_RIG_LIST_TIMEOUT_SECS. The bound must absorb a slow-but-healthy gc
# on a busy host (~16s observed): discovery callers degrade to a city-only
# filesystem scan on timeout, which silently drops external rig databases
# (gascity#2740).
GC_DOLT_RIG_LIST_TIMEOUT_SECS="${GC_DOLT_RIG_LIST_TIMEOUT_SECS:-30}"

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

proc = subprocess.Popen(cmd)
try:
    proc.wait(timeout=limit)
except subprocess.TimeoutExpired:
    proc.terminate()
    try:
        proc.wait(timeout=2)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
    sys.exit(124)
sys.exit(proc.returncode)
PY
  else
    printf 'dolt runtime: timeout/gtimeout/python3 not found; cannot run bounded command\n' >&2
    return 124
  fi
}

# --- Server-side remote operations (gp-f2yq) ---------------------------------
#
# A `CALL DOLT_FETCH` / `CALL DOLT_PULL` issued through the dolt CLI runs
# INSIDE the sql-server. When the client's wall-clock bound expires only the
# client dies; the server-side procedure keeps running, and a patrol that
# re-issues the call every cooldown stacks one more on top each run (boomtown
# 2026-09-11: 169 of 178 sql-server connections in DOLT_FETCH, one per
# 15-minute run for two days, dolt at 180% CPU, that store's sync dead for
# weeks). sync, pull and health share the helpers below so all three agree on
# what "a remote operation is in flight" means and how one is ended.
#
# Contract (verified on Dolt 2.1.10):
#   - information_schema.processlist lists every server-side session with
#     Id, Time (seconds the current statement has run), DB and Info (the
#     statement text). A session whose client died stays listed.
#   - The DB column is the connection's database as set by the CLI's
#     --use-db; a `USE` statement inside the query does NOT set it.
#   - The CLI prints each statement's result before running the next, so a
#     `SELECT CONNECTION_ID()` issued just before the procedure lands on
#     stdout before the fetch starts and survives the client's death.
#   - `KILL <id>` exits 0 with no text whether or not <id> exists, so the
#     proof that a session ended is the processlist read AFTER the KILL.

# dolt_sql_csv TIMEOUT_SECS USE_DB QUERY — run QUERY against the managed
# server through the dolt CLI under a wall-clock bound, CSV result. USE_DB,
# when non-empty, is passed as --use-db so the server attributes the session
# to that database (the processlist DB column). The password reaches dolt via
# DOLT_CLI_PASSWORD, never as an argv flag.
dolt_sql_csv() {
  _dsc_tmo="$1"
  _dsc_db="$2"
  _dsc_q="$3"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  if [ -n "$_dsc_db" ]; then
    run_bounded "$_dsc_tmo" dolt --host "${GC_DOLT_HOST:-127.0.0.1}" --port "$GC_DOLT_PORT" \
      --user "${GC_DOLT_USER:-root}" --no-tls --use-db "$_dsc_db" \
      sql --result-format csv -q "$_dsc_q"
  else
    run_bounded "$_dsc_tmo" dolt --host "${GC_DOLT_HOST:-127.0.0.1}" --port "$GC_DOLT_PORT" \
      --user "${GC_DOLT_USER:-root}" --no-tls \
      sql --result-format csv -q "$_dsc_q"
  fi
}

# remote_op_sessions_sql [DB] — the SQL that lists the live sessions running
# CALL DOLT_FETCH or CALL DOLT_PULL as `Id,Time,db` rows. With DB (already
# validated by the caller before it is interpolated): the sessions attributed
# to that database PLUS the sessions attributed to no database at all — an
# unattributed fetch (issued without --use-db: an older gc dolt sync, or an
# operator) may be this database's, so it counts, fail closed. Without DB:
# every such session on the server (the health probe).
# REMOTE_OP_INFO_REGEXP — the SQL REGEXP (ICU, applied to UPPER(Info)) that
# recognizes a running CALL DOLT_FETCH / CALL DOLT_PULL however it was typed:
# leading whitespace, block or line comments, any whitespace between CALL and
# the procedure name and before the paren. DOLT_PUSH, DOLT_FETCHX and the text
# inside a string literal do not match. Backslashes are doubled for the SQL
# string literal. Verified on Dolt 2.1.10 (evidence 04b: 7 variants hit, 5
# decoys miss).
REMOTE_OP_INFO_REGEXP='^\\s*((/\\*([^*]|\\*[^/])*\\*/|--[^\\n]*\\n)\\s*)*CALL\\s+DOLT_(FETCH|PULL)\\s*\\('

remote_op_sessions_sql() {
  _ros_q="SELECT Id, Time, COALESCE(db, '') AS db FROM information_schema.processlist WHERE UPPER(Info) REGEXP '$REMOTE_OP_INFO_REGEXP'"
  if [ -n "${1:-}" ]; then
    _ros_q="$_ros_q AND (db = '$1' OR db = '' OR db IS NULL)"
  fi
  printf '%s ORDER BY Time DESC, Id ASC' "$_ros_q"
}

# remote_op_sessions_parse — stdin: the CSV answer to remote_op_sessions_sql;
# stdout: one `Id Time` line per session. Returns 1 when the first line is not
# the `Id,Time,db` header (an empty stdout, a banner or an error text is NOT a
# processlist answer), 2 when a non-blank row does not carry an all-digit Id
# and Time (a NULL, a truncated line, a wrapper's noise). Neither may ever be
# read as "nothing in flight": a malformed row is a session whose state is
# unknown, so the whole answer is refused, fail closed.
remote_op_sessions_parse() {
  awk -F, '
    NR == 1 {
      hdr = $0
      gsub(/"|\r/, "", hdr)
      if (tolower(hdr) != "id,time,db") exit 1
      next
    }
    /^[[:space:]]*$/ { next }
    {
      gsub(/"|\r/, "", $1)
      gsub(/"|\r/, "", $2)
      if ($1 !~ /^[0-9]+$/ || $2 !~ /^[0-9]+$/) { bad = 1; exit 2 }
      print $1, $2
    }
    END { if (NR == 0) exit 1; if (bad) exit 2 }
  '
}

# remote_op_sessions DB TIMEOUT_SECS STDERR_FILE — list the in-flight remote
# operations (remote_op_sessions_sql DB; DB may be empty for server-wide) as
# `Id Time` lines on stdout. Returns 0 on a processlist answer (possibly with
# no rows); otherwise the query's exit code (124 = the bound expired), or 1
# when the answer was not a processlist, with the reason appended to
# STDERR_FILE for the caller to replay. Fail closed on every non-zero return.
remote_op_sessions() {
  _rs_db="$1"
  _rs_tmo="$2"
  _rs_errf="$3"
  _rs_rc=0
  _rs_csv=$(dolt_sql_csv "$_rs_tmo" "" "$(remote_op_sessions_sql "$_rs_db")" 2>>"$_rs_errf") || _rs_rc=$?
  [ "$_rs_rc" -eq 0 ] || return "$_rs_rc"
  _rs_rows=$(printf '%s\n' "$_rs_csv" | remote_op_sessions_parse) || {
    _rs_prc=$?
    if [ "$_rs_prc" -eq 2 ]; then
      printf 'malformed processlist row (Id or Time not all-digit) — refusing the whole answer\n' >>"$_rs_errf"
    else
      printf 'not a processlist answer (no Id,Time,db header)\n' >>"$_rs_errf"
    fi
    return 1
  }
  printf '%s\n' "$_rs_rows"
}

# remote_op_sessions_oldest — stdin: `Id Time` lines; stdout: the line with
# the largest Time (the oldest session); nothing on empty input.
remote_op_sessions_oldest() {
  awk 'NF == 2 && ($2 + 0) >= (m + 0) { m = $2; l = $0 } END { if (l != "") print l }'
}

# remote_op_session_id FILE — the connection id that a `SELECT CONNECTION_ID()
# AS id` printed into FILE (the all-digit line right after the `id` header);
# nothing when the client died before the server answered.
remote_op_session_id() {
  [ -f "$1" ] || return 0
  awk '{ gsub(/\r/, "") } prev == "id" && $0 ~ /^[0-9]+$/ { print; exit } { prev = $0 }' "$1"
}

# kill_remote_op_session LABEL DB SESSION_ID [TIMEOUT_SECS] — end the
# server-side DOLT_FETCH / DOLT_PULL this client abandoned when its bound
# expired, and PROVE it ended. SESSION_ID is the connection id the statement
# printed about itself before the procedure started: the session is ours by
# construction. An empty SESSION_ID (the client died before the server
# answered) falls back to every in-flight remote operation attributed to DB:
# the single-flight check found none before ours started, so each one is ours
# or a concurrent runner's that raced the same window, and none may survive.
# The verdict is the processlist read AFTER the KILL, never KILL's own exit
# code: a session still listed is reported as NOT killed and returns 1. LABEL
# names the operation in the lines ("fetch" / "pull"); every line goes to
# stderr next to the timeout line it resolves.
kill_remote_op_session() {
  _kr_label="$1"
  _kr_db="$2"
  _kr_id="$3"
  _kr_tmo="${4:-120}"
  case "$_kr_id" in
    ''|*[!0-9]*) _kr_ids="" ;;
    *) _kr_ids="$_kr_id" ;;
  esac
  _kr_errf=$(mktemp) || {
    echo "  $_kr_db: server-side $_kr_label NOT killed: cannot create temp file for processlist diagnostics" >&2
    return 1
  }
  if [ -z "$_kr_ids" ]; then
    _kr_rows=$(remote_op_sessions "$_kr_db" "$_kr_tmo" "$_kr_errf") || {
      echo "  $_kr_db: server-side $_kr_label NOT killed: session id unknown and the processlist query failed" >&2
      remote_op_replay_stderr "$_kr_db" "$_kr_errf"
      rm -f "$_kr_errf"
      return 1
    }
    _kr_ids=$(printf '%s\n' "$_kr_rows" | awk '{ print $1 }')
    if [ -z "$_kr_ids" ]; then
      rm -f "$_kr_errf"
      echo "  $_kr_db: server-side $_kr_label already ended (nothing in flight to kill)" >&2
      return 0
    fi
  fi
  for _kr_one in $_kr_ids; do
    _kr_krc=0
    _kr_kerr=$(dolt_sql_csv "$_kr_tmo" "" "KILL $_kr_one" 2>&1 >/dev/null) || _kr_krc=$?
    [ "$_kr_krc" -eq 0 ] || echo "  $_kr_db: KILL $_kr_one failed (exit $_kr_krc): $_kr_kerr" >&2
  done
  _kr_left=$(remote_op_sessions "$_kr_db" "$_kr_tmo" "$_kr_errf") || {
    echo "  $_kr_db: server-side $_kr_label kill NOT confirmed: the processlist query failed after KILL" >&2
    remote_op_replay_stderr "$_kr_db" "$_kr_errf"
    rm -f "$_kr_errf"
    return 1
  }
  rm -f "$_kr_errf"
  _kr_left_ids=" $(printf '%s\n' "$_kr_left" | awk '{ printf "%s ", $1 }')"
  _kr_rc=0
  for _kr_one in $_kr_ids; do
    case "$_kr_left_ids" in
      *" $_kr_one "*)
        echo "  $_kr_db: server-side $_kr_label NOT killed (session $_kr_one still in flight after KILL)" >&2
        _kr_rc=1
        ;;
      *)
        echo "  $_kr_db: server-side $_kr_label killed (session $_kr_one no longer in flight)" >&2
        ;;
    esac
  done
  return "$_kr_rc"
}

# remote_op_replay_stderr DB FILE — replay a captured dolt stderr, one line
# per line prefixed with the db name (scannable multi-db output); a final line
# without a trailing newline is flushed too. Nothing on an empty capture.
remote_op_replay_stderr() {
  [ -s "$2" ] || return 0
  while IFS= read -r _rr_line || [ -n "$_rr_line" ]; do
    printf '  %s: %s\n' "$1" "$_rr_line" >&2
  done < "$2"
}

# bound_expired RC — true when a run_bounded exit code means the wall-clock
# bound expired: 124 (GNU timeout, or the python fallback) or 137 (GNU timeout
# escalated to SIGKILL after --kill-after because the client ignored TERM).
# Either way the client is dead and the server-side call is not: both are the
# cleanup case, never the ordinary-error case.
bound_expired() {
  case "$1" in
    124|137) return 0 ;;
    *) return 1 ;;
  esac
}

# --- Per-database runner lock (gp-f2yq) ---------------------------------------
#
# The processlist check and the CALL are two round trips. Two runners on this
# host (the patrol and an operator's `gc dolt sync`, or sync and pull) can both
# read "nothing in flight" and both issue a fetch. remote_op_lock_* serialize
# runners per server + database with a mkdir lock (atomic on every POSIX
# filesystem; no flock dependency), held from before the check until the
# operation and its cleanup are done. A holder that died leaves a lock whose
# pid is gone; the next runner reclaims it. Root: GC_DOLT_REMOTE_OP_LOCK_ROOT
# (default ${TMPDIR:-/tmp}/gc-dolt-remote-op), one `<host>-<port>-<db>.lock`
# directory per database, `pid` inside.

remote_op_lock_root() {
  printf '%s' "${GC_DOLT_REMOTE_OP_LOCK_ROOT:-${TMPDIR:-/tmp}/gc-dolt-remote-op}"
}

remote_op_lock_dir() {
  _rl_host=$(printf '%s' "${GC_DOLT_HOST:-127.0.0.1}" | tr '[:upper:]' '[:lower:]')
  _rl_key=$(printf '%s-%s-%s' "$_rl_host" "${GC_DOLT_PORT:-}" "$1" | tr -c 'A-Za-z0-9_.-' '-')
  printf '%s/%s.lock' "$(remote_op_lock_root)" "$_rl_key"
}

# remote_op_lock_acquire DB — take DB's runner lock. Returns 0 holding it (path
# in REMOTE_OP_LOCK_HELD); 1 when a live runner holds it (its pid, or
# "unknown", in REMOTE_OP_LOCK_HOLDER); 2 when the lock root cannot be created
# or the pid cannot be recorded. A lock without a pid file is given one second
# (the holder is between its mkdir and its pid write) and then treated as
# stale; a stale lock is reclaimed once.
# shellcheck disable=SC2034  # REMOTE_OP_LOCK_HOLDER is read by the sync/pull callers
remote_op_lock_acquire() {
  _la_dir=$(remote_op_lock_dir "$1")
  _la_root=$(remote_op_lock_root)
  if [ ! -d "$_la_root" ]; then
    _la_umask=$(umask)
    umask 077
    mkdir -p "$_la_root" 2>/dev/null || { umask "$_la_umask"; return 2; }
    umask "$_la_umask"
  fi
  REMOTE_OP_LOCK_HOLDER=""
  _la_waited=0
  _la_reclaimed=0
  while :; do
    if mkdir "$_la_dir" 2>/dev/null; then
      if ! printf '%s\n' "$$" > "$_la_dir/pid" 2>/dev/null; then
        rmdir "$_la_dir" 2>/dev/null
        return 2
      fi
      REMOTE_OP_LOCK_HELD="$_la_dir"
      return 0
    fi
    _la_pid=$(cat "$_la_dir/pid" 2>/dev/null || true)
    case "$_la_pid" in
      ''|*[!0-9]*)
        if [ "$_la_waited" -eq 0 ]; then
          _la_waited=1
          sleep 1
          continue
        fi
        ;;
      *)
        if kill -0 "$_la_pid" 2>/dev/null; then
          REMOTE_OP_LOCK_HOLDER="$_la_pid"
          return 1
        fi
        ;;
    esac
    if [ "$_la_reclaimed" -ne 0 ]; then
      REMOTE_OP_LOCK_HOLDER="${_la_pid:-unknown}"
      return 1
    fi
    _la_reclaimed=1
    rm -f "$_la_dir/pid" 2>/dev/null
    rmdir "$_la_dir" 2>/dev/null
  done
}

# remote_op_lock_release — drop the lock remote_op_lock_acquire took (no-op
# when none is held).
remote_op_lock_release() {
  [ -n "${REMOTE_OP_LOCK_HELD:-}" ] || return 0
  rm -f "$REMOTE_OP_LOCK_HELD/pid" 2>/dev/null
  rmdir "$REMOTE_OP_LOCK_HELD" 2>/dev/null
  REMOTE_OP_LOCK_HELD=""
}
