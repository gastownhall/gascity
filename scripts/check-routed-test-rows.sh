#!/usr/bin/env bash
# check-routed-test-rows.sh
#
# Drift-prevention lint for the read-path six-row matrix defined by the
# enabler bead ga-71l (parent ga-h6w):
#
#     api-happy-path       — API path, 2xx, exit 0, route=api
#     api-cache-not-live   — API 503 "cache_not_live:", fallback, exit 0
#     api-500-fallback     — API 5xx, fallback, exit 0
#     api-404-error        — API 404, no fallback, exit 1
#     controller-down      — apiClient returns nil, fallback, exit 0
#     escape-hatch         — GC_NO_API=1, fallback, exit 0
#
# Semantics: a test file that contains ANY of the six rows MUST contain
# ALL six. This keeps per-file read-path migrations from regressing back
# below six rows as handlers evolve, without forcing the rows onto
# pre-existing mutation-routed tests that were never part of the
# read-path migration.
#
# Exits non-zero when any partially-covered test file is found,
# printing each violation. Passes silently when all test files with
# matrix rows are fully covered or when no read-path migrations have
# landed yet.

set -euo pipefail

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cmd_dir="$repo_root/cmd/gc"

required_rows=(
    "api-happy-path"
    "api-cache-not-live"
    "api-500-fallback"
    "api-404-error"
    "controller-down"
    "escape-hatch"
)

violations=0

shopt -s nullglob
for test_file in "$cmd_dir"/cmd_*_test.go; do
    present=0
    missing=()
    for row in "${required_rows[@]}"; do
        if grep -Fq "$row" "$test_file"; then
            present=$((present + 1))
        else
            missing+=("$row")
        fi
    done

    # 0 rows present: not a read-path migrated file (or migration hasn't
    # landed yet). Don't police it here.
    if (( present == 0 )); then
        continue
    fi
    # 6 rows present: fully covered.
    if (( present == 6 )); then
        continue
    fi
    # Partial coverage is always a violation.
    echo "INCOMPLETE: $test_file missing rows: ${missing[*]}"
    violations=$((violations + ${#missing[@]}))
done

if (( violations > 0 )); then
    echo "---"
    echo "Six-row matrix violations: $violations"
    echo "A test file with any six-row marker MUST contain all six."
    echo "See docs/plans/ga-h6w-read-path-api-routing.md."
    exit 1
fi

exit 0
