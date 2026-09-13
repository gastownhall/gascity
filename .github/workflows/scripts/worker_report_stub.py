#!/usr/bin/env python3

import glob
import json
import os
import sys


SCHEMA_VERSION = "gc.worker.conformance.v1"


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: worker_report_stub.py <report-dir> <suite>", file=sys.stderr)
        return 2

    report_dir = sys.argv[1]
    suite = sys.argv[2]
    os.makedirs(report_dir, exist_ok=True)

    paths = sorted(glob.glob(os.path.join(report_dir, "*.json")))
    for path in paths:
        with open(path, encoding="utf-8") as handle:
            report = json.load(handle)
        summary = report.get("summary", {})
        if summary.get("status") != "pass" or summary.get("suite_failed"):
            return 0

    profile = os.environ.get("PROFILE", "").strip() or "all-profiles"
    payload = {
        "schema_version": SCHEMA_VERSION,
        "run_id": f"{sanitize(suite)}-{sanitize(profile)}-job-failure",
        "suite": suite,
        "metadata": {
            "profile_filter": profile,
            "suite": suite,
            "synthetic": "true",
        },
        "summary": {
            "status": "fail",
            "total": 0,
            "passed": 0,
            "failed": 0,
            "unsupported": 0,
            "suite_failed": True,
            "failure_detail": "job failed outside emitted conformance reports",
            "profiles": 0,
            "requirements": 0,
        },
        "results": [],
    }
    out_path = os.path.join(report_dir, f"{sanitize(suite)}-{sanitize(profile)}-job-failure.json")
    with open(out_path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2)
        handle.write("\n")
    return 0


def sanitize(value: str) -> str:
    value = value.strip().lower()
    if not value:
        return "unknown"
    out = []
    last_dash = False
    for ch in value:
        if ch.isalnum():
            out.append(ch)
            last_dash = False
        elif not last_dash:
            out.append("-")
            last_dash = True
    return "".join(out).strip("-") or "unknown"


if __name__ == "__main__":
    raise SystemExit(main())
