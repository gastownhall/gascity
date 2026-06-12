#!/usr/bin/env python3
"""Render the worker conformance compatibility grid from report artifacts.

Consumes the JSON reports (schema gc.worker.conformance.v1) that the
conformance suites write into GC_WORKER_REPORT_DIR and renders the
provider-compatibility grid embedded in internal/worker/builtin/README.md.

Typical regeneration flow:

    GC_WORKER_REPORT_DIR=/tmp/grid/phase1 make test-worker-core
    GC_WORKER_REPORT_DIR=/tmp/grid/phase2 make test-worker-core-phase2
    # per provider with live credentials:
    GC_WORKER_REPORT_DIR=/tmp/grid/live PROFILE=<p>/tmux-cli make test-worker-inference
    python3 scripts/worker_conformance_grid.py \
        --report-dir /tmp/grid/phase1 --report-dir /tmp/grid/phase2 \
        --report-dir /tmp/grid/live \
        --readme internal/worker/builtin/README.md

Cells render the worst status seen for the profile/requirement pair so a
mixed pass/fail re-run never masks a regression.
"""

from __future__ import annotations

import argparse
import datetime
import json
import pathlib
import sys

PROFILE_ORDER = [
    "claude/tmux-cli",
    "codex/tmux-cli",
    "gemini/tmux-cli",
    "kimi/tmux-cli",
    "opencode/tmux-cli",
    "mimocode/tmux-cli",
    "pi/tmux-cli",
    "antigravity/tmux-cli",
]

PHASE1_COLUMNS = [
    ("WC-TX-001", "Transcript discovery"),
    ("WC-TX-002", "Transcript normalization"),
    ("WC-CONT-001", "Continuation continuity"),
    ("WC-CONT-002", "Fresh-session isolation"),
]

PHASE2_GROUPS = [
    ("Bring-up", ["WC-BRINGUP-001"]),
    ("Diagnostics", ["WC-TX-003"]),
    # WC-INT-004 (instance-local dedup) is enforced by the tmux runtime leg,
    # whose reporter does not attribute results per profile; it is covered by
    # the blanket-guarantee note in the README instead of a grid column.
    (
        "Interactions",
        [
            "WC-INT-000",
            "WC-INT-001",
            "WC-INT-002",
            "WC-INT-003",
            "WC-INT-005",
            "WC-INT-006",
        ],
    ),
    ("Tool events", ["WC-TOOL-001", "WC-TOOL-002"]),
]

LIVE_COLUMNS = [
    ("Spawn + task", ["WI-START-001", "WI-TASK-001", "WI-TX-001"]),
    ("Continuation", ["WI-CONT-001"]),
    ("Fresh reset", ["WI-RESET-001"]),
    ("Workspace task", ["WI-TOOL-001"]),
    ("Multi-turn", ["WI-MTURN-001"]),
    ("Interrupt/recover", ["WI-INT-001"]),
]

# Worst-status-wins ordering for repeated profile/requirement observations.
STATUS_RANK = {"fail": 0, "environment_error": 1, "unsupported": 2, "pass": 3}
STATUS_SYMBOL = {
    "pass": "✅",
    "fail": "❌",
    "unsupported": "➖",
    "environment_error": "🔒",
    None: "🔒",
}

GRID_BEGIN = "<!-- BEGIN GENERATED: worker-conformance-grid (scripts/worker_conformance_grid.py) -->"
GRID_END = "<!-- END GENERATED: worker-conformance-grid -->"


def load_results(report_dirs):
    """Collect the worst status per (profile, requirement) across reports."""
    cells = {}
    runs = 0
    for report_dir in report_dirs:
        for path in sorted(pathlib.Path(report_dir).glob("*.json")):
            try:
                report = json.loads(path.read_text())
            except (OSError, json.JSONDecodeError) as err:
                print(f"skipping {path}: {err}", file=sys.stderr)
                continue
            if report.get("schema_version") != "gc.worker.conformance.v1":
                continue
            runs += 1
            for result in report.get("results", []):
                key = (result.get("profile"), result.get("requirement"))
                status = result.get("status")
                if status not in STATUS_RANK:
                    continue
                current = cells.get(key)
                if current is None or STATUS_RANK[status] < STATUS_RANK[current]:
                    cells[key] = status
    return cells, runs


def provider_name(profile):
    return profile.split("/", 1)[0]


def cell_for(cells, profile, requirements):
    """Aggregate one rendered cell from one or more requirement codes."""
    statuses = [cells.get((profile, code)) for code in requirements]
    seen = [s for s in statuses if s is not None]
    if not seen:
        return STATUS_SYMBOL[None]
    worst = min(seen, key=lambda s: STATUS_RANK[s])
    if worst == "pass" and len(seen) < len(requirements):
        return "⚠️"
    return STATUS_SYMBOL[worst]


def render_table(cells, columns):
    header = "| Provider | " + " | ".join(label for label, _ in columns) + " |"
    rule = "|---" * (len(columns) + 1) + "|"
    rows = [header, rule]
    for profile in PROFILE_ORDER:
        row = [f"`{provider_name(profile)}`"]
        for _, requirements in columns:
            row.append(cell_for(cells, profile, requirements))
        rows.append("| " + " | ".join(row) + " |")
    return "\n".join(rows)


def render_grid(cells, runs, generated_on):
    phase1 = [(label, [code]) for code, label in PHASE1_COLUMNS]
    sections = [
        GRID_BEGIN,
        f"_Generated {generated_on} from {runs} conformance report(s)._",
        "",
        "### Phase 1 — transcript & continuation contract (deterministic fixtures)",
        "",
        render_table(cells, phase1),
        "",
        "### Phase 2 — runtime substrate (deterministic, fake transport)",
        "",
        render_table(cells, PHASE2_GROUPS),
        "",
        "### Phase 3 — live inference proofs (real provider CLI + real models)",
        "",
        render_table(cells, LIVE_COLUMNS),
        GRID_END,
    ]
    return "\n".join(sections)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report-dir", action="append", required=True, dest="report_dirs")
    parser.add_argument("--readme", help="README to update in place between grid markers")
    parser.add_argument(
        "--generated-on",
        default=datetime.date.today().isoformat(),
        help="date stamp for the grid header (defaults to today)",
    )
    args = parser.parse_args()

    cells, runs = load_results(args.report_dirs)
    if not cells:
        print("no conformance results found", file=sys.stderr)
        return 1
    grid = render_grid(cells, runs, args.generated_on)

    if not args.readme:
        print(grid)
        return 0

    readme = pathlib.Path(args.readme)
    text = readme.read_text()
    begin = text.find(GRID_BEGIN)
    end = text.find(GRID_END)
    if begin == -1 or end == -1:
        print(f"{args.readme}: grid markers not found", file=sys.stderr)
        return 1
    updated = text[:begin] + grid + text[end + len(GRID_END):]
    readme.write_text(updated)
    print(f"updated {args.readme}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
