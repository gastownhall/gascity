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
GC_BEADS_BD_SCRIPT="${GC_BEADS_BD_SCRIPT:-$GC_CITY_PATH/.gc/scripts/gc-beads-bd.sh}"

if [ ! -x "$GC_BEADS_BD_SCRIPT" ]; then
  echo "gc dolt restart: gc-beads-bd not found" >&2
  exit 1
fi

case "${1:-}" in
  "")
    force_restart=false
    ;;
  --force)
    force_restart=true
    shift
    ;;
  *)
    echo "usage: gc dolt restart [--force]" >&2
    exit 64
    ;;
esac
if [ "$#" -ne 0 ]; then
  echo "usage: gc dolt restart [--force]" >&2
  exit 64
fi

if [ -n "${GC_DOLT_HOST:-}" ] && [ "$GC_DOLT_HOST" != "0.0.0.0" ]; then
  echo "gc dolt restart: not supported for remote dolt servers (set GC_DOLT_HOST=0.0.0.0 or unset to manage a local server)" >&2
  exit 1
fi

CITY_RUNTIME_DIR="${GC_CITY_RUNTIME_DIR:-$GC_CITY_PATH/.gc/runtime}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-$CITY_RUNTIME_DIR/packs/dolt}"
LOG_FILE="${GC_DOLT_LOG_FILE:-$PACK_STATE_DIR/dolt.log}"
BD_SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$GC_BEADS_BD_SCRIPT")" && pwd)"
if [ ! -f "$BD_SCRIPT_DIR/dolt-enospc.sh" ]; then
  # GC_BEADS_BD_SCRIPT may be the stable city shim; the helper ships next
  # to the real script in the bd pack (the sibling of this dolt pack).
  DOLT_PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)}"
  BD_SCRIPT_DIR="$(CDPATH= cd -- "$DOLT_PACK_DIR/../assets/scripts" 2>/dev/null && pwd || printf '%s' "$BD_SCRIPT_DIR")"
fi
. "$BD_SCRIPT_DIR/dolt-enospc.sh"

if recovery_should_skip_due_to_enospc; then
  if [ "$force_restart" != "true" ]; then
    echo "gc dolt restart: recent Dolt log shows ENOSPC; refusing restart because it can amplify recovery writes" >&2
    echo "  free disk space, then run gc dolt restart --force only if a restart is still required" >&2
    exit 1
  fi
  echo "gc dolt restart: --force set; restarting despite recent ENOSPC evidence" >&2
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

set +e
GC_CITY_PATH="$GC_CITY_PATH" "$GC_BEADS_BD_SCRIPT" start
start_rc=$?
set -e
if [ "$start_rc" -ne 0 ]; then
  echo "gc dolt restart: start failed (exit $start_rc)" >&2
  exit "$start_rc"
fi
