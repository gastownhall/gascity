#!/usr/bin/env bash
# Round-trip test for the Workbench bundle: export a fake dist, install it with a
# health check, confirm upgrade leaves a rollback, and confirm a tampered bundle
# is rejected before anything is written.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
FAKE_DIST="$ROOT/scripts/testdata/fake-workbench-dist"
FAKE_HEALTH="$ROOT/scripts/testdata/fake-workbench-health.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() { echo "test-workbench-bundle: FAIL: $*" >&2; exit 1; }

# 1. Export a checksummed bundle from the fake dist.
WORKBENCH_SRC="$FAKE_DIST" WORKBENCH_PLATFORM=test \
  "$ROOT/scripts/export-workbench-bundle.sh" "$TMP/out" >/dev/null
BUNDLE="$TMP/out/workbench-test.tar.gz"
[ -f "$BUNDLE" ] || fail "no bundle produced"
[ -f "$BUNDLE.sha256" ] || fail "no checksum produced"

# 2. Install with a health check.
DEST="$TMP/install"
WORKBENCH_HEALTH_CMD="$FAKE_HEALTH" \
  "$ROOT/scripts/install-workbench-bundle.sh" "$BUNDLE" "$DEST" >/dev/null
[ -f "$DEST/index.html" ] || fail "install missing index.html"

# 3. Re-install creates a rollback of the previous install.
WORKBENCH_HEALTH_CMD="$FAKE_HEALTH" \
  "$ROOT/scripts/install-workbench-bundle.sh" "$BUNDLE" "$DEST" >/dev/null
ls "$DEST".rollback.* >/dev/null 2>&1 || fail "no rollback created on upgrade"

# 4. A tampered bundle is rejected before any write.
cp "$BUNDLE" "$TMP/tampered.tar.gz"
cp "$BUNDLE.sha256" "$TMP/tampered.tar.gz.sha256"
printf 'x' >> "$TMP/tampered.tar.gz"
if WORKBENCH_HEALTH_CMD="$FAKE_HEALTH" \
     "$ROOT/scripts/install-workbench-bundle.sh" "$TMP/tampered.tar.gz" "$TMP/install2" >/dev/null 2>&1; then
  fail "tampered bundle was accepted"
fi
[ -d "$TMP/install2" ] && fail "tampered install wrote to disk"

# 5. A failing health check rolls back.
mkdir -p "$TMP/failhealth"
cat > "$TMP/failhealth/health.sh" <<'SH'
#!/usr/bin/env bash
exit 1
SH
chmod +x "$TMP/failhealth/health.sh"
if WORKBENCH_HEALTH_CMD="$TMP/failhealth/health.sh" \
     "$ROOT/scripts/install-workbench-bundle.sh" "$BUNDLE" "$DEST" >/dev/null 2>&1; then
  fail "install succeeded despite failing health check"
fi
[ -f "$DEST/index.html" ] || fail "rollback did not restore the previous install"

echo "test-workbench-bundle: PASS"
