#!/usr/bin/env bash
# check-generated-docs-drift.sh - regenerate the genschema reference docs and
# fail on drift, leaving the exact regeneration patch for the docs-autofix
# workflow to apply (see .github/workflows/docs-autofix.yml).
#
# The generated set is exactly what cmd/genschema writes; keep GEN_PATHS in
# sync with cmd/genschema/main.go and with the path allowlist in
# scripts/docs-autofix-push.sh.
#
# Outputs:
#   generated-docs-freshness.patch (override with PATCH_OUT) - written only
#   when drift is detected; removed when the docs are fresh.
#
# Exit 0 when fresh, 1 on drift, so CI fails the step and the autofix
# workflow can pick up the patch artifact.

set -euo pipefail

PATCH_OUT="${PATCH_OUT:-generated-docs-freshness.patch}"

GEN_PATHS=(
    docs/reference/cli.md
    docs/reference/config.md
    docs/reference/schema/city-schema.json
    docs/reference/schema/city-schema.txt
    docs/reference/schema/pack-schema.json
    docs/reference/schema/pack-schema.txt
)

# CGO off: genschema is pure Go, and the transitive dolt ICU dependency
# fails to compile on hosts without ICU headers (mirrors the pure-Go build
# the beads pipeline uses for the same reason).
CGO_ENABLED=0 go run ./cmd/genschema

if git diff --quiet -- "${GEN_PATHS[@]}"; then
    echo "Generated reference docs are fresh."
    rm -f "$PATCH_OUT"
    exit 0
fi

git diff -- "${GEN_PATHS[@]}" > "$PATCH_OUT"
echo "Generated reference docs are STALE; regeneration patch written to $PATCH_OUT:"
git diff --stat -- "${GEN_PATHS[@]}"
echo "Fix locally with: make generate && git commit -- ${GEN_PATHS[*]}"
exit 1
