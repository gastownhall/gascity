#!/usr/bin/env bash
# Report and (optionally) guard cyclomatic complexity for shipped Go code.
#
# This experiment is advisory by default. Existing complexity is captured in a
# checked-in baseline; `check` fails only for a new or regressed function.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BASELINE="${COMPLEXITY_BASELINE:-$REPO_ROOT/engdocs/complexity-baseline.json}"
THRESHOLD="${COMPLEXITY_THRESHOLD:-20}"
TOP="${COMPLEXITY_TOP:-50}"
TOOL="${COMPLEXITY_TOOL:-gocyclo}"
BASE_REF="${COMPLEXITY_BASE_REF:-origin/main}"
MODE="${1:-report}"

case "$MODE" in report|diff|check|update) ;; *) echo "usage: $0 [report|diff|check|update]" >&2; exit 2 ;; esac
case "$THRESHOLD" in ''|*[!0-9]*) echo "complexity: COMPLEXITY_THRESHOLD must be an integer" >&2; exit 2 ;; esac
case "$TOP" in ''|*[!0-9]*) echo "complexity: COMPLEXITY_TOP must be an integer" >&2; exit 2 ;; esac
if ! command -v "$TOOL" >/dev/null 2>&1; then
	printf 'complexity: %s is required (go install github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0)\n' "$TOOL" >&2
	exit 2
fi

cd "$REPO_ROOT"
current=$(mktemp)
base_current=$(mktemp)
base_dir=""
trap 'rm -f "$current" "$base_current"; [ -z "$base_dir" ] || rm -rf "$base_dir"' EXIT
# Keep this allowlist explicit: examples, test harnesses, and code generators
# are not shipped library/runtime code and would distort the signal.
scan() {
    local root="$1" output="$2"
    (cd "$root" && COMPLEXITY_SCAN_KIND="${COMPLEXITY_SCAN_KIND:-head}" "$TOOL" -ignore '(_test\.go$|_testhook\.go$|_gen\.go$|(^|/)(genclient|dashboardspa|conformance|testdata|fixtures|generated|testutil|testpolicy|[[:alnum:]_-]*test)(/|$)|(^|/)[^/]*conformance[^/]*\.go$)' ./cmd/gc ./internal ./pkg) >"$output"
}
export COMPLEXITY_SCAN_KIND=head
export COMPLEXITY_MODE="$MODE"
scan "$REPO_ROOT" "$current"

if [[ "$MODE" == diff ]]; then
    base_dir=$(mktemp -d)
    if ! git archive --format=tar "$BASE_REF" | tar -x -C "$base_dir"; then
        echo "complexity: unable to archive base ref $BASE_REF" >&2
        exit 2
    fi
    COMPLEXITY_SCAN_KIND=base scan "$base_dir" "$base_current"
fi

python3 - "$MODE" "$current" "$BASELINE" "$THRESHOLD" "$TOP" "$base_current" "$BASE_REF" <<'PY'
import json, os, pathlib, sys

mode, current_path, baseline_path, threshold, top, base_path, base_ref = sys.argv[1:]
threshold, top = int(threshold), int(top)
items = []
for raw in pathlib.Path(current_path).read_text().splitlines():
    fields = raw.split(None, 3)
    if len(fields) != 4:
        continue
    try:
        ccn = int(fields[0])
    except ValueError:
        continue
    package, function, location = fields[1:]
    file = location.rsplit(":", 2)[0]
    # Keep a defensive filter here as well as gocyclo's -ignore expression;
    # this prevents a tool-version change from pulling non-shipped trees into
    # the baseline.
    path_parts = file.split("/")
    if (file.endswith("_test.go") or file.endswith("_testhook.go") or file.endswith("_gen.go") or file.endswith(".generated.go") or "conformance" in file.lower() or
            any(part in ("genclient", "dashboardspa", "testdata", "fixtures", "generated", "testutil", "testpolicy") or part.lower().endswith("test") for part in path_parts)):
        continue
    items.append({"package": package, "function": function, "file": file, "ccn": ccn})
items.sort(key=lambda x: (-x["ccn"], x["package"], x["function"], x["file"]))
report_items = [item for item in items if item["ccn"] >= threshold]

if mode == "update":
    payload = {"schema": "gascity.complexity/v1", "tool": "gocyclo@v0.6.0", "threshold": threshold, "items": report_items}
    pathlib.Path(baseline_path).parent.mkdir(parents=True, exist_ok=True)
    pathlib.Path(baseline_path).write_text(json.dumps(payload, indent=2) + "\n")
    print(f"complexity: wrote {baseline_path} ({len(report_items)} functions)")
    raise SystemExit(0)

if mode == "report":
    print(f"Cyclomatic complexity (threshold >= {threshold}; shipped Go production code)")
    for item in report_items[:top]:
        print(f"{item['ccn']:>3} {item['package']} {item['function']} {item['file']}")
    if not report_items:
        print("(no functions meet threshold)")
    raise SystemExit(0)

old = {}
if mode != "diff":
    if not os.path.exists(baseline_path):
        print(f"complexity: baseline not found: {baseline_path} (run '$0 update')", file=sys.stderr)
        raise SystemExit(1)
    try:
        baseline = json.loads(pathlib.Path(baseline_path).read_text())
    except (OSError, json.JSONDecodeError) as exc:
        print(f"complexity: invalid baseline {baseline_path}: {exc}", file=sys.stderr)
        raise SystemExit(2)
    if baseline.get("schema") != "gascity.complexity/v1" or baseline.get("tool") != "gocyclo@v0.6.0":
        print(f"complexity: invalid baseline schema/tool in {baseline_path}", file=sys.stderr)
        raise SystemExit(2)
    for x in baseline.get("items", []):
        key = (x["package"], x["function"], x["file"])
        if key in old:
            print(f"complexity: duplicate baseline key {key}", file=sys.stderr)
            raise SystemExit(2)
        old[key] = x["ccn"]

if mode == "diff":
    def parse(path):
        out = []
        for raw in pathlib.Path(path).read_text().splitlines():
            fields = raw.split(None, 3)
            if len(fields) != 4:
                continue
            try: ccn = int(fields[0])
            except ValueError: continue
            package, function, location = fields[1:]
            file = location.rsplit(":", 2)[0]
            if file.endswith(("_test.go", "_testhook.go", "_gen.go", ".generated.go")) or "conformance" in file.lower(): continue
            parts = file.split("/")
            if any(part in ("genclient", "dashboardspa", "testdata", "fixtures", "generated", "testutil", "testpolicy") or part.lower().endswith("test") for part in parts): continue
            out.append({"package": package, "function": function, "file": file, "ccn": ccn})
        result = {}
        for x in out:
            key = (x["package"], x["function"], x["file"])
            if key in result:
                # gocyclo can emit duplicate low-complexity init rows. They
                # are irrelevant to the guard; duplicate tracked offenders
                # remain an error so the diff cannot be ambiguous.
                if result[key] >= threshold or x["ccn"] >= threshold:
                    print(f"complexity: duplicate analyzer key {key} in {path}", file=sys.stderr)
                    raise SystemExit(2)
                continue
            result[key] = x["ccn"]
        return result
    head, base = parse(current_path), parse(base_path)
    changes = []
    for key in sorted(set(head) | set(base)):
        h, b = head.get(key), base.get(key)
        if h is not None and b is None and h >= threshold: changes.append(f"new: {h} {' '.join(key)}")
        elif h is not None and b is not None and h > b and h >= threshold: changes.append(f"regressed: {h} {' '.join(key)} (base {b})")
        elif h is not None and b is not None and h < b and b >= threshold: changes.append(f"improved: {h} {' '.join(key)} (base {b})")
        elif h is None and b >= threshold: changes.append(f"deleted: {b} {' '.join(key)}")
    print(f"complexity: diff against {base_ref}")
    print("\n".join(changes) if changes else "complexity: no threshold changes")
    raise SystemExit(0)

changes = []
for item in items:
    key = (item["package"], item["function"], item["file"])
    # The baseline captures threshold offenders; the guard remains focused on
    # meaningful entries at/above the report threshold.
    if item["ccn"] < threshold:
        continue
    if key not in old:
        changes.append(f"new: {item['ccn']} {item['package']} {item['function']} {item['file']}")
    elif item["ccn"] > old[key]:
        changes.append(f"regressed: {item['ccn']} {item['package']} {item['function']} {item['file']} (baseline {old[key]})")
if changes:
    print("complexity: changes relative to baseline:", file=sys.stderr)
    print("\n".join(changes), file=sys.stderr)
else:
    print("complexity: no regressions relative to baseline")
if mode == "check" and changes:
    raise SystemExit(1)
PY
