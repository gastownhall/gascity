#!/usr/bin/env bash
# check-eventexport-isolation.sh — guards the OSS event-export surface:
#   1. brand-free: no commercial/deployment tokens leak into the OSS projection.
#   2. one source of truth: each projection primitive is defined exactly once.
#   3. module boundary: pkg/eventexport (the published, OSS-consumable contract)
#      imports nothing from internal/.
#
# Usage: check-eventexport-isolation.sh [--deps-file FILE]
#   --deps-file names a Bazel genquery of deps(//pkg/eventexport) (the sh_test
#   //scripts:check_eventexport_isolation_test passes one) and replaces
#   `go list -deps`, which needs a module cache, for check #3.
#
# Scoped to the event-export surface so it never trips on legitimate uses of
# these tokens elsewhere in the tree (the module path, registry defaults, design
# docs, the x-gc-* API headers, etc.).
#
# Maintenance notes:
#   - BRAND below is a denylist of known commercial hosts/identifiers; EXTEND it
#     when a new one is introduced. The structural module-boundary check (#3) is
#     the airtight backstop — BRAND is the readability layer on top.
#   - The one-source check (#2) is a gofmt-dependent text match (single-line
#     'AllowedTypes = map' / 'func ...' forms). gofmt is enforced in CI, so this
#     holds; it fails safe (n!=1) if a definition is renamed or removed.
set -euo pipefail

deps_file=""
case "${1:-}" in
"") ;;
--deps-file)
  deps_file="$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"
  ;;
*)
  echo "usage: $0 [--deps-file FILE]" >&2
  exit 2
  ;;
esac

cd "$(dirname "$0")/.."

SURFACE=(
  pkg/eventexport
  internal/eventfeed
  cmd/gc/event_export.go
  internal/supervisor/config.go
)

fail() { echo "check-eventexport-isolation: FAIL: $1" >&2; exit 1; }

# 1. Brand-free. run_id/session_id are transported as JSON envelope FIELDS, never
# HTTP headers, so x-gc-* must not appear either — header transport is the
# commercial fabric that lives OUTSIDE the OSS. The 'gascity\.com' token is the
# FQDN (literal .com): test fixtures intentionally carry path-shaped
# 'gascity/...' / 'gascity-packs/...' leak-bait, which do not match it, and the
# module path github.com/gastownhall/gascity has no '.com' after 'gascity', so
# neither false-positives.
BRAND='gasworks|works\.gascity|gascity\.com|manifold|events-ingest|x-gc-'
hits=$(grep -RniE "$BRAND" "${SURFACE[@]}" 2>/dev/null || true)
if [ -n "$hits" ]; then
  echo "$hits" >&2
  fail "brand/commercial token in the OSS event-export surface (see above)"
fi

# 2. One source of truth: each projection primitive defined exactly once in
# non-test code across the surface.
for sym in 'allowedTypes = map' 'func ActorHash' 'func CityHash' 'func safeRef'; do
  files=$(grep -Rl "$sym" pkg/eventexport internal/eventfeed cmd/gc/event_export.go 2>/dev/null || true)
  n=$(printf '%s\n' "$files" | grep -v '_test.go' | grep -c . || true)
  [ "$n" = "1" ] || fail "expected exactly one definition of '$sym' in the surface, found $n"
done

# 3. Module boundary: the published package must not import internal/.
if [ -n "$deps_file" ]; then
  [ -s "$deps_file" ] || fail "deps file $deps_file is empty or missing; cannot verify the module boundary"
  if grep -q '^//internal/' "$deps_file"; then
    grep '^//internal/' "$deps_file" >&2
    fail "pkg/eventexport must import nothing from internal/ (see above)"
  fi
elif go list -deps ./pkg/eventexport 2>/dev/null | grep -q 'gastownhall/gascity/internal'; then
  go list -deps ./pkg/eventexport | grep 'gastownhall/gascity/internal' >&2
  fail "pkg/eventexport must import nothing from internal/ (see above)"
fi

echo "check-eventexport-isolation: OK (brand-free, one source of truth, pkg/eventexport internal-free)"
