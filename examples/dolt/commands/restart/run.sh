#!/bin/sh
# gc dolt restart — Stop and start the managed Dolt server.
#
# `gc dolt start` is idempotent and no-ops when a managed dolt server is
# already running. restart is the operator escape hatch when the server
# is alive but unable to make progress — for example, a wedged process
# that keeps returning ENOSPC on writes even after disk pressure has
# cleared (see gastownhall/gascity#2158, dolthub/dolt#11068). The
# `gc dolt recover` command only handles the read-only failure mode;
# this command is the deliberate forced-restart counterpart.
#
# Environment: GC_CITY_PATH
set -e

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

if [ ! -x "$GC_BEADS_BD_SCRIPT" ]; then
  echo "gc dolt restart: gc-beads-bd not found" >&2
  exit 1
fi

# Stop. Exit 2 from gc-beads-bd stop means "nothing was running" — a
# recoverable state for restart. Any other non-zero exit is a real
# failure (e.g., couldn't kill the managed PID); abort without calling
# start so the operator can investigate.
set +e
GC_CITY_PATH="$GC_CITY_PATH" "$GC_BEADS_BD_SCRIPT" stop
stop_rc=$?
set -e
case "$stop_rc" in
  0|2) ;;
  *) echo "gc dolt restart: stop failed (exit $stop_rc)" >&2; exit "$stop_rc" ;;
esac

GC_CITY_PATH="$GC_CITY_PATH" "$GC_BEADS_BD_SCRIPT" start
