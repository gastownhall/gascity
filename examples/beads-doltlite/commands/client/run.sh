#!/usr/bin/env bash
# Run the DoltLite debug client built by `gc beads-doltlite build client`.
set -euo pipefail

die() {
  echo "$*" >&2
  exit 1
}

has_doltlite_lib() {
  [ -r "$1/libdoltlite.so" ] || [ -r "$1/libdoltlite.so.0" ] || [ -r "$1/libdoltlite.dylib" ]
}

find_doltlite_lib() {
  for candidate in \
    "$CITY_ROOT/doltlite-work/build" \
    "$CITY_ROOT/doltlite/build" \
    "$CITY_ROOT/../doltlite-work/build" \
    "$CITY_ROOT/../doltlite/build"; do
    if has_doltlite_lib "$candidate"; then
      cd "$candidate" && pwd
      return 0
    fi
  done
  return 1
}

CITY_ROOT="${GC_CITY_PATH:-${GC_CITY:-$(pwd)}}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PACK_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
SOURCE_CITY_ROOT="$(cd "$PACK_DIR/../../.." && pwd)"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-$CITY_ROOT/.gc/runtime/packs/beads-doltlite}"
CLIENT_BIN="${GC_DOLTLITE_CLIENT_BIN:-}"
DOLTLITE_LIB="${DOLTLITE_LIB:-${GC_DOLTLITE_LIB:-}}"

if [ -z "$CLIENT_BIN" ]; then
  for candidate in \
    "$PACK_STATE_DIR/bin/doltlite-client" \
    "$CITY_ROOT/.gc/runtime/packs/beads-doltlite/bin/doltlite-client" \
    "$PACK_DIR/bin/doltlite-client"; do
    if [ -x "$candidate" ]; then
      CLIENT_BIN="$candidate"
      break
    fi
  done
fi

if [ ! -x "$CLIENT_BIN" ]; then
  die "doltlite-client is not built; run: gc beads-doltlite build client"
fi

if [ -z "$DOLTLITE_LIB" ]; then
  DOLTLITE_LIB="$(find_doltlite_lib || true)"
fi
if [ -z "$DOLTLITE_LIB" ] || ! has_doltlite_lib "$DOLTLITE_LIB"; then
  die "could not find libdoltlite; set DOLTLITE_LIB=/path/to/doltlite/build"
fi

export LD_LIBRARY_PATH="$DOLTLITE_LIB${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
exec "$CLIENT_BIN" -city "$CITY_ROOT" "$@"
