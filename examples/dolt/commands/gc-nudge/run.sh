#!/bin/sh
# gc dolt gc-nudge — Size-triggered CALL DOLT_GC() to compact a bloated
# Dolt database.
#
# Why this exists: Dolt's auto-GC (default-on in 1.75+) fires on *growth*
# — 125 MB delta since last GC. A database that bloated once and then
# stabilized never auto-GCs on its own. This command closes that corner:
# it checks disk size on each registered rig's Dolt database, and if any
# are above the configured threshold, issues CALL DOLT_GC() against the
# managed sql-server.
#
# Runs from the dolt pack's dolt-gc-nudge order on a slow cooldown (6h by
# default). Intended to be idempotent and cheap when nothing needs GC.
#
# Environment:
#   GC_CITY_PATH         (required) — city root
#   GC_DOLT_PORT         (required) — managed dolt port
#   GC_DOLT_HOST         (default: 127.0.0.1)
#   GC_DOLT_USER         (default: root)
#   GC_DOLT_PASSWORD     (optional)
#   GC_DOLT_GC_THRESHOLD_BYTES
#     (default: 2147483648 = 2 GiB) — minimum .dolt/ size that triggers GC.
#     Set to 0 to force GC on every tick (useful for tests).
#   GC_DOLT_GC_DRY_RUN   (optional) — when set, prints what would happen
#                        but does not execute CALL DOLT_GC().
set -eu

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"
: "${GC_DOLT_PORT:?GC_DOLT_PORT must be set}"
: "${GC_DOLT_USER:=root}"

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

host="${GC_DOLT_HOST:-127.0.0.1}"
threshold="${GC_DOLT_GC_THRESHOLD_BYTES:-2147483648}"
dry_run="${GC_DOLT_GC_DRY_RUN:-}"

# Cross-city flock to serialize CALL DOLT_GC() across multiple cities
# sharing the same Dolt sql-server. Keyed on host:port so per-city locks
# don't let concurrent GCs hit the same server.
lock_key=$(printf '%s-%s' "$host" "$GC_DOLT_PORT" | tr ':/ ' '---')
lock_file="${TMPDIR:-/tmp}/gc-dolt-gc-${lock_key}.lock"

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
  find "$GC_CITY_PATH/rigs" -path '*/.beads/metadata.json' 2>/dev/null || true
}

metadata_db() {
  meta="$1"
  [ -f "$meta" ] || return 0
  if command -v jq >/dev/null 2>&1; then
    jq -r '.dolt_database // empty' "$meta" 2>/dev/null || true
    return
  fi
  grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta" 2>/dev/null \
    | sed 's/.*: *"//;s/"$//' || true
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

  # Reset rc at function entry. Variables in POSIX sh are global: without
  # this reset, a previous call's non-zero rc would leak into subsequent
  # calls whose dolt command succeeds (the `|| rc=$?` branch never fires)
  # and we'd mis-report success as failure.
  rc=0

  db_dir="$DOLT_DATA_DIR/$db/.dolt"
  size=$(dir_bytes "$db_dir")

  if [ "$threshold" -gt 0 ] && [ "$size" -lt "$threshold" ]; then
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
  if [ -n "${GC_DOLT_PASSWORD:-}" ]; then
    dolt --host "$host" --port "$GC_DOLT_PORT" \
      --user "$GC_DOLT_USER" --password "$GC_DOLT_PASSWORD" \
      --no-tls \
      sql --database "$db" -q "CALL DOLT_GC()" || rc=$?
  else
    dolt --host "$host" --port "$GC_DOLT_PORT" \
      --user "$GC_DOLT_USER" --no-tls \
      sql --database "$db" -q "CALL DOLT_GC()" || rc=$?
  fi
  elapsed=$(( $(date +%s) - start ))

  after=$(dir_bytes "$db_dir")
  if [ "$rc" -eq 0 ]; then
    printf 'gc-nudge: db=%s before=%s after=%s reclaimed=%s duration=%ss — ok\n' \
      "$db" "$size" "$after" "$((size - after))" "$elapsed"
  else
    printf 'gc-nudge: db=%s bytes=%s duration=%ss rc=%s — error\n' \
      "$db" "$size" "$elapsed" "$rc" >&2
  fi
  return "$rc"
}

main() {
  # Non-blocking flock so two concurrent nudges (same host:port) don't
  # double-call GC. Skip silently when held — the other nudge is handling
  # it.
  exec 9>"$lock_file"
  if ! flock -n 9; then
    printf 'gc-nudge: another nudge already running for %s:%s — skipping\n' \
      "$host" "$GC_DOLT_PORT"
    exit 0
  fi

  # Snapshot rig list once. `metadata_files` can hit the gc binary which
  # may be slow — we only want to pay that once per run.
  _meta_tmp=$(mktemp)
  trap 'rm -f "$_meta_tmp"' EXIT
  metadata_files > "$_meta_tmp"

  seen_dbs=""
  rc=0
  while IFS= read -r meta; do
    [ -n "$meta" ] || continue
    db=$(metadata_db "$meta")
    [ -n "$db" ] || continue
    # Dedup: multiple rigs may share a database.
    case " $seen_dbs " in
      *" $db "*) continue ;;
    esac
    seen_dbs="$seen_dbs $db"

    run_dolt_gc_for_db "$db" || rc=$?
  done < "$_meta_tmp"

  exit "$rc"
}

main "$@"
