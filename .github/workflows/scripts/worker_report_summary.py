#!/usr/bin/env python3

import glob
import json
import os
import sys


COUNT_KEYS = [
    ("passed", "pass"),
    ("failed", "fail"),
    ("unsupported", "unsupported"),
    ("environment_errors", "environment_error"),
    ("provider_incidents", "provider_incident"),
    ("flaky_live", "flaky_live"),
    ("not_certifiable_live", "not_certifiable_live"),
]


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: worker_report_summary.py <report-dir>", file=sys.stderr)
        return 2

    report_dir = sys.argv[1]
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY", "").strip()
    if not summary_path:
        return 0

    paths = sorted(glob.glob(os.path.join(report_dir, "*.json")))
    with open(summary_path, "a", encoding="utf-8") as out:
        out.write("### Worker Conformance Reports\n")
        if not paths:
            out.write("- No worker reports were emitted.\n")
            return 0
        for path in paths:
            with open(path, encoding="utf-8") as handle:
                report = json.load(handle)
            summary = report.get("summary", {})
            counts = format_counts(summary)
            out.write(
                f"- `{os.path.basename(path)}`: {summary.get('status', 'unknown')} "
                f"({counts})\n"
            )
            failing = summary.get("failing_requirements") or []
            if failing:
                out.write(f"  failing requirements: {', '.join(failing)}\n")
    return 0


def format_counts(summary: dict) -> str:
    parts = []
    for key, label in COUNT_KEYS:
        value = int(summary.get(key, 0) or 0)
        if value > 0 or label in {"pass", "fail", "unsupported"}:
            parts.append(f"{value} {label}")
    return " / ".join(parts)


if __name__ == "__main__":
    raise SystemExit(main())
