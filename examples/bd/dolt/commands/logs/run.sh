#!/bin/sh
# gc dolt logs — Tail the Dolt server log file.
#
# Usage: gc dolt logs [-n LINES] [-f]
#
# Environment: GC_CITY_PATH (set by gc pack command infrastructure)
set -e

lines=50
follow=false
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

while [ $# -gt 0 ]; do
  case "$1" in
    -n|--lines) lines="$2"; shift 2 ;;
    -n*)        lines="${1#-n}"; shift ;;
    -f|--follow) follow=true; shift ;;
    -h|--help)
      echo "Usage: gc dolt logs [-n LINES] [-f]"
      echo ""
      echo "Tail the Dolt server log file."
      echo ""
      echo "Flags:"
      echo "  -n, --lines N   Number of lines to show (default: 50)"
      echo "  -f, --follow    Follow the log in real time"
      exit 0
      ;;
    *) echo "gc dolt logs: unknown flag: $1" >&2; exit 1 ;;
  esac
done

log_file="$DOLT_LOG_FILE"

if [ ! -f "$log_file" ]; then
  echo "gc dolt logs: log file not found: $log_file" >&2
  exit 1
fi

args="-n${lines}"
if [ "$follow" = true ]; then
  args="$args -f"
fi

exec tail $args "$log_file"
