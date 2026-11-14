#!/usr/bin/env bash
# doltlite gc — remove deleted beads via bd gc.
set -euo pipefail

BEADS_DIR="${BEADS_DIR:-${GC_CITY_PATH:-.}/.beads}"
SCOPE_DIR="${BEADS_DIR%/.*}"

if command -v bd >/dev/null 2>&1; then
  cd "$SCOPE_DIR" || exit 1
  export BEADS_BACKEND=doltlite GC_BEADS_BACKEND=doltlite BD_NON_INTERACTIVE=1
  bd gc --skip-decay --force --json 2>&1 || echo '{"deleted_beads_removed":0,"error":"bd gc failed"}'
else
  echo '{"deleted_beads_removed":0,"error":"bd CLI not found"}'
fi
