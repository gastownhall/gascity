#!/bin/sh
# gc dolt cleanup — Find and remove orphaned Dolt databases.
#
# By default, lists orphaned databases (dry-run). Use --force to remove them.
# Use --max to set a safety limit (refuses if more orphans than --max).
#
# Environment: GC_CITY_PATH
set -e

force=false
max_orphans=50
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --force) force=true; shift ;;
    --max)   max_orphans="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: gc dolt cleanup [--force] [--max N]"
      echo ""
      echo "Find Dolt databases not referenced by any rig's metadata."
      echo ""
      echo "Flags:"
      echo "  --force    Actually remove orphaned databases"
      echo "  --max N    Refuse if more than N orphans (default: 50)"
      exit 0
      ;;
    *) echo "gc dolt cleanup: unknown flag: $1" >&2; exit 1 ;;
  esac
done

if [ ! -d "$data_dir" ]; then
  echo "No orphaned databases found."
  exit 0
fi

# Collect referenced database names from metadata.json files.
referenced=""
for meta in "$GC_CITY_PATH"/.beads/metadata.json "$GC_CITY_PATH"/rigs/*/.beads/metadata.json; do
  [ -f "$meta" ] || continue
  db=$(grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta" 2>/dev/null | sed 's/.*"dolt_database"[[:space:]]*:[[:space:]]*"//;s/"//' || true)
  [ -n "$db" ] && referenced="$referenced $db "
done

# Find orphans.
orphans=""
orphan_count=0
for d in "$data_dir"/*/; do
  [ ! -d "$d/.dolt" ] && continue
  name="$(basename "$d")"
  case "$name" in information_schema|mysql|dolt_cluster) continue ;; esac
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

# Print orphan table.
printf "%-30s  %s\n" "NAME" "SIZE"
echo "$orphans" | while IFS='|' read -r name size path; do
  [ -z "$name" ] && continue
  printf "%-30s  %s\n" "$name" "$size"
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

# Remove each orphan.
removed=0
echo "$orphans" | while IFS='|' read -r name size path; do
  [ -z "$name" ] && continue
  rm -rf "$path"
  echo "  Removed $name"
done

# Count removed (re-check since we're in a subshell).
removed=$(echo "$orphans" | grep -c '|' || true)
echo ""
echo "Removed $removed of $orphan_count orphaned database(s)."
