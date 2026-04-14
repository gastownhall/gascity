#!/bin/sh
# gc dolt sync — Push Dolt databases to their configured remotes.
#
# Stops the server for a clean push, syncs each database, then restarts.
# Use --gc to purge closed ephemeral beads before syncing.
# Use --dry-run to preview without pushing.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_USER, GC_DOLT_PASSWORD
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/scripts/runtime.sh"

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

# Check if server is running.
is_running() {
  lsof -i :"$GC_DOLT_PORT" -sTCP:LISTEN >/dev/null 2>&1
}

# routes_files — emit one routes.jsonl path per line.
# Uses gc rig list --json when available so external rigs are included.
# Falls back to a filesystem glob when gc is absent.
routes_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/routes.jsonl"

  if command -v gc >/dev/null 2>&1; then
    rig_paths=$(gc rig list --json --city "$GC_CITY_PATH" 2>/dev/null \
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

# Optional GC phase: purge closed ephemerals while server is still up.
if [ "$do_gc" = true ] && [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$name" in information_schema|mysql|dolt_cluster) continue ;; esac
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

# Stop server for clean push.
was_running=false
if is_running; then
  was_running=true
  if [ "$dry_run" = false ] && [ -x "$beads_bd" ]; then
    "$beads_bd" stop 2>/dev/null || true
    "$beads_bd" shutdown 2>/dev/null || true
  fi
fi

# Sync each database.
exit_code=0
if [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$name" in information_schema|mysql|dolt_cluster) continue ;; esac
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue

    # Check for remote.
    remote=""
    if [ -f "$d/.dolt/remotes.json" ]; then
      remote=$(grep -o '"url":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"url":"//;s/"//' || true)
    fi

    if [ -z "$remote" ]; then
      echo "  $name: skipped (no remote)"
      continue
    fi

    if [ "$dry_run" = true ]; then
      echo "  $name: would push to $remote"
      continue
    fi

    push_args="push"
    [ "$force" = true ] && push_args="push --force"

    if (cd "$d" && dolt $push_args 2>&1); then
      echo "  $name: pushed to $remote"
    else
      echo "  $name: ERROR: push failed" >&2
      exit_code=1
    fi
  done
fi

# Restart server if it was running.
if [ "$was_running" = true ] && [ "$dry_run" = false ] && [ -x "$beads_bd" ]; then
  "$beads_bd" start 2>/dev/null || true
  "$beads_bd" ensure-ready 2>/dev/null || true
fi

exit $exit_code
