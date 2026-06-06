#!/bin/sh
# gc dolt pull — Pull Dolt databases from their configured remotes.
#
# Uses the live Dolt SQL server when reachable so pull does not contend with
# active databases. Falls back to CLI mode only when no server is running.
# Pulls the configured remote's `main` branch in both SQL and CLI modes.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_USER, GC_DOLT_PASSWORD
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

dolt_sql() {
  query="$1"
  host="${GC_DOLT_HOST:-127.0.0.1}"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  run_bounded 120 dolt --host "$host" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls \
    sql --result-format csv -q "$query"
}

find_remote_sql() {
  db="$1"
  remote_csv=$(dolt_sql "USE \`$db\`; SELECT name, url FROM dolt_remotes LIMIT 1") || return 1
  printf '%s\n' "$remote_csv" | awk -F, 'NR > 1 && $1 != "" {print $1 "|" $2; exit}'
}

pull_database_sql() {
  name="$1"
  if ! valid_database_name "$name"; then
    echo "  $name: ERROR: invalid database name" >&2
    return 1
  fi

  remote_pair=$(find_remote_sql "$name") || {
    echo "  $name: ERROR: failed to query remotes" >&2
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
    remote_name=$(grep -o '"name":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"name":"//;s/"//' || true)
    remote_url=$(grep -o '"url":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"url":"//;s/"//' || true)
  fi
  [ -z "$remote_name" ] && remote_name="origin"

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
