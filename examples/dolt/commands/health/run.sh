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
. "$PACK_DIR/assets/scripts/runtime.sh"

metadata_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/metadata.json"

  if command -v gc >/dev/null 2>&1; then
    # Bound the gc rig list call: if gc is itself in a bad state (the
    # failure mode this patrol is meant to detect) we must not block
    # here. Degrade to the fallback rig scan below.
    rig_paths=$(run_bounded 5 gc rig list --json 2>/dev/null \
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

# Note: run_bounded / TIMEOUT_BIN are provided by assets/scripts/runtime.sh.

# Determine host for probing.
host="${GC_DOLT_HOST:-127.0.0.1}"

# Check if server is running.
server_running=false
server_pid=0
server_latency=0
server_reachable=false

# Find dolt PID by port.
pid=$(managed_runtime_listener_pid "$GC_DOLT_PORT" || true)
if [ -n "$pid" ] || managed_runtime_tcp_reachable "$GC_DOLT_PORT"; then
  server_running=true
  [ -n "$pid" ] && server_pid="$pid"
  # Measure query latency.
  start_ms=$(date +%s%N 2>/dev/null | cut -c1-13 || date +%s)
  conn_args="--host $host --port $GC_DOLT_PORT --user $GC_DOLT_USER --no-tls"
  # Always export DOLT_CLI_PASSWORD (even empty) so the client does not
  # prompt for a password on stdin. Without this, the SELECT 1 probe
  # silently fails with "Failed to parse credentials: operation not
  # supported by device" on sessions without a controlling TTY —
  # which then left the health report claiming "server: running" but
  # never reporting per-database detail.
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  # Bound the ping. A TCP-reachable but unresponsive server (stuck
  # goroutine, saturated pool, migration lock) would otherwise hang.
  if run_bounded 5 dolt $conn_args sql -q "SELECT 1" >/dev/null 2>&1; then
    server_reachable=true
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
#
# NOTE: we must NOT invoke `dolt log` against the on-disk database
# directory while the sql-server holds it open. Historically this was
# done with `cd "$d" && dolt log --oneline | wc -l`; on an active DB
# the client contends with the server for Dolt's file locks and the
# client process blocks indefinitely, orphaning zombie `dolt log`
# processes and wedging the health CLI. Query the running server via
# SQL instead — it's the authoritative source, never deadlocks with
# itself, and is cheap (dolt_log is indexed by commit hash).
db_info=""
if [ -d "$data_dir" ] && [ "$server_reachable" = true ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|__gc_probe) continue ;; esac
    # Reject names with anything outside [A-Za-z0-9_] before interpolating
    # into the SQL identifier. Dolt permits directory names that shell
    # basename happily returns (e.g. backticks, semicolons) but which
    # would break out of the identifier and execute attacker-chosen SQL
    # as the patrol user. Not an external-attack surface today — data
    # directories are server-controlled — but fragile enough under
    # config drift that it's worth skipping rather than probing.
    case "$name" in
      *[!A-Za-z0-9_]*|'') continue ;;
    esac
    # Count commits via SQL (bounded). 0 on timeout or error — keep
    # going rather than hang the whole report. Extract the first
    # fully-numeric line rather than `sed -n '2p'`: future dolt builds
    # may emit a status row for `USE` or a warning banner, in which
    # case positional parsing silently collapses the count to 0 and the
    # "empty repo" fallback masks the parse miss. Numeric-line grep
    # gives a deterministic result or clearly-failed parse.
    commits_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
      -q "USE \`$name\`; SELECT COUNT(*) FROM dolt_log;" 2>/dev/null || true)
    commits=$(printf '%s\n' "$commits_csv" | grep -E '^[0-9]+$' | head -1)
    # JSON consumers (deacon patrol) require a number; use 0 on failure.
    case "$commits" in
      ''|*[!0-9]*) commits=0 ;;
    esac
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
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|__gc_probe) continue ;; esac
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
# Use pgrep -x to match only processes named "dolt", then verify
# each is actually running sql-server via ps. This avoids false
# positives from processes that merely mention "dolt" in their args
# (e.g., Claude sessions whose prompt text contains "dolt sql-server").
#
# GC_HEALTH_SKIP_ZOMBIE_SCAN is a test-only escape hatch. Zombie
# enumeration spawns one `ps` per matching process, which on shared
# dev machines with many accumulated dolt processes dominates the
# runtime of the hang-mode test below. Setting it to "1" skips the
# scan so tests exercise just the bounded-probe behavior they care
# about without being hostage to ambient process state.
zombie_count=0
zombie_pids=""
if [ "${GC_HEALTH_SKIP_ZOMBIE_SCAN:-0}" != "1" ]; then
  for p in $(pgrep -x dolt 2>/dev/null || true); do
    [ "$p" = "$server_pid" ] && continue
    cmd=$(ps -p "$p" -o args= 2>/dev/null || true)
    case "$cmd" in
      *sql-server*) ;;
      *) continue ;;
    esac
    zombie_count=$((zombie_count + 1))
    zombie_pids="$zombie_pids $p"
  done
fi

# Output.
timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

if [ "$json_output" = true ]; then
  # Build JSON output. `server.reachable` reports whether the SQL
  # handshake actually succeeded (port listening AND server answering
  # SELECT 1). Consumers (deacon patrol) should key health off
  # `server.reachable`, not `server.running`, because a process can
  # hold the port while its goroutines are wedged.
  cat <<JSONEOF
{
  "timestamp": "$timestamp",
  "server": {
    "running": $server_running,
    "reachable": $server_reachable,
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
  # JSON mode always exits 0 when the payload is well-formed. Health
  # state is signalled in-band via `server.reachable` (and the rest of
  # the document). Automation that parses the JSON — notably the deacon
  # patrol formula — must not fail before stdout is parsed just because
  # the server is down; that's exactly the condition the patrol is
  # supposed to detect and react to. Callers that want exit-code
  # signalling should use the human-readable form.
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

# Exit status (human mode only): 0 when the data plane is healthy
# (server running AND answering SQL). Non-zero signals a CLI caller
# that something is wrong — server not running, or port in use by a
# process that isn't speaking MySQL. Stale backups, orphans, and
# zombies are informational and do not fail the exit code.
#
# JSON mode is unconditionally exit 0 (see above) — programmatic
# consumers read `server.reachable` from the payload instead.
if [ "$server_reachable" = true ]; then
  exit 0
fi
exit 1
