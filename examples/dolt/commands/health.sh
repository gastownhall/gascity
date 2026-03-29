#!/bin/sh
# gc dolt health — Lightweight Dolt data-plane health report.
#
# Checks server status and latency, per-database commit counts and open
# beads, backup freshness, orphan databases, and zombie Dolt processes.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_HOST, GC_DOLT_USER,
#              GC_DOLT_PASSWORD
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/scripts/runtime.sh"

metadata_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/metadata.json"

  if command -v gc >/dev/null 2>&1; then
    rig_paths=$(gc rig list --json --city "$GC_CITY_PATH" 2>/dev/null \
      | if command -v jq >/dev/null 2>&1; then
          jq -r '.rigs[].path' 2>/dev/null
        else
          grep '"path"' | sed 's/.*"path": *"//;s/".*//'
        fi) || true
    if [ -n "$rig_paths" ]; then
      printf '%s\n' "$rig_paths" | while IFS= read -r p; do
        [ -n "$p" ] && printf '%s\n' "$p/.beads/metadata.json"
      done
      return
    fi
  fi

  # Fallback: scan local rigs/ directory only. Cannot discover external rigs
  # when gc is unavailable — acceptable degradation.
  find "$GC_CITY_PATH/rigs" -path '*/.beads/metadata.json' 2>/dev/null || true
}

metadata_db() {
  meta="$1"
  if command -v jq >/dev/null 2>&1; then
    jq -r '.dolt_database // empty' "$meta" 2>/dev/null || true
    return
  fi
  grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta" 2>/dev/null | sed 's/.*: *"//;s/"$//' || true
}

json_output=false
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --json) json_output=true; shift ;;
    -h|--help)
      echo "Usage: gc dolt health [--json]"
      echo ""
      echo "Lightweight Dolt data-plane health report for patrol cycles."
      echo ""
      echo "Flags:"
      echo "  --json    Output as JSON (consumed by deacon patrol)"
      exit 0
      ;;
    *) echo "gc dolt health: unknown flag: $1" >&2; exit 1 ;;
  esac
done

# Determine host for probing.
host="${GC_DOLT_HOST:-127.0.0.1}"

# Check if server is running.
server_running=false
server_pid=0
server_latency=0

# Find dolt PID by port.
pid=$(lsof -ti :"$GC_DOLT_PORT" -sTCP:LISTEN 2>/dev/null | head -1 || true)
if [ -n "$pid" ]; then
  server_running=true
  server_pid="$pid"
  # Measure query latency.
  start_ms=$(date +%s%N 2>/dev/null | cut -c1-13 || date +%s)
  conn_args="--host $host --port $GC_DOLT_PORT --user $GC_DOLT_USER --no-tls"
  if [ -n "$GC_DOLT_PASSWORD" ]; then
    export DOLT_CLI_PASSWORD="$GC_DOLT_PASSWORD"
  fi
  if dolt $conn_args sql -q "SELECT 1" >/dev/null 2>&1; then
    end_ms=$(date +%s%N 2>/dev/null | cut -c1-13 || date +%s)
    server_latency=$((end_ms - start_ms))
    [ "$server_latency" -lt 0 ] && server_latency=0
  fi
fi

# Cache metadata file paths once (avoids repeated gc calls and word-splitting).
_meta_cache=$(mktemp)
metadata_files > "$_meta_cache"
trap 'rm -f "$_meta_cache"' EXIT

# Collect database info.
db_info=""
if [ -d "$data_dir" ] && [ "$server_running" = true ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$name" in information_schema|mysql|dolt_cluster) continue ;; esac
    # Count commits (best-effort).
    commits=$(cd "$d" && dolt log --oneline 2>/dev/null | wc -l || echo 0)
    commits=$(echo "$commits" | tr -d '[:space:]')
    # Count open beads (best-effort).
    open_beads=0
    while IFS= read -r meta; do
      [ -f "$meta" ] || continue
      db=$(metadata_db "$meta")
      if [ "$db" = "$name" ]; then
        beads_dir="$(dirname "$meta")"
        if [ -f "$beads_dir/beads.jsonl" ]; then
          open_beads=$(grep -c '"status":"open"' "$beads_dir/beads.jsonl" 2>/dev/null || echo 0)
        fi
        break
      fi
    done < "$_meta_cache"
    db_info="$db_info$name|$commits|$open_beads
"
  done
fi

# Check backup freshness.
backup_freshness=""
backup_stale=false
backup_age_sec=0
newest_backup=$(ls -1d "$GC_CITY_PATH"/migration-backup-* 2>/dev/null | sort -r | head -1 || true)
if [ -n "$newest_backup" ]; then
  backup_mtime=$(stat -c %Y "$newest_backup" 2>/dev/null || stat -f %m "$newest_backup" 2>/dev/null || echo 0)
  now=$(date +%s)
  backup_age_sec=$((now - backup_mtime))
  if [ "$backup_age_sec" -ge 3600 ]; then
    backup_freshness="$((backup_age_sec / 3600))h$((backup_age_sec % 3600 / 60))m"
  elif [ "$backup_age_sec" -ge 60 ]; then
    backup_freshness="$((backup_age_sec / 60))m$((backup_age_sec % 60))s"
  else
    backup_freshness="${backup_age_sec}s"
  fi
  [ "$backup_age_sec" -gt 1800 ] && backup_stale=true
fi

# Find orphan databases.
orphan_list=""
orphan_count=0
if [ -d "$data_dir" ]; then
  referenced=""
  while IFS= read -r meta; do
    [ -f "$meta" ] || continue
    db=$(metadata_db "$meta")
    [ -n "$db" ] && referenced="$referenced $db "
  done < "$_meta_cache"
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$name" in information_schema|mysql|dolt_cluster) continue ;; esac
    case "$referenced" in *" $name "*) continue ;; esac
    size_bytes=$(du -sb "$d" 2>/dev/null | cut -f1 || echo 0)
    if [ "$size_bytes" -ge 1048576 ]; then
      size=$(awk "BEGIN {printf \"%.1f MB\", $size_bytes/1048576}")
    elif [ "$size_bytes" -ge 1024 ]; then
      size=$(awk "BEGIN {printf \"%.1f KB\", $size_bytes/1024}")
    else
      size="${size_bytes} B"
    fi
    orphan_list="$orphan_list$name|$size
"
    orphan_count=$((orphan_count + 1))
  done
fi

# Check for zombie dolt processes.
zombie_count=0
zombie_pids=""
for p in $(pgrep -f "dolt sql-server" 2>/dev/null || true); do
  [ "$p" = "$server_pid" ] && continue
  zombie_count=$((zombie_count + 1))
  zombie_pids="$zombie_pids $p"
done

# Output.
timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

if [ "$json_output" = true ]; then
  # Build JSON output.
  cat <<JSONEOF
{
  "timestamp": "$timestamp",
  "server": {
    "running": $server_running,
    "pid": $server_pid,
    "port": $GC_DOLT_PORT,
    "latency_ms": $server_latency
  },
  "databases": [
JSONEOF
  first=true
  echo "$db_info" | while IFS='|' read -r name commits open_beads; do
    [ -z "$name" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    printf '    {"name": "%s", "commits": %s, "open_beads": %s}' "$name" "$commits" "$open_beads"
  done
  cat <<JSONEOF

  ],
  "backups": {
    "dolt_freshness": "$backup_freshness",
    "dolt_age_sec": $backup_age_sec,
    "dolt_stale": $backup_stale
  },
  "orphans": [
JSONEOF
  first=true
  echo "$orphan_list" | while IFS='|' read -r name size; do
    [ -z "$name" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    printf '    {"name": "%s", "size": "%s"}' "$name" "$size"
  done
  cat <<JSONEOF

  ],
  "processes": {
    "zombie_count": $zombie_count,
    "zombie_pids": [$(echo "$zombie_pids" | tr -s ' ' ',' | sed 's/^,//;s/,$//')]
  }
}
JSONEOF
  exit 0
fi

# Human-readable output.
if [ "$server_running" = true ]; then
  echo "Server: running (PID $server_pid, port $GC_DOLT_PORT, latency ${server_latency}ms)"
else
  echo "Server: not running"
fi

if [ -n "$db_info" ]; then
  echo ""
  echo "Databases:"
  echo "$db_info" | while IFS='|' read -r name commits open_beads; do
    [ -z "$name" ] && continue
    echo "  $name: $commits commits, $open_beads open beads"
  done
fi

if [ -n "$backup_freshness" ]; then
  stale=""
  [ "$backup_stale" = true ] && stale=" [STALE]"
  echo ""
  echo "Backups: ${backup_freshness} ago${stale}"
else
  echo ""
  echo "Backups: none found"
fi

if [ "$orphan_count" -gt 0 ]; then
  echo ""
  echo "Orphans: $orphan_count"
  echo "$orphan_list" | while IFS='|' read -r name size; do
    [ -z "$name" ] && continue
    echo "  $name ($size)"
  done
fi

if [ "$zombie_count" -gt 0 ]; then
  echo ""
  echo "Zombie processes: $zombie_count (PIDs:$zombie_pids)"
fi
