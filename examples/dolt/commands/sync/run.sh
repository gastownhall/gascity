#!/bin/sh
# gc dolt sync — Push Dolt databases to their configured remotes.
#
# Uses the live Dolt SQL server when reachable so sync does not restart
# active databases. Falls back to CLI mode only when no server is running.
# Pushes committed branch state only; it does not auto-commit working
# changes before pushing.
# Use --gc to purge closed ephemeral beads before syncing.
# Use --dry-run to preview without pushing.
#
# Refspec resolution (per database):
#   1. GC_DOLT_REFSPEC_<DB_UPPER> env var override, in <local>:<remote> form
#      (e.g. GC_DOLT_REFSPEC_GA=main:gascity-3). DB name is uppercased with
#      '-' replaced by '_' to derive the env var key; database names that
#      differ only by '-' vs '_' intentionally share the same env var key.
#   2. Default: the database's active branch is pushed to a same-named branch
#      on the remote (i.e. <active>:<active>). This works transparently for the
#      common case where local and remote branch names match, including 'main'
#      on legacy setups.
#   3. Fallback when active_branch() cannot be resolved (or in CLI mode): 'main'.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_USER, GC_DOLT_PASSWORD
set -e

dry_run=false
force=false
do_gc=false
db_filter=""

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

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

beads_bd="$GC_BEADS_BD_SCRIPT"
data_dir="$DOLT_DATA_DIR"

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

valid_branch_name() {
  case "$1" in
    -*|.*|*..*|*@{*) return 1 ;;
    [A-Za-z0-9_.-]*)
      case "$1" in *[!A-Za-z0-9_./-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

# refspec_env_value <db> — emit the GC_DOLT_REFSPEC_<DB_UPPER> override, if any.
# DB name is uppercased and '-' is replaced with '_' to form a valid env key.
refspec_env_value() {
  db="$1"
  valid_database_name "$db" || return 1
  key=$(printf '%s' "$db" | tr 'a-z-' 'A-Z_')
  case "$key" in
    *[!A-Z0-9_]*) return 0 ;;
  esac
  eval "printf '%s' \"\${GC_DOLT_REFSPEC_$key:-}\""
}

warn_refspec_fallback() {
  printf '  %s: WARN: active branch unresolved; falling back to main\n' "$1" >&2
}

# refspec_parts <refspec> — split <local>:<remote> into two lines.
# A bare <branch> expands to <branch>:<branch>. Returns 1 if either side is
# empty or invalid.
refspec_parts() {
  rs="$1"
  case "$rs" in
    *:*)
      l=${rs%%:*}
      r=${rs#*:}
      ;;
    *)
      l="$rs"
      r="$rs"
      ;;
  esac
  [ -z "$l" ] && return 1
  [ -z "$r" ] && return 1
  valid_branch_name "$l" || return 1
  valid_branch_name "$r" || return 1
  printf '%s\n%s\n' "$l" "$r"
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

# resolve_refspec_sql <db> — emit two lines: local-branch and remote-branch.
# Honors GC_DOLT_REFSPEC_<DB> first, then falls back to active_branch() over SQL,
# then to 'main' if both fail.
resolve_refspec_sql() {
  db="$1"
  if ! valid_database_name "$db"; then
    echo "  $db: ERROR: invalid database name" >&2
    return 1
  fi
  override=$(refspec_env_value "$db") || return 1
  if [ -n "$override" ]; then
    parts=$(refspec_parts "$override") || {
      echo "  $db: ERROR: invalid refspec override: $override" >&2
      return 1
    }
    printf '%s\n' "$parts"
    return 0
  fi
  if active_csv=$(dolt_sql "USE \`$db\`; SELECT active_branch()" 2>/dev/null); then
    active=$(printf '%s\n' "$active_csv" | awk 'NR > 1 && $0 != "" {gsub(/^"|"$/, ""); print; exit}')
    if [ -n "$active" ] && valid_branch_name "$active"; then
      printf '%s\n%s\n' "$active" "$active"
      return 0
    fi
  fi
  warn_refspec_fallback "$db"
  printf 'main\nmain\n'
}

# resolve_refspec_cli <db-dir> <db-name> — same as resolve_refspec_sql, but
# resolves the active branch from repo_state.json when the SQL server is down.
repo_state_active_branch() {
  awk '
    function emit(line) {
      sub(/.*"head"[[:space:]]*:[[:space:]]*"refs\/heads\//, "", line)
      sub(/".*/, "", line)
      print line
      exit
    }
    {
      line = $0
      if (depth == 1 && line ~ /^[[:space:]]*"head"[[:space:]]*:[[:space:]]*"refs\/heads\//) {
        emit(line)
      }
      if (depth == 0 && line ~ /^[[:space:]]*\{[[:space:]]*"head"[[:space:]]*:[[:space:]]*"refs\/heads\//) {
        emit(line)
      }
      opens = gsub(/\{/, "{", line)
      closes = gsub(/\}/, "}", line)
      depth += opens - closes
      if (depth < 0) {
        depth = 0
      }
    }
  ' "$1"
}

resolve_refspec_cli() {
  d="$1"
  db="$2"
  if ! valid_database_name "$db"; then
    echo "  $db: ERROR: invalid database name" >&2
    return 1
  fi
  override=$(refspec_env_value "$db") || return 1
  if [ -n "$override" ]; then
    parts=$(refspec_parts "$override") || {
      echo "  $db: ERROR: invalid refspec override: $override" >&2
      return 1
    }
    printf '%s\n' "$parts"
    return 0
  fi
  state="$d/.dolt/repo_state.json"
  if [ -f "$state" ]; then
    head=$(repo_state_active_branch "$state" | head -1)
    if [ -n "$head" ] && valid_branch_name "$head"; then
      printf '%s\n%s\n' "$head" "$head"
      return 0
    fi
  fi
  warn_refspec_fallback "$db"
  printf 'main\nmain\n'
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

  refspec_pair=$(resolve_refspec_sql "$name") || return 1
  local_branch=$(printf '%s\n' "$refspec_pair" | sed -n '1p')
  remote_branch=$(printf '%s\n' "$refspec_pair" | sed -n '2p')

  if [ "$dry_run" = true ]; then
    echo "  $name: would push $local_branch -> $remote_name:$remote_branch ($remote_url)"
    return 0
  fi

  if [ "$local_branch" = "$remote_branch" ]; then
    refspec_arg="$local_branch"
  else
    refspec_arg="$local_branch:$remote_branch"
  fi

  if [ "$force" = true ]; then
    push_query="USE \`$name\`; CALL DOLT_PUSH('--force', '--set-upstream', '$remote_name', '$refspec_arg')"
  else
    push_query="USE \`$name\`; CALL DOLT_PUSH('$remote_name', '$refspec_arg')"
  fi
  if dolt_sql "$push_query" >/dev/null 2>&1; then
    echo "  $name: pushed $local_branch -> $remote_name:$remote_branch ($remote_url)"
    return 0
  fi

  echo "  $name: ERROR: push failed" >&2
  return 1
}

sync_database_cli() {
  d="$1"
  name="$2"

  # Check for remote.
  remote_name=""
  remote=""
  if [ -f "$d/.dolt/remotes.json" ]; then
    remote_name=$(grep -o '"name":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"name":"//;s/"//' || true)
    remote=$(grep -o '"url":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"url":"//;s/"//' || true)
  fi
  [ -z "$remote_name" ] && remote_name="origin"

  if [ -z "$remote" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  refspec_pair=$(resolve_refspec_cli "$d" "$name") || return 1
  local_branch=$(printf '%s\n' "$refspec_pair" | sed -n '1p')
  remote_branch=$(printf '%s\n' "$refspec_pair" | sed -n '2p')

  if [ "$dry_run" = true ]; then
    echo "  $name: would push $local_branch -> $remote_name:$remote_branch ($remote)"
    return 0
  fi

  if [ "$local_branch" = "$remote_branch" ]; then
    refspec_arg="$local_branch"
  else
    refspec_arg="$local_branch:$remote_branch"
  fi

  if [ "$force" = true ]; then
    if (cd "$d" && dolt push --force --set-upstream "$remote_name" "$refspec_arg" 2>&1); then
      echo "  $name: pushed $local_branch -> $remote_name:$remote_branch ($remote)"
      return 0
    fi
  elif (cd "$d" && dolt push "$remote_name" "$refspec_arg" 2>&1); then
    echo "  $name: pushed $local_branch -> $remote_name:$remote_branch ($remote)"
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
