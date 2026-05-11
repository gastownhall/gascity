#!/bin/sh
# gc dolt compact — flatten Dolt commit history on managed databases.
#
# Why this exists: every bead mutation creates a Dolt commit. Over time
# this builds an enormous commit graph (thousands of commits/day on busy
# cities). The commit graph IS the storage cost — DOLT_GC alone cannot
# reclaim space when all commits are live history. Flattening squashes
# the graph into a single commit and lets the next DOLT_GC reclaim
# orphaned chunks.
#
# This command replaces the formula-based mol-dog-compactor that was
# routed to the dog pool. Per the formula's own ZFC-exemption notice,
# compaction requires SQL access (database/sql) that agents don't have.
# Running as an exec order gives us direct SQL access via the dolt CLI.
#
# Algorithm (flatten mode):
#   1. Pre-flight: record row counts for all user tables.
#   2. Soft-reset to the root commit; all data stays staged.
#   3. Commit everything as a single "compaction: flatten history" commit.
#   4. Re-check post-flatten row counts. Any mismatch fails the run before
#      full GC unless the script can prove external-writer provenance.
#   5. Run CALL DOLT_GC('--full') to reclaim chunks orphaned by the flatten.
#
# Concurrent writes are not accepted as an explanation for row-count drift
# or value-hash drift unless the script can prove that provenance.
# Surgical mode (preserve recent N commits via interactive rebase) is
# intentionally not implemented; flatten is sufficient for bloat recovery
# and avoids the rebase-vs-concurrent-write hazards.
#
# Runs from the dolt pack's mol-dog-compactor order.
#
# Environment:
#   GC_CITY_PATH                          (required) — city root
#   GC_DOLT_PORT                          (required) — managed dolt port
#   GC_DOLT_HOST                          (default: 127.0.0.1)
#   GC_DOLT_USER                          (default: root)
#   GC_DOLT_PASSWORD                      (optional)
#   GC_DOLT_COMPACT_THRESHOLD_COMMITS
#     (default: 2000) — skip databases with fewer commits than this.
#   GC_DOLT_COMPACT_CALL_TIMEOUT_SECS
#     (default: 1800) — wall-clock bound for each SQL CALL.
#   GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS
#     (default: 120) — wall-clock bound for remote compare-and-push
#                     after local compaction. Push failures fail the order.
#   GC_DOLT_COMPACT_REMOTE               (optional) — remote to fetch/push.
#                                         Defaults to origin when present;
#                                         ambiguous multi-remote stores fail.
#   GC_DOLT_COMPACT_DRY_RUN              (optional) — when set, prints
#                                         what would happen but does not
#                                         execute any DOLT_RESET / COMMIT.
#   GC_DOLT_COMPACT_ONLY_DBS              (optional) — comma-separated list of
#                                         database names to compact. When set,
#                                         all other databases are skipped.
set -eu

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"
: "${GC_DOLT_PORT:=}"
gc_dolt_port_input="$GC_DOLT_PORT"
gc_dolt_host_input="${GC_DOLT_HOST:-}"

PACK_DIR="${GC_PACK_DIR:-$(unset CDPATH; cd -- "$(dirname "$0")/.." && pwd)}"
# shellcheck disable=SC1091
. "$PACK_DIR/assets/scripts/runtime.sh"

case "${GC_DOLT_MANAGED_LOCAL:-}" in
  0|false|FALSE|no|NO)
    printf 'compact: managed local Dolt runtime is not applicable for this order — skip\n'
    exit 0
    ;;
esac

if [ "${GC_DOLT_MANAGED_LOCAL:-}" = "1" ]; then
  managed_port=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR" || true)
  if [ -n "$managed_port" ]; then
    if [ -n "$gc_dolt_port_input" ] && [ "$gc_dolt_port_input" != "$managed_port" ]; then
      printf 'compact: GC_DOLT_PORT=%s does not match managed runtime port=%s for data_dir=%s — skip\n' \
        "$gc_dolt_port_input" "$managed_port" "$DOLT_DATA_DIR"
      exit 0
    fi
    GC_DOLT_PORT="$managed_port"
  elif [ -z "$gc_dolt_port_input" ]; then
    printf 'compact: managed local Dolt runtime is not active for data_dir=%s — skip\n' \
      "$DOLT_DATA_DIR"
    exit 0
  else
    GC_DOLT_PORT="$gc_dolt_port_input"
  fi
elif [ -n "$gc_dolt_port_input" ]; then
  case "$gc_dolt_host_input" in
    ''|127.0.0.1|localhost|0.0.0.0|::1|::|'[::1]'|'[::]')
      ;;
    *)
      printf 'compact: GC_DOLT_HOST=%s is not a local managed Dolt host — skip\n' \
        "$gc_dolt_host_input"
      exit 0
      ;;
  esac
  managed_port=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR" || true)
  if [ -z "$managed_port" ] || [ "$gc_dolt_port_input" != "$managed_port" ]; then
    printf 'compact: GC_DOLT_PORT=%s does not match managed runtime port=%s for data_dir=%s — skip\n' \
      "$gc_dolt_port_input" "${managed_port:-<inactive>}" "$DOLT_DATA_DIR"
    exit 0
  fi
  GC_DOLT_PORT="$managed_port"
elif [ -z "$gc_dolt_port_input" ]; then
  managed_port=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR" || true)
  if [ -z "$managed_port" ]; then
    printf 'compact: managed local Dolt runtime is not active for data_dir=%s — skip\n' \
      "$DOLT_DATA_DIR"
    exit 0
  fi
  GC_DOLT_PORT="$managed_port"
fi

: "${GC_DOLT_PORT:?GC_DOLT_PORT must be set}"
: "${GC_DOLT_USER:=root}"

host="${GC_DOLT_HOST:-127.0.0.1}"
threshold_commits="${GC_DOLT_COMPACT_THRESHOLD_COMMITS:-2000}"
call_timeout="${GC_DOLT_COMPACT_CALL_TIMEOUT_SECS:-1800}"
push_timeout="${GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS:-120}"
compact_remote="${GC_DOLT_COMPACT_REMOTE:-}"
dry_run="${GC_DOLT_COMPACT_DRY_RUN:-}"
only_dbs="${GC_DOLT_COMPACT_ONLY_DBS:-}"

case "$threshold_commits" in
  ''|*[!0-9]*)
    printf 'compact: invalid GC_DOLT_COMPACT_THRESHOLD_COMMITS=%s (must be a non-negative integer)\n' \
      "$threshold_commits" >&2
    exit 2
    ;;
esac

case "$call_timeout" in
  ''|*[!0-9]*|0)
    printf 'compact: invalid GC_DOLT_COMPACT_CALL_TIMEOUT_SECS=%s (must be a positive integer)\n' \
      "$call_timeout" >&2
    exit 2
    ;;
esac

case "$push_timeout" in
  ''|*[!0-9]*|0)
    printf 'compact: invalid GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS=%s (must be a positive integer)\n' \
      "$push_timeout" >&2
    exit 2
    ;;
esac

case "$compact_remote" in
  ''|[A-Za-z0-9_.-]*)
    case "$compact_remote" in
      *[!A-Za-z0-9_.-]*)
        printf 'compact: invalid GC_DOLT_COMPACT_REMOTE=%s\n' "$compact_remote" >&2
        exit 2
        ;;
    esac
    ;;
  *)
    printf 'compact: invalid GC_DOLT_COMPACT_REMOTE=%s\n' "$compact_remote" >&2
    exit 2
    ;;
esac

# Cross-city flock keyed on host:port so concurrent compactions on the
# same Dolt server don't interleave. Compaction holds open transactions
# and a second compactor running concurrently would race on the
# graph-rewrite step.
lock_host=$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]' | sed 's/^\[\(.*\)\]$/\1/')
case "$lock_host" in
  ''|127.0.0.1|localhost|0.0.0.0|::1|::)
    lock_host="127.0.0.1"
    ;;
esac
lock_key=$(printf '%s-%s' "$lock_host" "$GC_DOLT_PORT" | tr -c 'A-Za-z0-9_.-' '-')
lock_root="/tmp/gc-dolt-compact"
old_umask=$(umask)
umask 077
mkdir -p "$lock_root" || {
  umask "$old_umask"
  printf 'compact: unable to create lock directory %s\n' "$lock_root" >&2
  exit 1
}
umask "$old_umask"
chmod 700 "$lock_root" 2>/dev/null || {
  printf 'compact: unable to secure lock directory %s\n' "$lock_root" >&2
  exit 1
}
lock_path="$lock_root/${lock_key}.lock"
lock_dir="$lock_root/${lock_key}.dir"
lock_pid_path="$lock_dir/pid"
lock_cmd_path="$lock_dir/cmd"
pending_gc_dir="$PACK_STATE_DIR/compact-pending-gc"
pending_push_dir="$PACK_STATE_DIR/compact-pending-push"
quarantine_dir="$PACK_STATE_DIR/compact-quarantine"

# DB discovery uses rig metadata.json files first (authoritative), with a
# filesystem-scan fallback when gc itself is unavailable.
metadata_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/metadata.json"
  if command -v gc >/dev/null 2>&1; then
    if rig_json=$(run_bounded 5 gc rig list --json 2>/dev/null); then
      rig_paths=$(printf '%s\n' "$rig_json" \
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
    else
      rig_status=$?
      if [ "$rig_status" -eq 124 ]; then
        printf 'compact: gc rig list timed out after 5s; falling back to local filesystem metadata scan\n' >&2
      else
        printf 'compact: gc rig list failed rc=%s; falling back to local filesystem metadata scan\n' "$rig_status" >&2
      fi
    fi
  fi
  find "$GC_CITY_PATH" \
    \( -path "$GC_CITY_PATH/.gc" -o -path "$GC_CITY_PATH/.git" \) -prune -o \
    -path '*/.beads/metadata.json' -print 2>/dev/null || true
}

metadata_db() {
  meta="$1"
  db=""
  if [ ! -f "$meta" ]; then
    printf '%s\n' "beads"
    return 0
  fi
  if command -v jq >/dev/null 2>&1; then
    db=$(jq -r '.dolt_database // empty' "$meta" 2>/dev/null || true)
  else
    db=$(grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta" 2>/dev/null \
      | sed 's/.*: *"//;s/"$//' || true)
  fi
  if [ -z "$db" ]; then
    db="beads"
  fi
  printf '%s\n' "$db"
}

valid_database_name() {
  name="$1"
  case "$name" in
    [A-Za-z0-9_]*)
      case "$name" in
        *[!A-Za-z0-9_-]*) return 1 ;;
        *) return 0 ;;
      esac
      ;;
    *) return 1 ;;
  esac
}

valid_remote_name() {
  remote_candidate="$1"
  case "$remote_candidate" in
    [A-Za-z0-9_.-]*)
      case "$remote_candidate" in
        *[!A-Za-z0-9_.-]*) return 1 ;;
        *) return 0 ;;
      esac
      ;;
    *) return 1 ;;
  esac
}

is_system_database() {
  system_candidate=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  case "$system_candidate" in
    information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) return 0 ;;
    *) return 1 ;;
  esac
}

emit_database_name() {
  db="$1"
  if ! valid_database_name "$db"; then
    printf 'compact: db=%s invalid database name — skip\n' "$db" >&2
    return 0
  fi
  if is_system_database "$db"; then
    printf 'compact: db=%s system database — skip\n' "$db" >&2
    return 0
  fi
  printf '%s\n' "$db"
}

discover_database_names() {
  while IFS= read -r meta; do
    [ -n "$meta" ] || continue
    db=$(metadata_db "$meta")
    emit_database_name "$db"
  done < "$_meta_tmp"

  if [ -d "$DOLT_DATA_DIR" ]; then
    for d in "$DOLT_DATA_DIR"/*/; do
      [ -d "$d/.dolt" ] || continue
      db=${d%/}
      db=${db##*/}
      is_system_database "$db" && continue
      emit_database_name "$db"
    done
  fi
}

# dolt_query — wrapper that runs a single SQL statement against the
# managed server with the configured port/host/user. Honors the
# per-call timeout. Output is the raw -r result-format-tsv body.
dolt_query() {
  db="$1"
  query="$2"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  run_bounded "$call_timeout" \
    dolt --host "$host" --port "$GC_DOLT_PORT" \
    --user "$GC_DOLT_USER" --no-tls \
    --use-db "$db" \
    sql -r tabular -q "$query"
}

emit_error_file() {
  db="$1"
  err_file="$2"
  [ -s "$err_file" ] || return 0
  while IFS= read -r err_line; do
    printf 'compact: db=%s %s\n' "$db" "$err_line" >&2
  done < "$err_file"
}

query_single_cell() {
  db="$1"
  failure_message="$2"
  query="$3"
  out_tmp=$(mktemp)
  err_tmp=$(mktemp)
  if ! dolt_query "$db" "$query" > "$out_tmp" 2>"$err_tmp"; then
    printf 'compact: db=%s %s\n' "$db" "$failure_message" >&2
    emit_error_file "$db" "$err_tmp"
    rm -f "$out_tmp" "$err_tmp"
    return 1
  fi
  awk 'NR==4 {gsub(/[| ]/, ""); print; exit}' "$out_tmp"
  rm -f "$out_tmp" "$err_tmp"
}

# commit_count — count of commits reachable from main. Bounded scan
# (LIMIT 200000) so a runaway DB doesn't tie up the connection.
commit_count() {
  db="$1"
  query_single_cell "$db" "commit count probe failed" \
    "SELECT COUNT(*) FROM (SELECT 1 FROM dolt_log LIMIT 200000) AS t"
}

# root_commit — earliest commit hash on the main branch.
root_commit() {
  db="$1"
  query_single_cell "$db" "root commit probe failed" \
    "SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1"
}

# head_commit — current branch HEAD hash before flattening.
head_commit() {
  db="$1"
  query_single_cell "$db" "HEAD commit probe failed" \
    "SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1"
}

# user_tables — emit one user-table name per line (excludes dolt_*
# system tables and information_schema views).
user_tables() {
  db="$1"
  out_tmp=$(mktemp)
  err_tmp=$(mktemp)
  if ! dolt_query "$db" \
    "SELECT table_name FROM information_schema.tables WHERE table_schema = '$db' AND table_type = 'BASE TABLE' AND table_name NOT LIKE 'dolt\\_%' ESCAPE '\\\\' ORDER BY table_name" \
    > "$out_tmp" 2>"$err_tmp"; then
    printf 'compact: db=%s table list probe failed\n' "$db" >&2
    emit_error_file "$db" "$err_tmp"
    rm -f "$out_tmp" "$err_tmp"
    return 1
  fi
  awk 'NR>=4 && /^\|/ {gsub(/^\| | \|$/, ""); gsub(/ /, ""); if ($0 != "") print}' "$out_tmp"
  rm -f "$out_tmp" "$err_tmp"
}

# row_count — COUNT(*) for one table. Returns "" on error.
row_count() {
  db="$1"
  table="$2"
  query_single_cell "$db" "row count probe failed for table=$table" \
    "SELECT COUNT(*) FROM \`$table\`"
}

db_value_hash() {
  db="$1"
  query_single_cell "$db" "database value hash probe failed" \
    "SELECT DOLT_HASHOF_DB()"
}

remote_count() {
  db="$1"
  query_single_cell "$db" "remote count probe failed" \
    "SELECT COUNT(*) FROM dolt_remotes"
}

remote_exists() {
  db="$1"
  remote="$2"
  query_single_cell "$db" "remote existence probe failed" \
    "SELECT COUNT(*) FROM dolt_remotes WHERE name = '$remote'"
}

single_remote_name() {
  db="$1"
  query_single_cell "$db" "remote probe failed" \
    "SELECT name FROM dolt_remotes ORDER BY name LIMIT 1"
}

select_remote() {
  db="$1"

  if [ -n "$compact_remote" ]; then
    exists=$(remote_exists "$db" "$compact_remote") || return 1
    if [ "$exists" != "1" ]; then
      printf 'compact: db=%s configured remote=%s not found — fail\n' \
        "$db" "$compact_remote" >&2
      return 1
    fi
    printf '%s\n' "$compact_remote"
    return 0
  fi

  count=$(remote_count "$db") || return 1
  case "$count" in
    ''|*[!0-9]*)
      printf 'compact: db=%s remote count probe returned invalid value=%s\n' \
        "$db" "$count" >&2
      return 1
      ;;
  esac

  if [ "$count" -eq 0 ]; then
    printf '\n'
    return 0
  fi
  if [ "$count" -eq 1 ]; then
    single_remote_name "$db"
    return $?
  fi

  origin_exists=$(remote_exists "$db" "origin") || return 1
  if [ "$origin_exists" = "1" ]; then
    printf 'origin\n'
    return 0
  fi
  printf 'compact: db=%s multiple remotes found without origin; set GC_DOLT_COMPACT_REMOTE — fail\n' \
    "$db" >&2
  return 1
}

fetch_remote() {
  db="$1"
  remote="$2"
  dolt_query "$db" "CALL DOLT_FETCH('$remote')"
}

remote_main_head() {
  db="$1"
  remote="$2"
  query_single_cell "$db" "remote HEAD probe failed" \
    "SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/$remote/main'"
}

commit_exists_in_local_log() {
  db="$1"
  hash="$2"
  query_single_cell "$db" "remote ancestry probe failed" \
    "SELECT COUNT(*) FROM dolt_log WHERE commit_hash = '$hash'"
}

push_remote_main() {
  db="$1"
  remote="$2"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  run_bounded "$push_timeout" \
    dolt --host "$host" --port "$GC_DOLT_PORT" \
    --user "$GC_DOLT_USER" --no-tls \
    --use-db "$db" \
    sql -r tabular -q "CALL DOLT_PUSH('--force', '--set-upstream', '$remote', 'main')"
}

# preflight_counts — write "<table> <count>" lines for all user tables.
preflight_counts() {
  db="$1"
  out="$2"
  tables_tmp=$(mktemp)
  : > "$out"
  if ! user_tables "$db" > "$tables_tmp"; then
    rm -f "$tables_tmp"
    return 1
  fi
  preflight_failed=0
  while IFS= read -r t; do
    [ -n "$t" ] || continue
    if ! valid_database_name "$t"; then
      printf 'compact: db=%s invalid table name from information_schema table=%s — fail\n' \
        "$db" "$t" >&2
      preflight_failed=1
      break
    fi
    if ! cnt=$(row_count "$db" "$t"); then
      printf 'compact: db=%s pre-flight row count failed for table=%s\n' "$db" "$t" >&2
      preflight_failed=1
      break
    fi
    case "$cnt" in
      ''|*[!0-9]*)
        printf 'compact: db=%s pre-flight row count failed for table=%s\n' "$db" "$t" >&2
        preflight_failed=1
        break
        ;;
    esac
    printf '%s %s\n' "$t" "$cnt" >> "$out"
  done < "$tables_tmp"
  rm -f "$tables_tmp"
  return "$preflight_failed"
}

# verify_counts — re-count and compare against the pre-flight file.
# Count divergence fails unless the script can prove an external writer caused it.
verify_counts() {
  db="$1"
  preflight="$2"
  fail=0
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    t=${line%% *}
    expected=${line##* }
    if ! actual=$(row_count "$db" "$t"); then
      printf 'compact: db=%s post-flatten row count failed for table=%s\n' "$db" "$t" >&2
      fail=1
      continue
    fi
    case "$actual" in
      ''|*[!0-9]*)
        printf 'compact: db=%s post-flatten row count failed for table=%s\n' "$db" "$t" >&2
        fail=1
        continue
        ;;
    esac
    if [ "$actual" != "$expected" ]; then
      printf 'compact: db=%s row count changed after flatten table=%s before=%s after=%s\n' \
        "$db" "$t" "$expected" "$actual" >&2
      fail=1
    fi
  done < "$preflight"
  return "$fail"
}

oldgen_has_files() {
  db="$1"
  oldgen_dir="$DOLT_DATA_DIR/$db/.dolt/noms/oldgen"
  [ -d "$oldgen_dir" ] || return 1
  [ -n "$(find "$oldgen_dir" -mindepth 1 -print -quit 2>/dev/null)" ]
}

compact_marker_path() {
  dir="$1"
  db="$2"
  printf '%s/%s\n' "$dir" "$db"
}

has_compact_marker() {
  dir="$1"
  db="$2"
  [ -f "$(compact_marker_path "$dir" "$db")" ]
}

write_compact_marker() {
  dir="$1"
  db="$2"
  reason="$3"
  shift 3

  old_umask=$(umask)
  umask 077
  if ! mkdir -p "$dir"; then
    umask "$old_umask"
    printf 'compact: db=%s unable to create marker directory %s\n' "$db" "$dir" >&2
    return 1
  fi
  tmp=$(mktemp "$dir/$db.tmp.XXXXXX") || {
    umask "$old_umask"
    printf 'compact: db=%s unable to create marker in %s\n' "$db" "$dir" >&2
    return 1
  }
  umask "$old_umask"

  {
    printf 'db=%s\n' "$db"
    printf 'reason=%s\n' "$reason"
    printf 'created_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    while [ "$#" -gt 0 ]; do
      printf '%s\n' "$1"
      shift
    done
  } > "$tmp" || {
    rm -f "$tmp"
    printf 'compact: db=%s unable to write marker %s\n' "$db" "$tmp" >&2
    return 1
  }

  if ! mv -f "$tmp" "$(compact_marker_path "$dir" "$db")"; then
    rm -f "$tmp"
    printf 'compact: db=%s unable to install marker in %s\n' "$db" "$dir" >&2
    return 1
  fi
  return 0
}

compact_marker_value() {
  dir="$1"
  db="$2"
  key="$3"
  marker=$(compact_marker_path "$dir" "$db")
  [ -f "$marker" ] || return 1
  awk -v prefix="$key=" 'index($0, prefix) == 1 { print substr($0, length(prefix) + 1); exit }' "$marker"
}

clear_compact_marker() {
  dir="$1"
  db="$2"
  rm -f "$(compact_marker_path "$dir" "$db")"
}

run_full_gc() {
  db="$1"
  failure_prefix="$2"
  success_prefix="$3"
  start="$4"

  printf 'compact: db=%s — running DOLT_GC --full...\n' "$db"
  gc_rc=0
  gc_err_tmp=$(mktemp)
  dolt_query "$db" "CALL DOLT_GC('--full')" >/dev/null 2>"$gc_err_tmp" || gc_rc=$?

  elapsed=$(( $(date +%s) - start ))
  if [ "$gc_rc" -ne 0 ]; then
    printf 'compact: db=%s %s DOLT_GC failed rc=%s duration=%ss\n' \
      "$db" "$failure_prefix" "$gc_rc" "$elapsed" >&2
    emit_error_file "$db" "$gc_err_tmp"
    rm -f "$gc_err_tmp"
    return 1
  fi
  rm -f "$gc_err_tmp"

  printf 'compact: db=%s %s duration=%ss — ok\n' \
    "$db" "$success_prefix" "$elapsed"
  return 0
}

push_remote_after_compaction() {
  db="$1"
  remote="$2"
  expected_remote_head="$3"
  [ -n "$remote" ] || return 0

  fetch_rc=0
  fetch_err_tmp=$(mktemp)
  fetch_remote "$db" "$remote" >/dev/null 2>"$fetch_err_tmp" || fetch_rc=$?
  if [ "$fetch_rc" -ne 0 ]; then
    printf 'compact: db=%s remote=%s fetch failed rc=%s before push after local compaction\n' \
      "$db" "$remote" "$fetch_rc" >&2
    emit_error_file "$db" "$fetch_err_tmp"
    rm -f "$fetch_err_tmp"
    write_compact_marker "$pending_push_dir" "$db" "flatten and full GC succeeded but remote fetch before push failed" || true
    return 1
  fi
  rm -f "$fetch_err_tmp"

  if ! latest_remote_head=$(remote_main_head "$db" "$remote"); then
    printf 'compact: db=%s remote=%s HEAD probe failed before push after local compaction\n' \
      "$db" "$remote" >&2
    write_compact_marker "$pending_push_dir" "$db" "flatten and full GC succeeded but remote HEAD probe before push failed" || true
    return 1
  fi
  if [ -n "$latest_remote_head" ]; then
    case "$latest_remote_head" in
      *[!A-Za-z0-9]*)
        printf 'compact: db=%s remote=%s returned invalid HEAD=%s before push — fail\n' \
          "$db" "$remote" "$latest_remote_head" >&2
        write_compact_marker "$pending_push_dir" "$db" "flatten and full GC succeeded but remote HEAD before push was invalid" || true
        return 1
        ;;
    esac
  fi
  if [ "$latest_remote_head" != "$expected_remote_head" ]; then
    printf 'compact: db=%s remote=%s HEAD changed before push expected_HEAD=%s got_HEAD=%s — leaving local compaction pending remote repair\n' \
      "$db" "$remote" "${expected_remote_head:-<empty>}" "${latest_remote_head:-<empty>}" >&2
    write_compact_marker "$pending_push_dir" "$db" "flatten and full GC succeeded but remote HEAD changed before push" || true
    return 1
  fi

  push_rc=0
  push_err_tmp=$(mktemp)
  push_remote_main "$db" "$remote" >/dev/null 2>"$push_err_tmp" || push_rc=$?
  if [ "$push_rc" -ne 0 ]; then
    printf 'compact: db=%s remote=%s push failed rc=%s after local compaction\n' \
      "$db" "$remote" "$push_rc" >&2
    emit_error_file "$db" "$push_err_tmp"
    rm -f "$push_err_tmp"
    write_compact_marker "$pending_push_dir" "$db" "flatten and full GC succeeded but remote push failed" || true
    return 1
  fi
  rm -f "$push_err_tmp"
  clear_compact_marker "$pending_push_dir" "$db"
  printf 'compact: db=%s remote=%s pushed compacted main\n' "$db" "$remote"
  return 0
}

restore_head_if_current() {
  db="$1"
  head="$2"
  expected_current="$3"
  reason="$4"

  current_head=$(head_commit "$db" || true)
  if [ "$current_head" = "$head" ]; then
    printf 'compact: db=%s already at pre-flatten HEAD=%s after %s\n' \
      "$db" "$head" "$reason" >&2
    return 0
  fi
  if [ "$current_head" != "$expected_current" ]; then
    printf 'compact: db=%s current HEAD=%s is neither pre-flatten HEAD=%s nor expected recovery HEAD=%s after %s — refusing hard reset; manual repair required\n' \
      "$db" "${current_head:-<empty>}" "$head" "$expected_current" "$reason" >&2
    return 1
  fi

  restore_rc=0
  restore_err_tmp=$(mktemp)
  dolt_query "$db" "CALL DOLT_RESET('--hard', '$head')" >/dev/null 2>"$restore_err_tmp" || restore_rc=$?
  if [ "$restore_rc" -ne 0 ]; then
    printf 'compact: db=%s restore to pre-flatten HEAD=%s failed rc=%s — manual repair required\n' \
      "$db" "$head" "$restore_rc" >&2
    emit_error_file "$db" "$restore_err_tmp"
    rm -f "$restore_err_tmp"
    return 1
  fi
  rm -f "$restore_err_tmp"

  restored_head=$(head_commit "$db" || true)
  if [ "$restored_head" != "$head" ]; then
    printf 'compact: db=%s restore verification failed want_HEAD=%s got_HEAD=%s after %s — manual repair required\n' \
      "$db" "$head" "${restored_head:-<empty>}" "$reason" >&2
    return 1
  fi
  printf 'compact: db=%s restored pre-flatten HEAD=%s after %s\n' \
    "$db" "$head" "$reason" >&2
  return 0
}

restore_head_after_flatten_failure() {
  db="$1"
  head="$2"
  root="$3"
  restore_head_if_current "$db" "$head" "$root" "flatten failure"
}

preserve_head_after_integrity_failure() {
  db="$1"
  flatten_head="$3"
  current_head=$(head_commit "$db" || true)
  if [ -z "$current_head" ]; then
    current_head="$flatten_head"
  fi
  printf 'compact: db=%s leaving post-flatten HEAD=%s in place after integrity failure; manual repair required before compaction or GC to avoid discarding possible writer data\n' \
    "$db" "${current_head:-<empty>}" >&2
  return 0
}

flatten_database() {
  db="$1"

  if [ -n "$only_dbs" ]; then
    case ",$only_dbs," in
      *,"$db",*) ;;
      *)
        printf 'compact: db=%s not in GC_DOLT_COMPACT_ONLY_DBS — skip\n' "$db"
        return 0
        ;;
    esac
  fi

  if has_compact_marker "$quarantine_dir" "$db"; then
    printf 'compact: db=%s integrity quarantine marker exists — manual intervention required before compaction or GC\n' \
      "$db" >&2
    return 1
  fi

  if has_compact_marker "$pending_push_dir" "$db"; then
    printf 'compact: db=%s pending remote push marker exists — manual intervention required before compaction or GC\n' \
      "$db" >&2
    return 1
  fi

  if has_compact_marker "$pending_gc_dir" "$db"; then
    if [ -n "$dry_run" ]; then
      printf 'compact: db=%s pending_gc=present — dry-run (would retry DOLT_GC --full)\n' "$db"
      return 0
    fi
    pending_remote=$(compact_marker_value "$pending_gc_dir" "$db" remote || true)
    pending_expected_remote_head=$(compact_marker_value "$pending_gc_dir" "$db" expected_remote_head || true)
    if [ -n "$pending_remote" ] && ! valid_remote_name "$pending_remote"; then
      printf 'compact: db=%s pending_gc marker has invalid remote=%s — manual intervention required\n' \
        "$db" "$pending_remote" >&2
      return 1
    fi
    if [ -n "$pending_expected_remote_head" ]; then
      case "$pending_expected_remote_head" in
        *[!A-Za-z0-9]*)
          printf 'compact: db=%s pending_gc marker has invalid expected_remote_head=%s — manual intervention required\n' \
            "$db" "$pending_expected_remote_head" >&2
          return 1
          ;;
      esac
    fi
    printf 'compact: db=%s pending_gc=present — retrying DOLT_GC --full\n' "$db"
    start=$(date +%s)
    if run_full_gc "$db" "pending-GC retry" "pending-GC retry" "$start"; then
      clear_compact_marker "$pending_gc_dir" "$db"
      push_remote_after_compaction "$db" "$pending_remote" "$pending_expected_remote_head"
      return $?
    fi
    return 1
  fi

  if ! count=$(commit_count "$db"); then
    return 1
  fi
  case "$count" in
    ''|*[!0-9]*)
      printf 'compact: db=%s commit count probe returned invalid value=%s\n' "$db" "$count" >&2
      return 1
      ;;
  esac

  if [ "$count" -lt "$threshold_commits" ]; then
    if oldgen_has_files "$db"; then
      printf 'compact: db=%s commits=%s below_threshold=%s oldgen_archives=present pending_gc=absent — skip\n' \
        "$db" "$count" "$threshold_commits"
      return 0
    fi
    printf 'compact: db=%s commits=%s below_threshold=%s — skip\n' \
      "$db" "$count" "$threshold_commits"
    return 0
  fi

  if ! root=$(root_commit "$db"); then
    return 1
  fi
  if [ -z "$root" ]; then
    printf 'compact: db=%s root commit probe returned empty value — fail\n' "$db" >&2
    return 1
  fi

  if ! head=$(head_commit "$db"); then
    return 1
  fi
  if [ -z "$head" ]; then
    printf 'compact: db=%s HEAD commit probe returned empty value — fail\n' "$db" >&2
    return 1
  fi

  if [ -n "$dry_run" ]; then
    printf 'compact: db=%s commits=%s root=%s — dry-run (would flatten)\n' \
      "$db" "$count" "$root"
    return 0
  fi

  remote=""
  expected_remote_head=""
  if probed_remote=$(select_remote "$db"); then
    remote="$probed_remote"
  else
    printf 'compact: db=%s remote selection failed — fail\n' "$db" >&2
    return 1
  fi
  if [ -n "$remote" ]; then
    if ! valid_remote_name "$remote"; then
      printf 'compact: db=%s invalid remote name=%s — fail\n' "$db" "$remote" >&2
      return 1
    fi

    printf 'compact: db=%s remote=%s — fetching before flatten...\n' "$db" "$remote"
    fetch_rc=0
    fetch_err_tmp=$(mktemp)
    fetch_remote "$db" "$remote" >/dev/null 2>"$fetch_err_tmp" || fetch_rc=$?
    if [ "$fetch_rc" -ne 0 ]; then
      printf 'compact: db=%s remote=%s fetch failed rc=%s — aborting before flatten\n' \
        "$db" "$remote" "$fetch_rc" >&2
      emit_error_file "$db" "$fetch_err_tmp"
      rm -f "$fetch_err_tmp"
      return 1
    else
      if ! remote_head=$(remote_main_head "$db" "$remote"); then
        rm -f "$fetch_err_tmp"
        return 1
      fi
      expected_remote_head="$remote_head"
      if [ -n "$remote_head" ] && [ "$remote_head" != "$head" ]; then
        case "$remote_head" in
          *[!A-Za-z0-9]*)
            printf 'compact: db=%s remote=%s returned invalid HEAD=%s — fail\n' \
              "$db" "$remote" "$remote_head" >&2
            rm -f "$fetch_err_tmp"
            return 1
            ;;
        esac
        if ! in_local=$(commit_exists_in_local_log "$db" "$remote_head"); then
          rm -f "$fetch_err_tmp"
          return 1
        fi
        if [ "$in_local" != "1" ]; then
          printf 'compact: db=%s remote=%s remote HEAD=%s is not in local history — aborting before flatten\n' \
            "$db" "$remote" "$remote_head" >&2
          rm -f "$fetch_err_tmp"
          return 1
        else
          printf 'compact: db=%s remote=%s fetch ok\n' "$db" "$remote"
        fi
      else
        printf 'compact: db=%s remote=%s fetch ok\n' "$db" "$remote"
      fi
    fi
    rm -f "$fetch_err_tmp"
  fi

  preflight_tmp=$(mktemp)
  if ! preflight_counts "$db" "$preflight_tmp"; then
    rm -f "$preflight_tmp"
    return 1
  fi
  if ! preflight_hash=$(db_value_hash "$db"); then
    rm -f "$preflight_tmp"
    return 1
  fi
  if [ -z "$preflight_hash" ]; then
    printf 'compact: db=%s database value hash probe returned empty value — fail\n' "$db" >&2
    rm -f "$preflight_tmp"
    return 1
  fi
  table_count=$(wc -l < "$preflight_tmp")
  printf 'compact: db=%s commits=%s root=%s tables=%s — flattening...\n' \
    "$db" "$count" "$root" "$table_count"

  current_head=$(head_commit "$db" || true)
  if [ "$current_head" != "$head" ]; then
    printf 'compact: db=%s HEAD changed before flatten want_HEAD=%s got_HEAD=%s — aborting before reset\n' \
      "$db" "$head" "${current_head:-<empty>}" >&2
    rm -f "$preflight_tmp"
    return 1
  fi

  start=$(date +%s)

  # Soft-reset to root + commit-everything is the flatten transaction.
  # Both run in a single dolt sql invocation so the session keeps the
  # USE selection across the two CALLs.
  reset_rc=0
  reset_err_tmp=$(mktemp)
  dolt_query "$db" "
    CALL DOLT_RESET('--soft', '$root');
    CALL DOLT_COMMIT('-Am', 'compaction: flatten history');
  " >/dev/null 2>"$reset_err_tmp" || reset_rc=$?

  if [ "$reset_rc" -ne 0 ]; then
    printf 'compact: db=%s flatten failed rc=%s — restoring pre-flatten HEAD=%s\n' \
      "$db" "$reset_rc" "$head" >&2
    emit_error_file "$db" "$reset_err_tmp"
    rm -f "$preflight_tmp"
    rm -f "$reset_err_tmp"
    restore_head_after_flatten_failure "$db" "$head" "$root" || true
    return 1
  fi
  rm -f "$reset_err_tmp"

  flatten_head=$(head_commit "$db" || true)
  if [ -z "$flatten_head" ]; then
    printf 'compact: db=%s post-flatten HEAD probe failed — quarantine and investigate before GC\n' \
      "$db" >&2
    write_compact_marker "$quarantine_dir" "$db" "post-flatten HEAD probe failed" || true
    rm -f "$preflight_tmp"
    return 1
  fi

  post_hash=$(db_value_hash "$db" || true)
  if [ -z "$post_hash" ]; then
    printf 'compact: db=%s post-flatten database value hash probe failed — quarantine and investigate before GC\n' \
      "$db" >&2
    write_compact_marker "$quarantine_dir" "$db" "post-flatten database value hash probe failed" || true
    preserve_head_after_integrity_failure "$db" "$head" "$flatten_head" || true
    rm -f "$preflight_tmp"
    return 1
  fi
  if [ "$post_hash" != "$preflight_hash" ]; then
    printf 'compact: db=%s value hash changed after flatten before_hash=%s after_hash=%s — quarantine and investigate before GC\n' \
      "$db" "$preflight_hash" "$post_hash" >&2
    write_compact_marker "$quarantine_dir" "$db" "post-flatten value hash changed" || true
    preserve_head_after_integrity_failure "$db" "$head" "$flatten_head" || true
    rm -f "$preflight_tmp"
    return 1
  fi

  if ! verify_counts "$db" "$preflight_tmp"; then
    printf 'compact: db=%s post-flatten INTEGRITY check failed — escalate (row counts diverged; investigate before re-running)\n' \
      "$db" >&2
    write_compact_marker "$quarantine_dir" "$db" "post-flatten row count changed" || true
    preserve_head_after_integrity_failure "$db" "$head" "$flatten_head" || true
    rm -f "$preflight_tmp"
    return 1
  fi
  rm -f "$preflight_tmp"

  after_count=$(commit_count "$db" || true)

  # CALL DOLT_GC() alone only reclaims working-set chunks — the bulk of
  # the orphaned history lives in noms/oldgen/ archives that require
  # --full to rewrite. Since flatten always orphans the entire prior
  # commit graph, --full is always appropriate here.
  if run_full_gc "$db" "flatten ok commits=$count->${after_count:-?} but" \
    "commits=$count->${after_count:-?}" "$start"; then
    clear_compact_marker "$pending_gc_dir" "$db"
    push_remote_after_compaction "$db" "$remote" "$expected_remote_head"
    return $?
  fi
  write_compact_marker "$pending_gc_dir" "$db" "flatten succeeded but full GC failed" \
    "remote=$remote" "expected_remote_head=$expected_remote_head" || true
  return 1
}

# shellcheck disable=SC2317
cleanup() {
  if [ "$flock_acquired" = "1" ]; then
    flock -u 9 2>/dev/null || true
    exec 9>&- 2>/dev/null || true
    rm -f "$lock_path" 2>/dev/null || true
  fi
  if [ -n "$lock_cleanup" ]; then
    rm -f "$lock_pid_path" "$lock_cmd_path" 2>/dev/null || true
    rmdir "$lock_cleanup" 2>/dev/null || true
  fi
  if [ -n "${_meta_tmp:-}" ]; then
    rm -f "$_meta_tmp"
  fi
  if [ -n "${_db_tmp:-}" ]; then
    rm -f "$_db_tmp"
  fi
  if [ -n "${_unique_db_tmp:-}" ]; then
    rm -f "$_unique_db_tmp"
  fi
}

lock_process_command() {
  pid="$1"
  command -v ps >/dev/null 2>&1 || return 1
  ps -p "$pid" -o command= 2>/dev/null | sed -n '1p'
}

lock_holder_alive() {
  [ -f "$lock_pid_path" ] || return 1
  pid=$(sed -n '1p' "$lock_pid_path" 2>/dev/null || true)
  case "$pid" in
    ''|*[!0-9]*) return 1 ;;
  esac

  current_cmd=$(lock_process_command "$pid" || true)
  if [ -f "$lock_cmd_path" ]; then
    expected_cmd=$(sed -n '1p' "$lock_cmd_path" 2>/dev/null || true)
    if [ -n "$current_cmd" ] && [ "$current_cmd" = "$expected_cmd" ]; then
      return 0
    fi
    if [ -n "$current_cmd" ]; then
      return 1
    fi
  fi

  if kill -0 "$pid" 2>/dev/null; then
    return 0
  fi
  [ -n "$current_cmd" ]
}

claim_lock_dir() {
  old_umask=$(umask)
  umask 077
  if ! mkdir "$lock_dir" 2>/dev/null; then
    umask "$old_umask"
    return 1
  fi
  if ! printf '%s\n' "$$" > "$lock_pid_path"; then
    umask "$old_umask"
    rmdir "$lock_dir" 2>/dev/null || true
    printf 'compact: unable to write lock metadata %s\n' "$lock_pid_path" >&2
    exit 1
  fi
  lock_cmd=$(lock_process_command "$$" || true)
  if [ -n "$lock_cmd" ]; then
    printf '%s\n' "$lock_cmd" > "$lock_cmd_path" 2>/dev/null || true
  fi
  umask "$old_umask"
  lock_cleanup="$lock_dir"
  return 0
}

clear_stale_lock_dir() {
  [ -d "$lock_dir" ] || return 0
  if [ ! -f "$lock_pid_path" ]; then
    sleep 1
  fi
  if lock_holder_alive; then
    return 1
  fi
  rm -f "$lock_pid_path" "$lock_cmd_path" 2>/dev/null || true
  rmdir "$lock_dir" 2>/dev/null
}

acquire_lock() {
  if command -v flock >/dev/null 2>&1; then
    old_umask=$(umask)
    umask 077
    if ! : >> "$lock_path" 2>/dev/null; then
      umask "$old_umask"
      if [ -d "$lock_path" ]; then
        return 1
      fi
      printf 'compact: unable to create lock file %s\n' "$lock_path" >&2
      exit 1
    fi
    if ! exec 9<>"$lock_path"; then
      umask "$old_umask"
      if [ -d "$lock_path" ]; then
        return 1
      fi
      printf 'compact: unable to open lock file %s\n' "$lock_path" >&2
      exit 1
    fi
    umask "$old_umask"
    chmod 600 "$lock_path" 2>/dev/null || true
    if ! flock -n 9; then
      return 1
    fi
    flock_acquired=1
    if claim_lock_dir; then
      return 0
    fi
    if [ -d "$lock_dir" ] && clear_stale_lock_dir && claim_lock_dir; then
      return 0
    fi
    return 1
  fi

  if claim_lock_dir; then
    return 0
  fi
  if [ -d "$lock_dir" ] && clear_stale_lock_dir && claim_lock_dir; then
    return 0
  fi
  if [ -d "$lock_dir" ]; then
    return 1
  fi

  printf 'compact: unable to create lock directory %s\n' "$lock_dir" >&2
  exit 1
}

main() {
  lock_cleanup=""
  flock_acquired=""
  _meta_tmp=""
  _db_tmp=""
  _unique_db_tmp=""
  trap cleanup EXIT

  # Non-blocking host:port lock. Skip rather than queue up; the other
  # compactor is handling this Dolt server.
  if ! acquire_lock; then
    printf 'compact: another compaction already running for %s:%s — skipping\n' \
      "$host" "$GC_DOLT_PORT"
    exit 0
  fi

  _meta_tmp=$(mktemp)
  metadata_files > "$_meta_tmp"

  _db_tmp=$(mktemp)
  _unique_db_tmp=$(mktemp)
  discover_database_names > "$_db_tmp"

  seen_dbs=""
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    case " $seen_dbs " in
      *" $db "*) continue ;;
    esac
    seen_dbs="$seen_dbs $db"
    printf '%s\n' "$db" >> "$_unique_db_tmp"
  done < "$_db_tmp"

  failed_count=0
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    if ! flatten_database "$db"; then
      failed_count=$((failed_count + 1))
    fi
  done < "$_unique_db_tmp"

  if [ "$failed_count" -gt 0 ]; then
    printf 'compact: %s database(s) failed compaction\n' "$failed_count" >&2
    exit 1
  fi
  exit 0
}

main "$@"
