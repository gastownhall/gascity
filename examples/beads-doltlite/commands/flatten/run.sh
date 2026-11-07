#!/usr/bin/env bash
# doltlite flatten — compact the doltlite database via bd flatten.
set -euo pipefail

BEADS_DIR="${BEADS_DIR:-${GC_CITY_PATH:-.}/.beads}"
SCOPE_DIR="${BEADS_DIR%/.*}"

if command -v bd >/dev/null 2>&1; then
  cd "$SCOPE_DIR" || exit 1
  export BEADS_BACKEND=doltlite GC_BEADS_BACKEND=doltlite BD_NON_INTERACTIVE=1
  bd flatten --force --json 2>&1 || echo '{"reclaimed_bytes":0,"error":"bd flatten failed"}'
else
  echo '{"reclaimed_bytes":0,"error":"bd CLI not found"}'
fi
