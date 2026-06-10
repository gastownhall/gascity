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
    # here. Degrade to the fallback rig scan below. The bound must absorb
    # a slow-but-healthy gc on a busy host (~16s observed) because the
    # fallback scan only sees the city directory and silently drops
    # external rig databases (gascity#2740).
    rig_paths=$(run_bounded 30 gc rig list --json 2>/dev/null \
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
      echo "  --json    Output as JSON (consumed by health patrol automation)"
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

# Portable millisecond timestamp. BSD date(1) on macOS treats %N as a
# literal 'N' (exits 0, output like "1776740122N"), so the GNU-only
# || fallback never triggers. Feature-test the output instead.
now_ms() {
  _raw=$(date +%s%N 2>/dev/null)
  case "$_raw" in
    ''|*[!0-9]*) printf '%s000' "$(date +%s 2>/dev/null)" ;;
    *)        printf '%s' "$_raw" | cut -c1-13 ;;
  esac
}

# Find dolt PID by port.
pid=$(managed_runtime_listener_pid "$GC_DOLT_PORT" || true)
if [ -n "$pid" ] || managed_runtime_tcp_reachable "$GC_DOLT_PORT"; then
  server_running=true
  [ -n "$pid" ] && server_pid="$pid"
  # Measure query latency.
  start_ms=$(now_ms)
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
    end_ms=$(now_ms)
    server_latency=$((end_ms - start_ms))
    [ "$server_latency" -lt 0 ] && server_latency=0
  fi
fi

# Cache metadata file paths once (avoids repeated gc calls and word-splitting).
_meta_cache=$(mktemp)
# Scratch file for the zombie scan's matched-server filter. The foreign-managed
# decision runs in a `... | while read` subshell (so $zombie_count can't be
# mutated through the pipe); the survivors are spooled here and read back in
# the parent shell.
_zombie_scan_out=$(mktemp)
metadata_files > "$_meta_cache"
trap 'rm -f "$_meta_cache" "$_zombie_scan_out"' EXIT

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
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    # Reject names with anything outside [A-Za-z0-9_-] before interpolating
    # into the SQL identifier. The first byte must still be alnum/underscore
    # to avoid option-shaped names. Dolt permits directory names that shell
    # basename happily returns (e.g. backticks, semicolons) but which
    # would break out of the identifier and execute attacker-chosen SQL
    # as the patrol user. Not an external-attack surface today — data
    # directories are server-controlled — but fragile enough under
    # config drift that it's worth skipping rather than probing.
    case "$name" in
      [A-Za-z0-9_]*)
        case "$name" in *[!A-Za-z0-9_-]*) continue ;; esac
        ;;
      *) continue ;;
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
    # JSON consumers require a number; use 0 on failure.
    case "$commits" in
      ''|*[!0-9]*) commits=0 ;;
    esac
    # Count open beads from the running server (authoritative). Under managed
    # Dolt the beads live in the server's `issues` table, not an on-disk
    # beads.jsonl — that file is absent or stale, so the old file grep reported
    # open_beads=0 for every live database (#3200). 0 on timeout, error, or a
    # database without an `issues` table (a non-beads DB) — same fail-soft
    # contract as the commit count above.
    open_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
      -q "USE \`$name\`; SELECT COUNT(*) FROM issues WHERE status='open';" 2>/dev/null || true)
    open_beads=$(printf '%s\n' "$open_csv" | grep -E '^[0-9]+$' | head -1)
    case "$open_beads" in
      ''|*[!0-9]*) open_beads=0 ;;
    esac
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
#
# Authoritative source: `gc dolt-cleanup` (HYPHEN — the Go-side command,
# dry-run by default, rig-protected). Its dry-run drop candidates
# (`dropped.names`) are the real orphans: every registered rig DB is excluded
# via city config, so a live rig DB is never listed. The previous
# metadata-only scan flagged every live rig DB as an orphan whenever a rig's
# metadata.json was sparse or unreachable (e.g. externally-pathed rigs) — a
# false positive automation could act on destructively (#3200). Reuse
# the cleanup authority; fall back to the metadata scan only when gc/jq are
# unavailable (gc itself may be the failure this patrol is detecting).
orphan_list=""
orphan_count=0
if [ -d "$data_dir" ]; then
  orphan_names=""
  cleanup_ok=false
  if command -v gc >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
    cleanup_json=$(run_bounded 10 gc dolt-cleanup --json 2>/dev/null) || true
    if [ -n "$cleanup_json" ] && printf '%s' "$cleanup_json" | jq -e '.dropped.names' >/dev/null 2>&1; then
      orphan_names=$(printf '%s' "$cleanup_json" | jq -r '.dropped.names[]? // empty' 2>/dev/null)
      cleanup_ok=true
    fi
  fi

  if [ "$cleanup_ok" != true ]; then
    # Fallback: approximate orphans from rig metadata (every DB whose name is
    # not referenced by a rig's metadata.json dolt_database). Less reliable
    # than the cleanup authority — used only when gc/jq are unavailable.
    referenced=""
    while IFS= read -r meta; do
      [ -f "$meta" ] || continue
      db=$(metadata_db "$meta")
      [ -n "$db" ] && referenced="$referenced $db "
    done < "$_meta_cache"
    for d in "$data_dir"/*/; do
      [ ! -d "$d/.dolt" ] && continue
      name="$(basename "$d")"
      case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
      case "$referenced" in *" $name "*) continue ;; esac
      orphan_names="$orphan_names$name
"
    done
  fi

  # Materialize the orphan list with on-disk sizes, from whichever source
  # produced the names. Only names that still exist as a Dolt database
  # directory are reported.
  for name in $orphan_names; do
    [ -n "$name" ] || continue
    d="$data_dir/$name"
    [ -d "$d/.dolt" ] || continue
    size_kb=$(du -sk "$d" 2>/dev/null | cut -f1)
    size_bytes=$(( ${size_kb:-0} * 1024 ))
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
# Rig-local Dolt servers (configured via dolt.port in config.yaml)
# are legitimate — exclude any PID listening on a known rig port.
#
# Foreign Dolt servers (managed by OTHER cities on the same host) are
# also legitimate. gc ALWAYS writes a dolt.pid next to a managed dolt
# config, so the sibling dolt.pid — located by parsing `--config <path>`
# from the process command line — is the authoritative ownership signal:
# present and self-referential means a healthy gc-managed instance.
# Externally-managed Dolt servers (launchd- or manually-started servers
# for unrelated apps, on their own datadir and port) also carry an
# explicit `--config` but have NO sibling dolt.pid; they are not town
# strays and must not be flagged, or health patrol automation could kill a
# healthy, unrelated server. Without these exclusions, every patrol in
# every city flags the others (and unrelated apps) as zombies on shared
# dev hosts. The `--config` parse happens inside the single bounded
# `ps -eo` + awk pass below (it already has the full args line in hand);
# only the sibling dolt.pid read is left to the shell loop, which
# iterates O(matched sql-servers) — never O(all pids/zombies) — so the
# bounded-fork invariant still holds.
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
  # Collect PIDs of legitimate rig-local Dolt servers.
  rig_dolt_pids=""
  while IFS= read -r meta; do
    [ -f "$meta" ] || continue
    config_file="$(dirname "$meta")/config.yaml"
    [ -f "$config_file" ] || continue
    rig_port=$(grep '^dolt\.port:' "$config_file" 2>/dev/null | sed "s/^dolt\\.port:[[:space:]]*//; s/[[:space:]]*#.*$//; s/['\\\"]//g; s/[[:space:]]*$//" | head -1)
    case "$rig_port" in ''|*[!0-9]*) continue ;; esac
    [ "$rig_port" = "$GC_DOLT_PORT" ] && continue
    rig_pid=$(managed_runtime_listener_pid "$rig_port" || true)
    [ -n "$rig_pid" ] && rig_dolt_pids="$rig_dolt_pids $rig_pid "
  done < "$_meta_cache"

  # Enumerate the process table ONCE, not one `ps -p <pid> -o args=` fork per
  # `pgrep -x dolt` match. pgrep matches every dolt-named process including
  # Z-state zombies, so under a non-reaping PID 1 the old per-PID fork became
  # an O(zombies) `ps` storm re-paid on every 30s health tick (#2482). Collect
  # the candidate PIDs from pgrep, then classify them in a single `ps`+`awk`
  # pass: keep candidates that are dolt sql-server processes, skip Z-state
  # zombies (a defunct dolt never carries sql-server args anyway), and exclude
  # the managed city server and rig-local dolts. For each survivor the awk
  # pass also extracts the dolt `--config <path>` (or `--config=<path>`) from
  # the args line it already holds, and emits `pid<TAB>config_path` so the
  # shell loop below can do the foreign-managed check without re-forking ps.
  candidate_pids=" $(pgrep -x dolt 2>/dev/null | tr '\n' ' ' || true)"
  ps -eo pid=,stat=,args= 2>/dev/null | awk \
    -v server="$server_pid" -v rigs="$rig_dolt_pids" -v cands="$candidate_pids" '
    BEGIN {
      # Build an O(1) lookup set from the pgrep candidates once. The
      # per-row membership test below was an index() substring scan
      # re-paid for every process-table row, i.e. O(rows x candidate
      # string length); the reported incident had ~41k candidate PIDs
      # (#2618). Splitting into an associative set makes each lookup O(1).
      n = split(cands, a, " ")
      for (i = 1; i <= n; i++) if (a[i] != "") cand[a[i]] = 1
    }
    {
      pid = $1
      if (!(pid in cand)) next                   # not a pgrep -x dolt match
      if (pid == server) next                     # the managed city server
      if (index(rigs, " " pid " ") != 0) next     # a configured rig-local dolt
      if ($2 ~ /Z/) next                          # Z-state zombie: never a server
      if (index($0, "sql-server") == 0) next      # not a dolt sql-server
      # Extract the dolt --config path from the args fields (args start at
      # $3 after pid/stat). Accept both the space-separated `--config PATH`
      # and the `--config=PATH` spellings. Emitted alongside the pid so the
      # shell can read the sibling dolt.pid; empty when no --config is given.
      config = ""
      for (i = 3; i <= NF; i++) {
        if ($i == "--config" && (i + 1) <= NF) { config = $(i+1); break }
        if (index($i, "--config=") == 1) { config = substr($i, 10); break }
      }
      print pid "\t" config
    }' > "$_zombie_scan_out" 2>/dev/null || true

  # Iterate ONLY the matched sql-servers (O(matched servers)) the awk pass
  # emitted — not the full candidate/zombie set. This loop is where the
  # foreign-managed decision lives; keeping it bounded by the awk output is
  # what preserves the bounded-fork invariant. Reading from the scratch file
  # (not a pipe) keeps the loop in the parent shell so the zombie_count /
  # zombie_pids accumulation survives.
  _tab="$(printf '\t')"
  while IFS="$_tab" read -r p config_path; do
    [ -n "$p" ] || continue
    # Ownership check for processes launched with an explicit --config.
    # The sibling dolt.pid (gc writes one next to every managed config)
    # is authoritative — we key on its presence, not on whether the
    # config file itself is readable (it may live in another user's home
    # on a shared host):
    #   - present and claims this PID   -> healthy gc-managed Dolt instance
    #     (another city/rig on this host) -> not a zombie.
    #   - present but claims a DIFFERENT PID -> a gc-style config dir whose
    #     recorded server died or was replaced -> still a zombie.
    #   - absent -> the process is NOT gc-managed (e.g. a launchd-managed
    #     or manually-started server for an unrelated app on its own
    #     datadir/port) -> not a town stray; exclude it so automation
    #     does not kill a healthy, unrelated Dolt server.
    if [ -n "$config_path" ]; then
      foreign_pid_file="$(dirname "$config_path")/dolt.pid"
      if [ -f "$foreign_pid_file" ]; then
        recorded_pid=$(head -1 "$foreign_pid_file" 2>/dev/null | tr -d ' \t\r\n')
        [ "$recorded_pid" = "$p" ] && continue
      else
        continue
      fi
    fi
    zombie_count=$((zombie_count + 1))
    zombie_pids="$zombie_pids $p"
  done < "$_zombie_scan_out"
fi

# Output.
timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

if [ "$json_output" = true ]; then
  # Build JSON output. `server.reachable` reports whether the SQL
  # handshake actually succeeded (port listening AND server answering
  # SELECT 1). Consumers should key health off
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
  # the document). Automation that parses the JSON must not fail before
  # stdout is parsed just because
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
