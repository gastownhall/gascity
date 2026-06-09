#!/bin/sh
# gc dolt rollback — List or restore from migration backups.
#
# With no arguments, lists all migration backups (newest first).
# With a backup path or timestamp, restores from that backup.
# Restore is destructive and requires --force.
#
# Environment: GC_CITY_PATH
set -e

force=false
target=""

while [ $# -gt 0 ]; do
  case "$1" in
    --force) force=true; shift ;;
    -h|--help)
      echo "Usage: gc dolt rollback [PATH-OR-TIMESTAMP] [--force]"
      echo ""
      echo "List available migration backups or restore from one."
      echo ""
      echo "With no arguments, lists all migration backups (newest first)."
      echo "With a backup path or timestamp, restores from that backup."
      echo "Restore is destructive and requires --force."
      exit 0
      ;;
    *) target="$1"; shift ;;
  esac
done

city="$GC_CITY_PATH"

# List mode (no target specified).
if [ -z "$target" ]; then
  found=0
  # Find migration-backup-* directories, sorted newest first.
  for d in $(ls -1d "$city"/migration-backup-* 2>/dev/null | sort -r); do
    [ ! -d "$d" ] && continue
    ts="$(basename "$d" | sed 's/migration-backup-//')"
    if [ "$found" -eq 0 ]; then
      printf "%-20s  %s\n" "TIMESTAMP" "PATH"
    fi
    printf "%-20s  %s\n" "$ts" "$d"
    found=$((found + 1))
  done
  if [ "$found" -eq 0 ]; then
    echo "No backups found."
  fi
  exit 0
fi

# Restore mode.
if [ "$force" != true ]; then
  echo "gc dolt rollback: restore is destructive; use --force to confirm" >&2
  exit 1
fi

# Resolve target: path or timestamp.
backup_path="$target"
if [ ! -d "$backup_path" ]; then
  backup_path="$city/migration-backup-$target"
  if [ ! -d "$backup_path" ]; then
    echo "gc dolt rollback: backup not found: $target" >&2
    exit 1
  fi
fi

# Restore town beads.
if [ -d "$backup_path/town-beads" ]; then
  rm -rf "$city/.beads"
  cp -a "$backup_path/town-beads" "$city/.beads"
  echo "Restored town beads"
fi

# Restore rig beads.
for d in "$backup_path"/*-beads; do
  [ ! -d "$d" ] && continue
  name="$(basename "$d" | sed 's/-beads$//')"
  [ "$name" = "town" ] && continue
  rig_dir="$city/rigs/$name"
  if [ -d "$rig_dir" ]; then
    rm -rf "$rig_dir/.beads"
    cp -a "$d" "$rig_dir/.beads"
    echo "Restored rig: $name"
  else
    echo "Skipped rig: $name"
  fi
done
