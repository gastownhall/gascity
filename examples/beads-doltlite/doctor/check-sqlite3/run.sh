#!/usr/bin/env bash
set -euo pipefail

if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "sqlite3 is required for doltlite bead store maintenance"
  exit 1
fi

echo "sqlite3 available: $(sqlite3 --version 2>&1 | head -1)"
exit 0
