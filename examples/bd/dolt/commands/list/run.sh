#!/bin/sh
# gc dolt list — List Dolt databases with their filesystem paths.
#
# Shows databases for the HQ (city) and all configured rigs.
#
# Environment: GC_CITY_PATH
set -e

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
data_dir="$DOLT_DATA_DIR"

if [ ! -d "$data_dir" ]; then
  echo "No databases found."
  exit 0
fi

found=0
for d in "$data_dir"/*/; do
  [ ! -d "$d/.dolt" ] && continue
  name="$(basename "$d")"
  # Skip system databases.
  case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in
    information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;;
  esac
  printf "%s\t%s\n" "$name" "$d"
  found=$((found + 1))
done

if [ "$found" -eq 0 ]; then
  echo "No databases found."
fi
