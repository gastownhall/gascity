#!/bin/sh
# gc dolt pull — Pull Dolt databases from their configured remotes.
#
# Uses the live Dolt SQL server when reachable so pull does not contend with
# active databases. Falls back to CLI mode only when no server is running.
# Pulls the configured remote's `main` branch in both SQL and CLI modes.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_USER, GC_DOLT_PASSWORD,
# GC_DOLT_REMOTE_<DB> (select among multiple remotes), GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 (permit a non-file:// pull)
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

dolt_sql() {
  query="$1"
  host="${GC_DOLT_HOST:-127.0.0.1}"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  run_bounded 120 dolt --host "$host" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls \
    sql --result-format csv -q "$query"
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

  if dolt_sql "USE \`$name\`; CALL DOLT_PULL('$remote_name', 'main')" >/dev/null 2>&1; then
    echo "  $name: pulled from $remote_url"
    return 0
  fi

  echo "  $name: ERROR: pull failed" >&2
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
