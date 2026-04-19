#!/bin/sh
# gc dolt recover — Check for and recover from Dolt read-only state.
#
# Dolt can enter read-only mode after certain failures. This command
# detects the condition and attempts automatic recovery by calling
# the gc-beads-bd recover operation.
#
# Environment: GC_CITY_PATH, GC_DOLT_HOST, GC_DOLT_PORT, GC_DOLT_USER,
#              GC_DOLT_PASSWORD
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

beads_bd="$GC_BEADS_BD_SCRIPT"

# Reject remote servers — can't manage remote dolt processes.
if [ -n "$GC_DOLT_HOST" ]; then
  case "$GC_DOLT_HOST" in
    127.0.0.1|localhost|"::1"|"[::1]") ;; # local is fine
    *) echo "gc dolt recover: not supported for remote dolt servers" >&2; exit 1 ;;
  esac
fi

# Check read-only state by attempting a write probe.
#
# Always export DOLT_CLI_PASSWORD (even empty) so the client does not
# prompt for a password on stdin; non-TTY agent sessions would otherwise
# fail with "Failed to parse credentials: operation not supported by
# device" and the probe would falsely report writable. The write probe
# is wrapped in run_bounded so an unresponsive server — the very
# failure mode `gc dolt recover` exists to handle — cannot hang the
# script indefinitely. Mirrors the patterns established in health/run.sh.
check_read_only() {
  host="${GC_DOLT_HOST:-127.0.0.1}"
  args="--host $host --port $GC_DOLT_PORT --user $GC_DOLT_USER --no-tls"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  result=$(run_bounded 10 dolt $args sql -q "CREATE TABLE __gc_ro_check (id INT); DROP TABLE __gc_ro_check;" 2>&1) || true
  case "$result" in
    *read*only*|*read-only*|*readonly*) return 0 ;; # read-only detected
  esac
  return 1 # writable
}

if ! check_read_only; then
  echo "Dolt server is not in read-only state."
  exit 0
fi

echo "Dolt server is in read-only state. Attempting recovery..."

if [ -x "$beads_bd" ]; then
  "$beads_bd" recover || {
    echo "gc dolt recover: recovery failed" >&2
    exit 1
  }
else
  echo "gc dolt recover: gc-beads-bd script not found at $beads_bd" >&2
  exit 1
fi

echo "Recovery successful."
