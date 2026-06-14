#!/bin/sh
# gc dolt sql — Open a Dolt SQL shell or run a one-shot query.
#
# Connects to the running Dolt server if available, otherwise opens
# in embedded mode using the first database directory found. Trailing
# arguments are forwarded verbatim to `dolt sql`, so non-interactive
# use is supported via `gc dolt sql -q "QUERY"`.
#
# Environment: GC_CITY_PATH, GC_DOLT_HOST, GC_DOLT_PORT, GC_DOLT_USER,
#              GC_DOLT_PASSWORD (all optional except GC_CITY_PATH)
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
data_dir="$DOLT_DATA_DIR"

# Check if the server is reachable.
is_running() {
  if [ -n "$GC_DOLT_HOST" ]; then
    # Remote server — TCP probe.
    (echo > /dev/tcp/"$GC_DOLT_HOST"/"$GC_DOLT_PORT") 2>/dev/null && return 0
    # Fallback: nc/ncat.
    command -v nc >/dev/null 2>&1 && nc -z "$GC_DOLT_HOST" "$GC_DOLT_PORT" 2>/dev/null && return 0
    return 1
  fi
  managed_runtime_tcp_reachable "$GC_DOLT_PORT"
}

if is_running; then
  # Build connection args.
  args=""
  if [ -n "$GC_DOLT_HOST" ]; then
    host="$GC_DOLT_HOST"
  else
    host="127.0.0.1"
  fi
  args="--host $host --port $GC_DOLT_PORT --user $GC_DOLT_USER --no-tls"
  # Always export DOLT_CLI_PASSWORD so dolt's credential parser skips
  # the TTY password prompt. When GC_DOLT_PASSWORD is empty (the
  # managed-local default — root has no password), an unset env var
  # causes `dolt sql -q "..."` to fail with "inappropriate ioctl for
  # device" under non-interactive callers (CI, scripts, automation).
  # Exporting empty satisfies dolt without changing auth outcomes.
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  exec dolt $args sql "$@"
else
  # Embedded mode — find first database directory.
  if [ ! -d "$data_dir" ]; then
    echo "gc dolt sql: no dolt server running and no databases found" >&2
    exit 1
  fi
  first_db=""
  for d in "$data_dir"/*/; do
    [ -d "$d/.dolt" ] && first_db="$d" && break
  done
  if [ -z "$first_db" ]; then
    echo "gc dolt sql: no dolt server running and no databases found" >&2
    exit 1
  fi
  exec dolt --data-dir "$data_dir" sql "$@"
fi
