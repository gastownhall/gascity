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
# Semantics (two layers):
#
#   1. MANIFEST (scripts/routed-test-rows.manifest) — a checked-in list of the
#      files that MUST carry all six rows. Every manifest file must exist and be
#      fully covered. This is a NON-EMPTY denominator: without it, renaming a
#      row marker would drop every file to "0 rows found" and the old
#      any-then-all rule would pass vacuously, silently disabling the guard.
#      With it, the same rename makes every manifest file report <6 and fail.
#
#   2. DISCOVERY — any cmd_*_test.go NOT in the manifest that carries at least
#      one row must carry all six (a partially-migrated file is a bug), and a
#      fully-covered file missing from the manifest must be added (keeps the
#      manifest current so layer 1 keeps policing it).
#
# Exits non-zero on any violation; passes silently when the manifest files are
# fully covered and no unlisted file is partial or fully-covered-but-unlisted.

set -euo pipefail

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cmd_dir="$repo_root/cmd/gc"
manifest="$repo_root/scripts/routed-test-rows.manifest"

required_rows=(
    "api-happy-path"
    "api-cache-not-live"
    "api-500-fallback"
    "api-404-error"
    "controller-down"
    "escape-hatch"
)

# count_rows FILE -> echoes how many of the six required rows the file contains.
count_rows() {
    local file="$1" present=0 row
    for row in "${required_rows[@]}"; do
        if grep -Fq "$row" "$file"; then
            present=$((present + 1))
        fi
    done
    echo "$present"
}

violations=0

# --- Layer 1: the manifest is the non-empty policed denominator. ---
if [[ ! -f "$manifest" ]]; then
    echo "ERROR: manifest missing: $manifest"
    exit 1
fi

manifest_files=()
while IFS= read -r line; do
    line="${line%%#*}"                       # strip trailing comment
    line="${line#"${line%%[![:space:]]*}"}"  # ltrim
    line="${line%"${line##*[![:space:]]}"}"  # rtrim
    [[ -z "$line" ]] && continue
    manifest_files+=("$line")
done < "$manifest"

if (( ${#manifest_files[@]} == 0 )); then
    echo "ERROR: $manifest lists no files — the six-row guard would police nothing."
    exit 1
fi

declare -A in_manifest=()
for rel in "${manifest_files[@]}"; do
    in_manifest["$rel"]=1
    f="$repo_root/$rel"
    if [[ ! -f "$f" ]]; then
        echo "MANIFEST FILE MISSING: $rel (listed in the manifest but not on disk)"
        violations=$((violations + 1))
        continue
    fi
    present=$(count_rows "$f")
    if (( present != 6 )); then
        echo "MANIFEST FILE UNDER-COVERED: $rel has $present/6 rows (a marker rename or a dropped row?)"
        violations=$((violations + 1))
    fi
done

# --- Layer 2: discovery over the rest of cmd_*_test.go. ---
shopt -s nullglob
for test_file in "$cmd_dir"/cmd_*_test.go; do
    rel="cmd/gc/$(basename "$test_file")"
    [[ -n "${in_manifest[$rel]:-}" ]] && continue
    present=$(count_rows "$test_file")
    if (( present == 0 )); then
        continue
    elif (( present == 6 )); then
        echo "ADD TO MANIFEST: $rel is fully six-row-covered but not listed in the manifest"
        violations=$((violations + 1))
    else
        echo "INCOMPLETE: $rel has $present/6 rows — a matrix file must contain all six"
        violations=$((violations + 1))
    fi
done

if (( violations > 0 )); then
    echo "---"
    echo "Six-row matrix violations: $violations"
    echo "A matrix test file MUST contain all six rows and be listed in scripts/routed-test-rows.manifest."
    echo "See docs/plans/ga-h6w-read-path-api-routing.md."
    exit 1
fi

exit 0
