#!/bin/sh
# gc dolt cleanup — Find and remove orphaned Dolt databases.
#
# Discovers databases from the authoritative rig registry (all registered rigs,
# including external rigs outside GC_CITY_PATH). By default, lists orphaned
# databases (dry-run). Use --force to remove them.
# Use --max to set a safety limit (refuses if more orphans than --max).
#
# Removal strategy: when the dolt SQL server is reachable, --force issues
# `DROP DATABASE IF EXISTS` through the running server (server-side NBS lock
# serializes the close+remove safely). Falling back to filesystem `rm -rf`
# while the server has the database open corrupts NBS state and crash-loops
# the journal on next restart (#1549). The fallback is only taken when the
# server is provably unreachable AND the operator passes --server-down-ok.
#
# Environment: GC_CITY_PATH (also GC_DOLT_PORT, GC_DOLT_HOST, GC_DOLT_USER,
# GC_DOLT_PASSWORD when probing the running server)
set -e

force=false
max_orphans=50
server_down_ok=false
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --force) force=true; shift ;;
    --max)   max_orphans="$2"; shift 2 ;;
    --server-down-ok) server_down_ok=true; shift ;;
    -h|--help)
      echo "Usage: gc dolt cleanup [--force] [--max N] [--server-down-ok]"
      echo ""
      echo "Find Dolt databases not referenced by any registered rig."
      echo ""
      echo "Flags:"
      echo "  --force            Actually remove orphaned databases"
      echo "  --max N            Refuse if more than N orphans (default: 50)"
      echo "  --server-down-ok   Permit filesystem rm fallback when the dolt"
      echo "                     server is provably stopped. Without this flag"
      echo "                     --force refuses to run when dolt is unreachable,"
      echo "                     because rm -rf against a live server's data"
      echo "                     directory corrupts NBS state (#1549)."
      exit 0
      ;;
    *) echo "gc dolt cleanup: unknown flag: $1" >&2; exit 1 ;;
  esac
done

if [ ! -d "$data_dir" ]; then
  echo "No orphaned databases found."
  exit 0
fi

# metadata_files() — discover databases from authoritative rig registry.
# Uses gc rig list --json when available (all rigs, including external).
# Falls back to filesystem glob when gc is unavailable (local rigs only).
# Outputs: pathnames of .beads/metadata.json files (space-safe).
metadata_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/metadata.json"

  if command -v gc >/dev/null 2>&1; then
    rig_paths=$(gc rig list --json 2>/dev/null \
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

# Collect referenced database names from metadata.json files.
referenced=""
while IFS= read -r meta; do
  [ -z "$meta" ] && continue
  [ -f "$meta" ] || continue
  db=$(grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta" 2>/dev/null | sed 's/.*"dolt_database"[[:space:]]*:[[:space:]]*"//;s/"//' || true)
  [ -n "$db" ] && referenced="$referenced $db "
done <<EOF
$(metadata_files)
EOF

# Find orphans.
orphans=""
orphan_count=0
for d in "$data_dir"/*/; do
  [ ! -d "$d/.dolt" ] && continue
  name="$(basename "$d")"
  case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|__gc_probe) continue ;; esac
  case "$referenced" in
    *" $name "*) continue ;; # referenced, not orphan
  esac
  # Calculate size.
  size_bytes=$(du -sb "$d" 2>/dev/null | cut -f1 || echo 0)
  if [ "$size_bytes" -ge 1073741824 ]; then
    size=$(awk "BEGIN {printf \"%.1f GB\", $size_bytes/1073741824}")
  elif [ "$size_bytes" -ge 1048576 ]; then
    size=$(awk "BEGIN {printf \"%.1f MB\", $size_bytes/1048576}")
  elif [ "$size_bytes" -ge 1024 ]; then
    size=$(awk "BEGIN {printf \"%.1f KB\", $size_bytes/1024}")
  else
    size="${size_bytes} B"
  fi
  orphans="$orphans$name|$size|$d
"
  orphan_count=$((orphan_count + 1))
done

if [ "$orphan_count" -eq 0 ]; then
  echo "No orphaned databases found."
  exit 0
fi

# Precompute the non-HQ allowlist once from `gc rig list --json`. This lets us
# fail closed if the registry query or jq parse fails at runtime (not just if
# the binaries are missing), and avoids spawning N subprocess pairs for N
# orphans. The allowlist file is empty iff no non-HQ rigs are registered —
# distinguished from a *failed* query, which exits before any delete runs.
#
# compute_allowlist_file — write one non-HQ rig path per line to $1, or fail
# with exit 1 if the pipeline can't be completed.
compute_allowlist_file() {
  _out=$1
  if ! command -v gc >/dev/null 2>&1; then
    echo "gc dolt cleanup: gc not found on PATH; cannot evaluate rig overlap allowlist" >&2
    return 1
  fi
  if ! command -v jq >/dev/null 2>&1; then
    echo "gc dolt cleanup: jq not found on PATH; cannot evaluate rig overlap allowlist" >&2
    echo "install jq or remove orphans manually" >&2
    return 1
  fi
  _list=$(gc rig list --json 2>/dev/null) || {
    echo "gc dolt cleanup: gc rig list --json failed; refusing to run overlap allowlist unverified" >&2
    return 1
  }
  if ! printf '%s\n' "$_list" | jq -e '.rigs' >/dev/null 2>&1; then
    echo "gc dolt cleanup: gc rig list --json produced unparseable output; refusing to run overlap allowlist unverified" >&2
    return 1
  fi
  printf '%s\n' "$_list" | jq -r '.rigs[] | select(.hq != true) | .path' > "$_out" || return 1
}

# overlapping_rig_path — emit the non-HQ rig path from $allowlist_file that
# overlaps $1, or nothing if no overlap. Strips trailing slashes so
# `$data_dir/*/` glob output (always ending in `/`) matches against registry
# paths (no trailing slash).
overlapping_rig_path() {
  _db_path=${1%/}
  while IFS= read -r rig_path; do
    [ -z "$rig_path" ] && continue
    rig_path=${rig_path%/}
    # Exact equality, db under rig, or rig under db.
    if [ "$_db_path" = "$rig_path" ] \
      || case "$_db_path" in "$rig_path/"*) true ;; *) false ;; esac \
      || case "$rig_path" in "$_db_path/"*) true ;; *) false ;; esac
    then
      printf '%s\n' "$rig_path"
      return
    fi
  done < "$allowlist_file"
}

# Build the allowlist. Under --force, failure aborts before any rm -rf.
# Under dry-run, failure degrades to "no annotations" — we still print the
# table so operators can see what exists.
allowlist_file=$(mktemp)
trap 'rm -f "$allowlist_file" "${refused_tmp:-}"' EXIT
allowlist_ready=true
if ! compute_allowlist_file "$allowlist_file"; then
  allowlist_ready=false
  if [ "$force" = true ]; then
    exit 1
  fi
  : > "$allowlist_file"  # empty → no overlap annotations in dry-run
fi

# Print orphan table. Under dry-run, annotate entries that --force would refuse
# so users can preview refusals without running the destructive command.
printf "%-30s  %-12s  %s\n" "NAME" "SIZE" "STATUS"
echo "$orphans" | while IFS='|' read -r name size path; do
  [ -z "$name" ] && continue
  status=""
  if [ "$force" != true ] && [ "$allowlist_ready" = true ]; then
    overlap=$(overlapping_rig_path "$path")
    [ -n "$overlap" ] && status="refused: overlaps rig at $overlap"
  fi
  printf "%-30s  %-12s  %s\n" "$name" "$size" "$status"
done

# Safety limit.
if [ "$orphan_count" -gt "$max_orphans" ]; then
  echo "" >&2
  echo "gc dolt cleanup: $orphan_count orphans exceeds --max $max_orphans; remove manually or increase --max" >&2
  exit 1
fi

if [ "$force" != true ]; then
  echo ""
  echo "$orphan_count orphaned database(s). Use --force to remove."
  exit 0
fi

# Choose deletion strategy. Four states the probe can land in (the
# "cannot probe" state was missed initially — `managed_runtime_tcp_reachable`
# returns false for both genuinely-unreachable AND no-probe-tool-available,
# which would otherwise let --server-down-ok rm against a live server on
# systems missing both nc and python3):
#   * SELECT 1 succeeds → server is up and answering; SQL DROP is safe.
#   * Port reachable but SELECT 1 fails → server may still hold open fds;
#     refuse regardless of --server-down-ok (the flag advertises a STOPPED
#     server, not an unhealthy one).
#   * Cannot probe TCP (no nc, no python3) → cannot establish "stopped";
#     refuse regardless of --server-down-ok.
#   * Port unreachable → server is stopped; fall back to rm only when the
#     operator has acknowledged via --server-down-ok.
host="${GC_DOLT_HOST:-127.0.0.1}"
: "${GC_DOLT_USER:=root}"
export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"

# dolt_sql_q TIMEOUT QUERY  — invoke dolt CLI with each arg explicitly quoted
# so neither host nor user (env-controlled) word-splits into adjacent flags
# even on unexpected values. Stdout/stderr are captured by callers as needed.
dolt_sql_q() {
  _dolt_sql_q_timeout="$1"; shift
  run_bounded "$_dolt_sql_q_timeout" \
    dolt --host "$host" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls \
    sql -q "$1"
}

probe_available=false
if command -v nc >/dev/null 2>&1 || command -v python3 >/dev/null 2>&1; then
  probe_available=true
fi

tcp_reachable=false
if [ "$probe_available" = true ] \
  && [ -n "$GC_DOLT_PORT" ] \
  && command -v managed_runtime_tcp_reachable >/dev/null 2>&1 \
  && managed_runtime_tcp_reachable "$GC_DOLT_PORT"; then
  tcp_reachable=true
fi

sql_works=false
if [ "$tcp_reachable" = true ] \
  && command -v dolt >/dev/null 2>&1 \
  && command -v run_bounded >/dev/null 2>&1 \
  && dolt_sql_q 5 "SELECT 1" >/dev/null 2>&1; then
  sql_works=true
fi

unset delete_via
if [ "$sql_works" = true ]; then
  delete_via=sql
elif [ "$tcp_reachable" = true ]; then
  echo "gc dolt cleanup: dolt is listening on port $GC_DOLT_PORT but 'SELECT 1' failed;" >&2
  echo "  refusing to rm against a potentially-live server (#1549). Fix SQL access or stop dolt and retry." >&2
  exit 1
elif [ "$probe_available" = false ]; then
  echo "gc dolt cleanup: cannot probe TCP reachability (neither nc nor python3 available);" >&2
  echo "  refusing rm fallback regardless of --server-down-ok — cannot establish 'server is stopped' (#1549)." >&2
  echo "  Install nc or python3, or stop dolt and use 'dolt sql -q \"DROP DATABASE\"' against another live instance." >&2
  exit 1
elif [ "$server_down_ok" = true ]; then
  delete_via=rm
else
  echo "gc dolt cleanup: dolt server unreachable on port ${GC_DOLT_PORT:-unset};" >&2
  echo "  rm -rf against per-database dirs while the server is up corrupts NBS state (#1549)." >&2
  echo "  Either start dolt and re-run, or pass --server-down-ok if the server is intentionally stopped." >&2
  exit 1
fi
# Belt-and-suspenders: a future edit that opens a fall-through path here would
# silently route to the rm branch below, re-introducing the corruption #1549
# fixes. Crash loudly instead.
case "${delete_via:-}" in
  sql|rm) ;;
  *) echo "gc dolt cleanup: internal error — delete_via not set" >&2; exit 1 ;;
esac

# Remove each orphan. Track refusals and successful removals via tmpfiles so
# the subshell's counters survive (the pipe creates a subshell). Identifier-
# safety refusals are tracked separately because they signal "DB in an
# impossible state" (manual fs mucking, corrupted metadata, attempted
# injection) and must surface as a non-zero exit even when other orphans were
# removed successfully — overlap-allowlist refusals stay on the existing
# partial-progress semantics ("did the batch make as much progress as it
# could").
refused_tmp=$(mktemp)
removed_tmp=$(mktemp)
unsafe_tmp=$(mktemp)
trap 'rm -f "$allowlist_file" "$refused_tmp" "$removed_tmp" "$unsafe_tmp"' EXIT
echo "$orphans" | while IFS='|' read -r db_name size path; do
  [ -z "$db_name" ] && continue

  # Allowlist safety check: refuse if path overlaps any registered rig.
  # Exclude HQ: HQ's path is the city root; the managed data-dir (.beads/dolt/) is
  # always a subdirectory of it. Including HQ would refuse every orphan at the default
  # data-dir location. Only non-HQ rig paths need the overlap guard.
  overlap=$(overlapping_rig_path "$path")
  if [ -n "$overlap" ]; then
    echo "refusing to remove '$db_name': path overlaps registered rig at '$overlap'" >&2
    echo "refused" >> "$refused_tmp"
    continue
  fi

  # Identifier safety: dolt_database flows from operator-controlled metadata.json
  # straight into a backtick-quoted SQL identifier. Reject anything outside the
  # safe charset before interpolating, so an embedded backtick or semicolon
  # cannot break out of the quoted identifier into arbitrary SQL. Charset
  # matches `valid_database_name` in commands/gc-nudge/run.sh so a name probed
  # by `gc dolt health` or nudged by `gc dolt gc-nudge` is also reachable here.
  case "$db_name" in
    [A-Za-z0-9_]*)
      case "$db_name" in
        *[!A-Za-z0-9_-]*)
          echo "refusing to remove '$db_name': name contains forbidden characters (allowed: A-Z, a-z, 0-9, _, -)" >&2
          echo "unsafe" >> "$unsafe_tmp"
          continue
          ;;
      esac
      ;;
    *)
      echo "refusing to remove '$db_name': name must start with [A-Za-z0-9_]" >&2
      echo "unsafe" >> "$unsafe_tmp"
      continue
      ;;
  esac

  if [ "$delete_via" = sql ]; then
    # Capture stdout+stderr so a DROP failure (auth, TLS, unknown-db, etc.)
    # surfaces actionable detail to the operator instead of a generic message.
    if drop_output=$(dolt_sql_q 30 "DROP DATABASE IF EXISTS \`$db_name\`" 2>&1); then
      echo "removed" >> "$removed_tmp"
      echo "  Dropped $db_name"
    else
      echo "  Failed to drop $db_name via SQL: ${drop_output:-(no output)}" >&2
    fi
  else
    if rm -rf "$path"; then
      echo "removed" >> "$removed_tmp"
      echo "  Removed $db_name"
    else
      echo "  Failed to remove $db_name" >&2
    fi
  fi
done

# Count removed, refused (allowlist), and unsafe (identifier-safety) (the
# removal loop runs in a subshell, so the parent shell reads back through the
# tmpfiles).
removed=$(wc -l < "$removed_tmp" | tr -d ' ')
refused_count=$(wc -l < "$refused_tmp" | tr -d ' ')
unsafe_count=$(wc -l < "$unsafe_tmp" | tr -d ' ')
echo ""
echo "Removed $removed of $orphan_count orphaned database(s)."

# Exit non-zero when:
#   * any unsafe identifier was found — DB in an impossible state, demands
#     operator attention even if other orphans were removed, OR
#   * any orphan failed to remove (count math doesn't add up — silent failure
#     in the loop), OR
#   * the entire batch was refused (no progress made).
if [ "$unsafe_count" -gt 0 ] \
  || [ "$removed" -lt "$((orphan_count - refused_count - unsafe_count))" ] \
  || { [ "$refused_count" -gt 0 ] && [ "$removed" -eq 0 ]; }; then
  exit 1
fi
