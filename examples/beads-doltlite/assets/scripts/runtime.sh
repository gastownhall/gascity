#!/usr/bin/env bash
# doltlite runtime utilities — shared functions for doltlite maintenance ops.
# The doltlite backend stores data via the bd CLI with BEADS_BACKEND=doltlite.
# The actual database is at .beads/doltlite/<database>.db (SQLite with Dolt VC API).
set -euo pipefail

doltlite_scope_dir() {
  local beads_dir="${1:-${BEADS_DIR:-${GC_CITY_PATH:-.}/.beads}}"
  echo "${beads_dir%/.*}"
}

run_bd_doltlite() {
  local dir="$1"
  shift
  cd "$dir" || die "cannot enter $dir"
  export BEADS_BACKEND=doltlite GC_BEADS_BACKEND=doltlite BD_NON_INTERACTIVE=1
  bd "$@"
}
