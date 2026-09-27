#!/usr/bin/env bash
# Verify a checksummed Workbench bundle and install it for this host.
#
# Guarantees:
#   - The bundle's sha256 is verified before anything is written.
#   - The previous install is preserved for rollback (DEST.rollback.<ts>).
#   - Gas City state (Sessions, worktrees, agent state, Beads data) is untouched:
#     this only writes the static SPA under DEST.
#   - The `gc` binary is never replaced or altered.
#
# Usage: scripts/install-workbench-bundle.sh <bundle.tar.gz> [dest]
set -euo pipefail

BUNDLE=${1:?usage: install-workbench-bundle.sh <bundle.tar.gz> [dest]}
DEST=${2:-${WORKBENCH_DEST:-/home/ricky/.local/share/gascity-workbench}}
HEALTH=${WORKBENCH_HEALTH_CMD:-}

[ -f "$BUNDLE" ] || { echo "install-workbench-bundle: bundle not found: $BUNDLE" >&2; exit 1; }

# 1. Verify the checksum (fail closed).
CHECK="$BUNDLE.sha256"
[ -f "$CHECK" ] || { echo "install-workbench-bundle: missing checksum $CHECK" >&2; exit 1; }
( cd "$(dirname "$BUNDLE")" && sha256sum -c "$(basename "$CHECK")" ) \
  || { echo "install-workbench-bundle: checksum verification FAILED" >&2; exit 1; }

# 2. Stage and validate the payload before touching the live install.
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
tar -C "$STAGE" -xzf "$BUNDLE"
[ -d "$STAGE/workbench" ] || { echo "install-workbench-bundle: bundle missing workbench/" >&2; exit 1; }
[ -f "$STAGE/workbench/index.html" ] || { echo "install-workbench-bundle: bundle missing workbench/index.html" >&2; exit 1; }

# 3. Back up the previous install for rollback, then swap atomically.
mkdir -p "$(dirname "$DEST")"
ROLLBACK=""
if [ -d "$DEST" ]; then
  ROLLBACK="$DEST.rollback.$(date -u +%Y%m%dT%H%M%SZ)"
  cp -R "$DEST" "$ROLLBACK"
fi
rm -rf "$DEST"
mv "$STAGE/workbench" "$DEST"

# 4. Health check the install.
if [ -n "$HEALTH" ]; then
  if ! "$HEALTH" "$DEST"; then
    echo "install-workbench-bundle: health check failed" >&2
    if [ -n "$ROLLBACK" ]; then
      rm -rf "$DEST"; mv "$ROLLBACK" "$DEST"
      echo "install-workbench-bundle: rolled back to $DEST" >&2
    fi
    exit 1
  fi
fi

echo "install-workbench-bundle: installed $DEST"
if [ -n "$ROLLBACK" ]; then echo "install-workbench-bundle: rollback at $ROLLBACK"; fi
