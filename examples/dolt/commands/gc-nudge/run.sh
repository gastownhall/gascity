#!/bin/sh
# gc dolt gc-nudge — periodic CALL DOLT_GC() to bound the Dolt commit graph.
#
# Why this exists: Gas City's managed-Dolt launch path now forces
# `DOLT_GC_SCHEDULER=NONE` to work around
# https://github.com/dolthub/dolt/issues/10944, so threshold-triggered
# auto-GC can fire again on multi-core hosts. We still keep an hourly
# nudge because the bd workload can accumulate history quickly, and an
# unconditional `CALL DOLT_GC()` remains a cheap belt-and-suspenders
# backstop for reclaiming orphan chunks before they turn into disk bloat
# and tail-latency spikes.
#
# Policy: fire CALL DOLT_GC() unconditionally on every cooldown tick
# (default 1h). The GC is idempotent and near-free when there's nothing
# to reclaim. A threshold knob remains as an optional escape hatch.
#
# Runs from the dolt pack's dolt-gc-nudge order.
#
# Environment:
#   GC_CITY_PATH         (required) — city root
#   GC_DOLT_PORT         (required) — managed dolt port
#   GC_DOLT_HOST         (default: 127.0.0.1)
#   GC_DOLT_USER         (default: root)
#   GC_DOLT_PASSWORD     (optional)
#   GC_DOLT_GC_THRESHOLD_BYTES
#     (default: 0 — run unconditionally). Set a positive byte count to
#     skip GC on databases below that size; useful for test suites that
#     don't want GC noise on tiny fixtures.
#   GC_DOLT_GC_CALL_TIMEOUT_SECS
#     (default: 1800) — wall-clock bound for one `CALL DOLT_GC()` invocation.
#   GC_DOLT_GC_DRY_RUN   (optional) — when set, prints what would happen
#                        but does not execute CALL DOLT_GC().
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
    printf 'gc-nudge: managed local Dolt runtime is not applicable for this order — skip\n'
    exit 0
    ;;
esac

if [ "${GC_DOLT_MANAGED_LOCAL:-}" = "1" ]; then
  managed_port=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR" || true)
  if [ -n "$managed_port" ]; then
    if [ -n "$gc_dolt_port_input" ] && [ "$gc_dolt_port_input" != "$managed_port" ]; then
      printf 'gc-nudge: GC_DOLT_PORT=%s does not match managed runtime port=%s for data_dir=%s — skip\n' \
        "$gc_dolt_port_input" "$managed_port" "$DOLT_DATA_DIR"
      exit 0
    fi
    GC_DOLT_PORT="$managed_port"
  elif [ -z "$gc_dolt_port_input" ]; then
    printf 'gc-nudge: managed local Dolt runtime is not active for data_dir=%s — skip\n' \
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
      printf 'gc-nudge: GC_DOLT_HOST=%s is not a local managed Dolt host — skip\n' \
        "$gc_dolt_host_input"
      exit 0
      ;;
  esac
  managed_port=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR" || true)
  if [ -z "$managed_port" ] || [ "$gc_dolt_port_input" != "$managed_port" ]; then
    printf 'gc-nudge: GC_DOLT_PORT=%s does not match managed runtime port=%s for data_dir=%s — skip\n' \
      "$gc_dolt_port_input" "${managed_port:-<inactive>}" "$DOLT_DATA_DIR"
    exit 0
  fi
  GC_DOLT_PORT="$managed_port"
elif [ -z "$gc_dolt_port_input" ]; then
  managed_port=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR" || true)
  if [ -z "$managed_port" ]; then
    printf 'gc-nudge: managed local Dolt runtime is not active for data_dir=%s — skip\n' \
      "$DOLT_DATA_DIR"
    exit 0
  fi
  GC_DOLT_PORT="$managed_port"
fi

: "${GC_DOLT_PORT:?GC_DOLT_PORT must be set}"
: "${GC_DOLT_USER:=root}"

host="${GC_DOLT_HOST:-127.0.0.1}"
threshold="${GC_DOLT_GC_THRESHOLD_BYTES:-0}"
gc_call_timeout="${GC_DOLT_GC_CALL_TIMEOUT_SECS:-1800}"
dry_run="${GC_DOLT_GC_DRY_RUN:-}"

case "$threshold" in
  ''|*[!0-9]*)
    printf 'gc-nudge: invalid GC_DOLT_GC_THRESHOLD_BYTES=%s (must be a non-negative integer)\n' \
      "$threshold" >&2
    exit 2
    ;;
esac

case "$gc_call_timeout" in
  ''|*[!0-9]*|0)
    printf 'gc-nudge: invalid GC_DOLT_GC_CALL_TIMEOUT_SECS=%s (must be a positive integer)\n' \
      "$gc_call_timeout" >&2
    exit 2
    ;;
esac

# Cross-city flock to serialize CALL DOLT_GC() across multiple cities
# sharing the same Dolt sql-server. Keyed on host:port so per-city locks
# don't let concurrent GCs hit the same server.
lock_host=$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]' | sed 's/^\[\(.*\)\]$/\1/')
case "$lock_host" in
  ''|127.0.0.1|localhost|0.0.0.0|::1|::)
    lock_host="127.0.0.1"
    ;;
esac
lock_key=$(printf '%s-%s' "$lock_host" "$GC_DOLT_PORT" | tr -c 'A-Za-z0-9_.-' '-')
lock_root="/tmp/gc-dolt-gc"
mkdir -p "$lock_root"
chmod 700 "$lock_root" 2>/dev/null || true
lock_path="$lock_root/${lock_key}.lock"
lock_dir="${lock_path}.d"
lock_pid_path="${lock_dir}/pid"
lock_cmd_path="${lock_dir}/cmd"
lock_cleanup=""

# metadata_files — enumerate managed rig metadata.json files, same as
# commands/health/run.sh. Authoritative source is `gc rig list --json`;
# fall back to filesystem scan when gc is unavailable.
metadata_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/metadata.json"
  if command -v gc >/dev/null 2>&1; then
    # Bound the gc rig list call: if gc itself is wedged (we've seen this
    # during reconciler incidents) we must not block the nudge for the
    # full 35m order timeout. Degrade to the filesystem fallback below.
    # Matches the pattern in examples/dolt/commands/health/run.sh:22.
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
        printf 'gc-nudge: gc rig list timed out after 5s; falling back to local filesystem metadata scan\n' >&2
      else
        printf 'gc-nudge: gc rig list failed rc=%s; falling back to local filesystem metadata scan\n' "$rig_status" >&2
      fi
    fi
  fi
  # Fallback: scan the local city tree (excluding runtime/admin roots) so
  # rigs outside <city>/rigs are still discovered when gc is unavailable.
  # External rigs remain undiscoverable in this degraded path.
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
  db="$1"
  case "$db" in
    [A-Za-z0-9_]*)
      case "$db" in
        *[!A-Za-z0-9_-]*) return 1 ;;
        *) return 0 ;;
      esac
      ;;
    *) return 1 ;;
  esac
}

is_system_database() {
  name=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  case "$name" in
    information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) return 0 ;;
    *) return 1 ;;
  esac
}

emit_database_name() {
  db="$1"
  if ! valid_database_name "$db"; then
    printf 'gc-nudge: db=%s invalid database name — skip\n' "$db" >&2
    return 0
  fi
  if is_system_database "$db"; then
    printf 'gc-nudge: db=%s system database — skip\n' "$db" >&2
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

# dir_bytes — POSIX byte sum of a directory tree. Uses `du -sk` for
# portability across Linux/macOS; returns 0 if the path is missing.
dir_bytes() {
  dir="$1"
  if [ ! -d "$dir" ]; then
    printf '0'
    return 0
  fi
  kb=$(du -sk "$dir" 2>/dev/null | awk '{print $1}')
  case "$kb" in
    ''|*[!0-9]*) printf '0' ;;
    *) printf '%s' $((kb * 1024)) ;;
  esac
}

run_dolt_gc_for_db() {
  db="$1"
  [ -n "$db" ] || return 0

  cmd_rc=0

  db_dir="$DOLT_DATA_DIR/$db/.dolt"
  if [ ! -d "$db_dir" ]; then
    printf 'gc-nudge: db=%s local_data_dir=%s missing — skip\n' \
      "$db" "$db_dir"
    return 0
  fi
  size=$(dir_bytes "$db_dir")

  if [ "$threshold" -gt 0 ] && [ "${aggregate_gc:-0}" != "1" ] && [ "$size" -lt "$threshold" ]; then
    printf 'gc-nudge: db=%s bytes=%s below_threshold=%s — skip\n' \
      "$db" "$size" "$threshold"
    return 0
  fi

  if [ -n "$dry_run" ]; then
    printf 'gc-nudge: db=%s bytes=%s — dry-run (would CALL DOLT_GC)\n' \
      "$db" "$size"
    return 0
  fi

  printf 'gc-nudge: db=%s bytes=%s — calling DOLT_GC()...\n' "$db" "$size"
  start=$(date +%s)

  # CALL DOLT_GC() is disruptive on pre-1.75 Dolt; the dolt CLI shells
  # out to a fresh connection per invocation, so connection churn is
  # bounded to this one call. Server-side auto-GC is unaffected.
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  run_bounded "$gc_call_timeout" \
    dolt --host "$host" --port "$GC_DOLT_PORT" \
    --user "$GC_DOLT_USER" --no-tls \
    --use-db "$db" \
    sql -q "CALL DOLT_GC()" || cmd_rc=$?
  elapsed=$(( $(date +%s) - start ))

  after=$(dir_bytes "$db_dir")
  if [ "$cmd_rc" -eq 0 ]; then
    printf 'gc-nudge: db=%s before=%s after=%s reclaimed=%s duration=%ss — ok\n' \
      "$db" "$size" "$after" "$((size - after))" "$elapsed"
  elif [ "$cmd_rc" -eq 124 ]; then
    printf 'gc-nudge: db=%s bytes=%s duration=%ss timed out after=%ss rc=%s — error\n' \
      "$db" "$size" "$elapsed" "$gc_call_timeout" "$cmd_rc" >&2
  else
    printf 'gc-nudge: db=%s bytes=%s duration=%ss rc=%s — error\n' \
      "$db" "$size" "$elapsed" "$cmd_rc" >&2
  fi
  return "$cmd_rc"
}

# shellcheck disable=SC2317
cleanup() {
  if [ -n "${_meta_tmp:-}" ]; then
    rm -f "$_meta_tmp"
  fi
  if [ -n "${_db_tmp:-}" ]; then
    rm -f "$_db_tmp"
  fi
  if [ -n "${_unique_db_tmp:-}" ]; then
    rm -f "$_unique_db_tmp"
  fi
  if [ -n "$lock_cleanup" ]; then
    rm -f "$lock_pid_path" 2>/dev/null || true
    rm -f "$lock_cmd_path" 2>/dev/null || true
    rmdir "$lock_cleanup" 2>/dev/null || true
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
    printf 'gc-nudge: unable to write lock metadata %s\n' "$lock_pid_path" >&2
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

# Stale lock markers can survive SIGKILL / timeout paths. The pid file lets a
# later run distinguish a live holder from abandoned state before skipping.
clear_stale_lock_dir() {
  [ -d "$lock_dir" ] || return 0
  if [ ! -f "$lock_pid_path" ]; then
    # Another process may have just created the lock dir and not yet written
    # pid metadata. Give it a short chance to finish before reclaiming.
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
      printf 'gc-nudge: unable to create lock file %s\n' "$lock_path" >&2
      exit 1
    fi
    if ! exec 9<>"$lock_path"; then
      umask "$old_umask"
      if [ -d "$lock_path" ]; then
        return 1
      fi
      printf 'gc-nudge: unable to open lock file %s\n' "$lock_path" >&2
      exit 1
    fi
    umask "$old_umask"
    chmod 600 "$lock_path" 2>/dev/null || true
    if ! flock -n 9; then
      return $?
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
    printf 'gc-nudge: unable to create lock directory %s\n' "$lock_dir" >&2
    exit 1
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

  printf 'gc-nudge: unable to create lock directory %s\n' "$lock_dir" >&2
  exit 1
}

main() {
  trap cleanup EXIT

  # Non-blocking flock so two concurrent nudges (same host:port) don't
  # double-call GC. Skip silently when held — the other nudge is handling
  # it.
  if ! acquire_lock; then
    printf 'gc-nudge: another nudge already running for %s:%s — skipping\n' \
      "$host" "$GC_DOLT_PORT"
    exit 0
  fi

  # Snapshot rig list once. `metadata_files` can hit the gc binary which
  # may be slow — we only want to pay that once per run.
  _meta_tmp=$(mktemp)
  metadata_files > "$_meta_tmp"

  _db_tmp=$(mktemp)
  _unique_db_tmp=$(mktemp)
  discover_database_names > "$_db_tmp"

  seen_dbs=""
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    # Dedup: multiple rigs may share a database.
    case " $seen_dbs " in
      *" $db "*) continue ;;
    esac
    seen_dbs="$seen_dbs $db"
    printf '%s\n' "$db" >> "$_unique_db_tmp"
  done < "$_db_tmp"

  aggregate_size=0
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    db_dir="$DOLT_DATA_DIR/$db/.dolt"
    [ -d "$db_dir" ] || continue
    size=$(dir_bytes "$db_dir")
    aggregate_size=$((aggregate_size + size))
  done < "$_unique_db_tmp"

  aggregate_gc=0
  if [ "$threshold" -gt 0 ] && [ "$aggregate_size" -ge "$threshold" ]; then
    aggregate_gc=1
    printf 'gc-nudge: aggregate_bytes=%s threshold=%s — enabling GC for discovered databases\n' \
      "$aggregate_size" "$threshold"
  fi

  failed_count=0
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    if ! run_dolt_gc_for_db "$db"; then
      failed_count=$((failed_count + 1))
    fi
  done < "$_unique_db_tmp"

  if [ "$failed_count" -gt 0 ]; then
    printf 'gc-nudge: %s database(s) failed GC\n' "$failed_count" >&2
    exit 1
  fi
  exit 0
}

main "$@"
