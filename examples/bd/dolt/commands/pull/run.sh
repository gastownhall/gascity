#!/bin/sh
# gc dolt pull — Pull Dolt databases from their configured remotes.
#
# Uses the live Dolt SQL server when reachable so pull does not contend with
# active databases. Falls back to CLI mode only when no server is running.
# Pulls the configured remote's `main` branch in both SQL and CLI modes.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_USER, GC_DOLT_PASSWORD,
# GC_DOLT_REMOTE_<DB> (select among multiple remotes), GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 (permit a non-file:// pull)
#   GC_DOLT_PULL_TIMEOUT_SECS (default: 120) — wall-clock bound for the
#   SQL-mode DOLT_PULL; increase for a slow link or a large first pull.
#
# One server-side pull per database at a time (gp-f2yq): a CALL DOLT_PULL
# runs inside the sql-server (it fetches first) and outlives a client the
# bound killed, so before issuing one the script asks the server whether a
# DOLT_PULL / DOLT_FETCH is already in flight on the server (skipped when
# one is), and when the bound expires it KILLs the server-side session the
# pull printed about itself and proves it gone from the processlist. The pull
# statement also takes the server's session lock for the database (GET_LOCK,
# timeout 0) in the same batch, so two runners that both read "nothing in
# flight" cannot both pull. See the "Server-side remote operations" helpers
# in assets/scripts/runtime.sh.
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

db_filter=""
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --db) db_filter="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: gc dolt pull [--db NAME]"
      echo ""
      echo "Pull Dolt databases from their configured remotes."
      echo ""
      echo "Flags:"
      echo "  --db NAME   Pull only the named database"
      echo ""
      echo "Environment:"
      echo "  GC_DOLT_REMOTE_<DB>                Select which remote to pull from when a database has several"
      echo "  GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1   Permit pulling from a non-file:// remote"
      echo "  GC_DOLT_PULL_TIMEOUT_SECS  SQL-mode pull bound (default 120)"
      exit 0
      ;;
    *) echo "gc dolt pull: unknown flag: $1" >&2; exit 1 ;;
  esac
done

case "$(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')" in
  information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe)
  echo "gc dolt pull: reserved Dolt database name: $(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//') (used internally by Dolt or gc)" >&2
  exit 1
  ;;
esac

# Wall-clock bound for the SQL-mode DOLT_PULL (seconds). Defaults to 120s (the
# prior fixed ceiling). Validated the way the sync bounds are: an empty /
# non-numeric / all-zero value is rejected before any database is touched —
# GNU `timeout 0` disables the timeout, i.e. an unbounded pull, the exact
# anti-hang outcome this bound exists to prevent.
pull_timeout="${GC_DOLT_PULL_TIMEOUT_SECS-120}"
case "$pull_timeout" in
  ''|*[!0-9]*) pull_timeout_valid=false ;;
  *[1-9]*)     pull_timeout_valid=true ;;
  *)           pull_timeout_valid=false ;;
esac
if [ "$pull_timeout_valid" != true ]; then
  printf 'gc dolt pull: invalid GC_DOLT_PULL_TIMEOUT_SECS=%s (must be a positive integer)\n' \
    "$pull_timeout" >&2
  exit 2
fi
# Canonical decimal: leading zeros dropped (validated non-zero, so never empty)
# so the value prints and compares as the integer it is.
pull_timeout=$(printf '%s' "$pull_timeout" | sed 's/^0*//')

is_running() {
  managed_runtime_tcp_reachable "$GC_DOLT_PORT"
}

valid_database_name() {
  case "$1" in
    [A-Za-z0-9_]*)
      case "$1" in *[!A-Za-z0-9_-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

valid_remote_name() {
  case "$1" in
    [A-Za-z0-9_.-]*)
      case "$1" in *[!A-Za-z0-9_.-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

remote_env_value() {
  key=$(printf '%s' "$1" | tr 'a-z-' 'A-Z_')
  case "$key" in *[!A-Z0-9_]*) return 0 ;; esac
  eval "printf '%s' \"\${GC_DOLT_REMOTE_$key:-}\""
}

remote_allow_env_value() {
  key=$(printf '%s' "$1" | tr 'a-z-' 'A-Z_')
  case "$key" in *[!A-Z0-9_]*) return 0 ;; esac
  eval "printf '%s' \"\${GC_DOLT_PULL_ALLOW_REMOTE_$key:-}\""
}

# Pull's own remote-selection policy: refuse an ambiguous multi-remote db
# unless GC_DOLT_REMOTE_<DB> names one explicitly, and require
# GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 before honoring any selection — override
# or sole-remote default alike — that resolves to a non-local (non-file://)
# remote. The locality check applies regardless of how many candidates there
# are, including exactly one (ga-nht26j; mirrors the equivalent sync-side
# fix, ga-2w96wd, not yet on main — consolidating the two into a shared
# helper remains a future intent, not current behavior).
select_remote() {
  sel_db="$1"; sel_candidates="$2"
  [ -z "$sel_candidates" ] && return 0
  sel_count=$(printf '%s\n' "$sel_candidates" | grep -c '.')
  if [ "$sel_count" -le 1 ]; then
    sel_solo_name=${sel_candidates%%,*}
    sel_solo_url=${sel_candidates#*,}
    case "$sel_solo_url" in
      file://*) ;;
      *)
        sel_solo_allowed=$(remote_allow_env_value "$sel_db") || return 1
        if [ "$sel_solo_allowed" != "1" ]; then
          echo "  $sel_db: ERROR: sole configured remote '$sel_solo_name' is a non-local remote ($sel_solo_url) — set GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 to allow pulling from it" >&2
          return 1
        fi ;;
    esac
    printf '%s\n' "$sel_candidates"; return 0
  fi
  sel_names=$(printf '%s\n' "$sel_candidates" | awk -F, '{ if (o=="") o=$1; else o=o","$1 } END{print o}')
  sel_override=$(remote_env_value "$sel_db") || return 1
  if [ -z "$sel_override" ]; then
    echo "  $sel_db: ERROR: multiple remotes configured ($sel_names) — set GC_DOLT_REMOTE_<DB> to disambiguate" >&2
    return 1
  fi
  if ! valid_remote_name "$sel_override"; then
    echo "  $sel_db: ERROR: invalid GC_DOLT_REMOTE override: $sel_override" >&2
    return 1
  fi
  sel_match=$(printf '%s\n' "$sel_candidates" | awk -F, -v want="$sel_override" '$1 == want {print; exit}')
  if [ -z "$sel_match" ]; then
    echo "  $sel_db: ERROR: GC_DOLT_REMOTE names unknown remote '$sel_override' (available: $sel_names)" >&2
    return 1
  fi
  sel_url=${sel_match#*,}
  case "$sel_url" in
    file://*) ;;
    *)
      sel_allowed=$(remote_allow_env_value "$sel_db") || return 1
      if [ "$sel_allowed" != "1" ]; then
        echo "  $sel_db: ERROR: GC_DOLT_REMOTE names non-local remote '$sel_override' ($sel_url) — set GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 to allow pulling from it" >&2
        return 1
      fi ;;
  esac
  printf '%s\n' "$sel_match"; return 0
}

# dolt_sql QUERY [TIMEOUT_SECS] [USE_DB] — run a SQL query against the live
# server under a wall-clock bound (dolt_sql_csv, runtime.sh); 120s by default,
# sized for metadata queries. The pull passes its own bound and its database
# (--use-db, so the server attributes the session in its processlist).
dolt_sql() {
  dolt_sql_csv "${2:-120}" "${3:-}" "$1"
}

find_remote_sql() {
  db="$1"
  remote_csv=$(dolt_sql "USE \`$db\`; SELECT name, url FROM dolt_remotes ORDER BY name") || return 1
  candidates=$(printf '%s\n' "$remote_csv" | awk -F, 'NR > 1 && $1 != "" {print $1 "," $2}')
  # 2 = select_remote already printed a specific policy refusal; 1 = lookup failed.
  chosen=$(select_remote "$db" "$candidates") || return 2
  [ -z "$chosen" ] && return 0
  printf '%s\n' "$chosen" | awk -F, '{print $1 "|" $2}'
}

pull_database_sql() {
  name="$1"
  if ! valid_database_name "$name"; then
    echo "  $name: ERROR: invalid database name" >&2
    return 1
  fi

  remote_pair=$(find_remote_sql "$name") || {
    find_rc=$?
    [ "$find_rc" -eq 2 ] || echo "  $name: ERROR: failed to query remotes" >&2
    return 1
  }
  if [ -z "$remote_pair" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  remote_name=${remote_pair%%|*}
  remote_url=${remote_pair#*|}
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  pull_err_tmp=$(mktemp) || {
    echo "  $name: ERROR: cannot create temp file for pull diagnostics" >&2
    return 1
  }
  # gp-f2yq: ONE server-side remote operation per database at a time. A CALL
  # DOLT_PULL runs inside the sql-server and outlives a client the bound
  # killed, so ask the server first and skip when a pull or fetch is already
  # in flight for this database. Fail closed: a processlist query that fails,
  # or answers with anything but a processlist, skips too.
  inflight_rc=0
  inflight=$(remote_op_sessions "$name" 120 "$pull_err_tmp") || inflight_rc=$?
  if [ "$inflight_rc" -ne 0 ]; then
    echo "  $name: ERROR: processlist query failed (exit $inflight_rc) — skipped" >&2
    remote_op_replay_stderr "$name" "$pull_err_tmp"
    rm -f "$pull_err_tmp"
    return 1
  fi
  inflight_oldest=$(printf '%s\n' "$inflight" | remote_op_sessions_oldest)
  if [ -n "$inflight_oldest" ]; then
    rm -f "$pull_err_tmp"
    echo "  $name: pull already in flight for ${inflight_oldest#* }s (session ${inflight_oldest%% *}) — skipped" >&2
    return 1
  fi
  pull_out_tmp=$(mktemp) || {
    echo "  $name: ERROR: cannot create temp file for the pull session id" >&2
    rm -f "$pull_err_tmp"
    return 1
  }
  # The statement prints its OWN connection id before the procedure starts;
  # --use-db attributes the session to this database. The id is the KILL
  # operand when the bound expires.
  pull_rc=0
  # Then the server-side gate (remote_op_gate_sql): the session takes this
  # database's lock or the batch stops here, before the CALL.
  dolt_sql "USE \`$name\`; SELECT CONNECTION_ID() AS id; $(remote_op_gate_sql "$name"); CALL DOLT_PULL('$remote_name', 'main')" "$pull_timeout" "$name" \
    >"$pull_out_tmp" 2>"$pull_err_tmp" || pull_rc=$?
  pull_session_id=$(remote_op_session_id "$pull_out_tmp")
  rm -f "$pull_out_tmp"
  if [ "$pull_rc" -eq 0 ]; then
    rm -f "$pull_err_tmp"
    echo "  $name: pulled from $remote_url"
    return 0
  fi
  # The bound's verdict outranks anything the client printed.
  if bound_expired "$pull_rc"; then
    echo "  $name: pull timed out after ${pull_timeout}s (GC_DOLT_PULL_TIMEOUT_SECS; client exit $pull_rc)" >&2
    # The client is dead (124: the bound; 137: the bound's SIGKILL escalation);
    # the server-side pull is not. End it and prove it ended (the outcome is
    # reported on its own line).
    kill_remote_op_session pull "$name" "$pull_session_id" || true
  elif remote_op_gate_refused "$pull_err_tmp"; then
    rm -f "$pull_err_tmp"
    echo "  $name: pull already in flight — the server refused a second one (session lock $(remote_op_lock_name "$name") held) — skipped" >&2
    return 1
  else
    echo "  $name: ERROR: pull failed (exit $pull_rc)" >&2
  fi
  remote_op_replay_stderr "$name" "$pull_err_tmp"
  rm -f "$pull_err_tmp"
  return 1
}

pull_database_cli() {
  d="$1"
  name="$2"

  remote_name=""
  remote_url=""
  if [ -f "$d/.dolt/remotes.json" ]; then
    candidates=$(grep -o '"name":"[^"]*","url":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null \
      | sed 's/"name":"//;s/","url":"/,/;s/"$//' \
      | sort)
    if [ -n "$candidates" ]; then
      chosen=$(select_remote "$name" "$candidates") || return 1
      remote_name=${chosen%%,*}
      remote_url=${chosen#*,}
    fi
  fi

  if [ -z "$remote_url" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  if (cd "$d" && dolt pull "$remote_name" main 2>&1); then
    echo "  $name: pulled from $remote_url"
    return 0
  fi

  echo "  $name: ERROR: pull failed" >&2
  return 1
}

exit_code=0
server_running=false
is_running && server_running=true
if [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue
    if [ -f "$d/.no-sync" ]; then
      echo "  $name: skipped (.no-sync)"
      continue
    fi

    if [ "$server_running" = true ]; then
      pull_database_sql "$name" || exit_code=1
    else
      pull_database_cli "$d" "$name" || exit_code=1
    fi
  done
fi

exit $exit_code
