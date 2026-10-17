#!/usr/bin/env bash
set -euo pipefail

city_root="${GC_CITY:-${GC_CITY_PATH:-}}"
if [[ -z "$city_root" ]]; then
  city_root="$(pwd)"
fi

gc_bin="$(command -v gc || true)"
if [[ -z "$gc_bin" ]]; then
  echo "gc binary not found in PATH"
  exit 1
fi

status_file="$(mktemp)"
trap 'rm -f "$status_file"' EXIT
controller_bin=""
if gc status --json "$city_root" >"$status_file" 2>/dev/null; then
  controller_bin="$(sed -nE 's/^[[:space:]]*"binary"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' "$status_file" | head -1)"
fi
if [[ -n "$controller_bin" && -x "$controller_bin" ]]; then
  gc_bin="$controller_bin"
fi

build_stamp="$city_root/.gc/runtime/packs/beads-doltlite/last-build-gc.json"
if [[ -f "$build_stamp" ]]; then
  stamped_bin="$(sed -nE 's/^[[:space:]]*"binary_path"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' "$build_stamp" | head -1)"
  tags="$(sed -nE 's/^[[:space:]]*"tags"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' "$build_stamp" | head -1)"
  if [[ -n "$controller_bin" && -n "$stamped_bin" && "$controller_bin" != "$stamped_bin" ]]; then
    echo "running controller binary does not match doltlite build stamp"
    echo "controller: $controller_bin"
    echo "build stamp: $stamped_bin"
    exit 1
  fi
  if [[ "$tags" != *gascity_doltlite_lib* ]]; then
    echo "last-build-gc.json does not record gascity_doltlite_lib tag"
    echo "build stamp: $build_stamp"
    exit 1
  fi
fi

if command -v ldd >/dev/null 2>&1; then
  if ldd "$gc_bin" 2>/dev/null | grep -q 'libdoltlite'; then
    echo "gc links libdoltlite: $gc_bin"
    exit 0
  fi
fi

if strings "$gc_bin" 2>/dev/null | grep -q 'libdoltlite'; then
  echo "gc contains libdoltlite symbols: $gc_bin"
  exit 0
fi

echo "gc does not appear to be built against libdoltlite: $gc_bin"
echo "repair: rebuild gc with the beads-doltlite build command so ldd reports libdoltlite"
exit 1
