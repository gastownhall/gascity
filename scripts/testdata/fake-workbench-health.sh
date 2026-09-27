#!/usr/bin/env bash
# Fake Workbench health check for bundle tests: succeeds when the install has an
# index.html, fails otherwise. Mirrors the real health contract (a shell command
# handed the install dir).
set -euo pipefail
dest=${1:?usage: fake-workbench-health.sh <dest>}
[ -f "$dest/index.html" ] || { echo "fake-workbench-health: no index.html in $dest" >&2; exit 1; }
echo "fake-workbench-health: ok"
