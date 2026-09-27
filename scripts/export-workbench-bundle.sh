#!/usr/bin/env bash
# Package the Workbench SPA bundle into a checksummed, upgradeable tarball for
# one host platform. No public Internet service is required: the bundle is built
# from the embedded dashboardspa dist already in the repo.
#
# Usage: scripts/export-workbench-bundle.sh [out-dir]
# Env:   WORKBENCH_PLATFORM  override the platform tag (default: uname)
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=${1:-"$ROOT/dist/workbench-bundle"}
PLATFORM=${WORKBENCH_PLATFORM:-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m)}
SRC=${WORKBENCH_SRC:-"$ROOT/internal/api/dashboardspa/dist"}

[ -d "$SRC" ] || { echo "export-workbench-bundle: no SPA dist to package at $SRC" >&2; exit 1; }

mkdir -p "$OUT"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

mkdir -p "$STAGE/workbench"
cp -R "$SRC/." "$STAGE/workbench/"
cat > "$STAGE/workbench/MANIFEST" <<EOF
platform=$PLATFORM
generated_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
source=internal/api/dashboardspa/dist
EOF

TAR="$OUT/workbench-$PLATFORM.tar.gz"
tar -C "$STAGE" -czf "$TAR" workbench
( cd "$OUT" && sha256sum "$(basename "$TAR")" > "$(basename "$TAR").sha256" )

echo "export-workbench-bundle: wrote $TAR"
echo "export-workbench-bundle: wrote $TAR.sha256"
