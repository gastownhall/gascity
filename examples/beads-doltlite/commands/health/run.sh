#!/usr/bin/env bash
# doltlite health — check doltlite database integrity and stats via bd.
set -euo pipefail

BEADS_DIR="${BEADS_DIR:-${GC_CITY_PATH:-.}/.beads}"
SCOPE_DIR="${BEADS_DIR%/.*}"

if command -v bd >/dev/null 2>&1; then
  cd "$SCOPE_DIR" || exit 1
  export BEADS_BACKEND=doltlite GC_BEADS_BACKEND=doltlite BD_NON_INTERACTIVE=1
  bd status --json 2>&1 || echo '{"ok":false,"error":"bd status failed"}'
else
  echo '{"ok":false,"error":"bd CLI not found"}'
fi
