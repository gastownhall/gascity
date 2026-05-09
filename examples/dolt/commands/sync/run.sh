#!/bin/sh
# gc dolt sync — Push Dolt databases to their configured remotes.
#
# Uses the live Dolt SQL server when reachable so sync does not restart
# active databases. Falls back to CLI mode only when no server is running.
# Use --gc to purge closed ephemeral beads before syncing.
# Use --dry-run to preview without pushing.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_USER, GC_DOLT_PASSWORD
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

dry_run=false
force=false
do_gc=false
db_filter=""
beads_bd="$GC_BEADS_BD_SCRIPT"
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) dry_run=true; shift ;;
    --force)   force=true; shift ;;
    --gc)      do_gc=true; shift ;;
    --db)      db_filter="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: gc dolt sync [--dry-run] [--force] [--gc] [--db NAME]"
      echo ""
      echo "Push Dolt databases to their configured remotes."
      echo ""
      echo "Flags:"
      echo "  --dry-run   Show what would be pushed without pushing"
      echo "  --force     Force-push to remotes"
      echo "  --gc        Purge closed ephemeral beads before sync"
      echo "  --db NAME   Sync only the named database"
      exit 0
      ;;
    *) echo "gc dolt sync: unknown flag: $1" >&2; exit 1 ;;
  esac
done

case "$(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')" in
  information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe)
  echo "gc dolt sync: reserved Dolt database name: $(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//') (used internally by Dolt or gc)" >&2
  exit 1
  ;;
esac

# Check if server is running.
is_running() {
  managed_runtime_tcp_reachable "$GC_DOLT_PORT"
}

# routes_files — emit one routes.jsonl path per line.
# Uses gc rig list --json when available so external rigs are included.
# Falls back to a filesystem glob when gc is absent.
routes_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/routes.jsonl"

  if command -v gc >/dev/null 2>&1; then
    rig_paths=$(gc rig list --json 2>/dev/null \
      | if command -v jq >/dev/null 2>&1; then
          jq -r '.rigs[].path' 2>/dev/null
        else
          grep '"path"' | sed 's/.*"path": *"//;s/".*//'
        fi) || true
    if [ -n "$rig_paths" ]; then
      printf '%s\n' "$rig_paths" | while IFS= read -r p; do
        [ -n "$p" ] && printf '%s\n' "$p/.beads/routes.jsonl"
      done
      return
    fi
  fi

  # Fallback: scan local rigs/ directory only. Cannot discover external rigs
  # when gc is unavailable — acceptable degradation.
  find "$GC_CITY_PATH/rigs" -path '*/.beads/routes.jsonl' 2>/dev/null || true
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
  dolt_sql "USE \`$db\`; SELECT name, url FROM dolt_remotes LIMIT 1" \
    | awk -F, 'NR > 1 && $1 != "" {print $1 "|" $2; exit}'
}

sync_database_sql() {
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

  if [ "$dry_run" = true ]; then
    echo "  $name: would push to $remote_url"
    return 0
  fi

  dolt_sql "USE \`$name\`; CALL DOLT_ADD('-A')" >/dev/null 2>&1 || true
  dolt_sql "USE \`$name\`; CALL DOLT_COMMIT('-m', 'gc dolt sync: auto-commit working changes', '--allow-empty', '--author', 'Gas City Sync <sync@gascity.local>')" >/dev/null 2>&1 || true

  if [ "$force" = true ]; then
    push_query="USE \`$name\`; CALL DOLT_PUSH('--force', '$remote_name', 'main')"
  else
    push_query="USE \`$name\`; CALL DOLT_PUSH('$remote_name', 'main')"
  fi
  if dolt_sql "$push_query" >/dev/null 2>&1; then
    echo "  $name: pushed to $remote_url"
    return 0
  fi

  echo "  $name: ERROR: push failed" >&2
  return 1
}

sync_database_cli() {
  d="$1"
  name="$2"

  # Check for remote.
  remote=""
  if [ -f "$d/.dolt/remotes.json" ]; then
    remote=$(grep -o '"url":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"url":"//;s/"//' || true)
  fi

  if [ -z "$remote" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi

  if [ "$dry_run" = true ]; then
    echo "  $name: would push to $remote"
    return 0
  fi

  push_args="push"
  [ "$force" = true ] && push_args="push --force"

  if (cd "$d" && dolt $push_args 2>&1); then
    echo "  $name: pushed to $remote"
    return 0
  fi

  echo "  $name: ERROR: push failed" >&2
  return 1
}

# Optional GC phase: purge closed ephemerals while server is still up.
if [ "$do_gc" = true ] && [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue
    beads_dir=""
    # Find the .beads directory for this database.
    while IFS= read -r route_file; do
      [ -f "$route_file" ] || continue
      if grep -q "\"$name\"" "$route_file" 2>/dev/null; then
        beads_dir="$(dirname "$route_file")"
        break
      fi
    done <<ROUTES_LIST
$(routes_files)
ROUTES_LIST
    if [ -n "$beads_dir" ]; then
      purge_args=""
      [ "$dry_run" = true ] && purge_args="--dry-run"
      purged=$(BEADS_DIR="$beads_dir" bd purge $purge_args 2>/dev/null | grep -c "purged" || true)
      [ "$purged" -gt 0 ] && echo "Purged $purged ephemeral bead(s) from $name"
    fi
  done
fi

# Sync each database.
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
      sync_database_sql "$name" || exit_code=1
    else
      sync_database_cli "$d" "$name" || exit_code=1
    fi
  done
fi

exit $exit_code
