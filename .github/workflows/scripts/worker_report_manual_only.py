#!/usr/bin/env python3

import json
import os
import sys


SCHEMA_VERSION = "gc.worker.conformance.v1"

CATALOG_BY_SUITE = {
    "worker-inference-phase3": [
        "WI-START-001",
        "WI-TOOL-001",
        "WI-MTURN-001",
        "WI-CONT-001",
        "WI-RESET-001",
        "WI-INT-001",
    ],
}


def main() -> int:
    if len(sys.argv) != 3:
        print(
            "usage: worker_report_manual_only.py <report-dir> <suite>",
            file=sys.stderr,
        )
        return 2

    report_dir = sys.argv[1]
    suite = sys.argv[2].strip()
    requirements = CATALOG_BY_SUITE.get(suite)
    if not requirements:
        print(f"unsupported manual-only suite: {suite!r}", file=sys.stderr)
        return 2

    profile = os.environ.get("PROFILE", "").strip() or "all-profiles"
    os.makedirs(report_dir, exist_ok=True)

    results = [
        {
            "profile": profile,
            "requirement": requirement,
            "status": "unsupported",
            "detail": "manual-only live inference is disabled in PR CI",
        }
        for requirement in requirements
    ]
    payload = {
        "schema_version": SCHEMA_VERSION,
        "run_id": f"{sanitize(suite)}-{sanitize(profile)}-manual-only",
        "suite": suite,
        "metadata": {
            "profile_filter": profile,
            "suite": suite,
            "manual_only": True,
            "synthetic": "true",
        },
        "summary": {
            "status": "unsupported",
            "total": len(results),
            "passed": 0,
            "failed": 0,
            "unsupported": len(results),
            "environment_errors": 0,
            "provider_incidents": 0,
            "flaky_live": 0,
            "not_certifiable_live": 0,
            "suite_failed": False,
            "profiles": 1,
            "requirements": len(results),
            "failing_requirements": [],
            "top_evidence": [],
        },
        "results": results,
    }
    out_path = os.path.join(
        report_dir,
        f"{sanitize(suite)}-{sanitize(profile)}-manual-only.json",
    )
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
