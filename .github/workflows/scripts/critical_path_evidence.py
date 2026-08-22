"""Evaluate whether matched critical-path categories have successful evidence.

ci-required's allow_skipped list exists so genuinely path-gated jobs can
report "skipped" without failing the merge -- that is correct when the
`changes` job did not match their category. It is silently wrong when the
category *did* match: a matched-but-skipped (or failed, cancelled, or
altogether absent) result means the suite never actually produced evidence,
and ci-required's allow_skipped list cannot tell the difference.

This module is the runtime logic behind the `critical-path-evidence` CI job:
it cross-references each critical-path category's `changes` output against
its mapped gate job's `needs.<job>.result` and reports a failure for every
category that matched without a successful result. An unmatched category is
always fine, regardless of what its job reports (a manual full-suite run
should not be penalized for running jobs that would otherwise be skipped).

Usage (invoked from .github/workflows/ci.yml):
    CHANGES_JSON='{"cmd_gc_process": "true", ...}' \
    NEEDS_JSON='{"cmd-gc-process": {"result": "success"}, ...}' \
    python3 .github/workflows/scripts/critical_path_evidence.py
"""

from __future__ import annotations

import json
import os
import sys

# Category name (a `changes` job output) -> the gate job whose result proves
# that category's suite actually ran and passed. Kept in sync with the
# critical-path-evidence job's `needs:` list in .github/workflows/ci.yml and
# with scripts/cipolicy/critical_path.go's static wiring check.
CRITICAL_PATH_JOBS = {
    "cmd_gc_process": "cmd-gc-process",
    "integration": "integration-shards",
    "worker": "worker-core-summary",
    "worker_phase2": "worker-core-phase2-summary",
    "packs": "pack-gate",
    "docker": "docker-session",
    "k8s": "k8s-session",
    "openclaw_bridge": "openclaw-bridge",
}


def evaluate(changes, needs):
    """Evaluate every critical-path category against its job's result.

    ``changes`` maps category name -> the `changes` job's boolean-string
    output; a missing key defaults to unmatched. ``needs`` maps job name ->
    {"result": <github result string>}, as produced by GitHub Actions'
    `toJSON(needs)`; a job missing from ``needs`` reports result "absent".

    Returns rows sorted by category name, each with "category", "job",
    "matched", "result", and "ok" keys.
    """
    rows = []
    for category in sorted(CRITICAL_PATH_JOBS):
        job = CRITICAL_PATH_JOBS[category]
        matched = changes.get(category) == "true"
        result = needs.get(job, {}).get("result", "absent")
        ok = (not matched) or result == "success"
        rows.append(
            {
                "category": category,
                "job": job,
                "matched": matched,
                "result": result,
                "ok": ok,
            }
        )
    return rows


def failures(rows):
    """Render one human-readable failure message per not-ok row."""
    return [
        f"{row['category']} ({row['job']}): expected success, got {row['result']}"
        for row in rows
        if not row["ok"]
    ]


def _write_step_summary(rows):
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not summary_path:
        return
    lines = [
        "## Critical Path Evidence",
        "",
        "| Category | Matched | Job | Result |",
        "| --- | --- | --- | --- |",
    ]
    for row in rows:
        lines.append(f"| {row['category']} | {row['matched']} | {row['job']} | {row['result']} |")
    with open(summary_path, "a", encoding="utf-8") as handle:
        handle.write("\n".join(lines) + "\n")


def main(argv):
    del argv
    changes = json.loads(os.environ.get("CHANGES_JSON", "{}"))
    needs = json.loads(os.environ.get("NEEDS_JSON", "{}"))
    rows = evaluate(changes, needs)
    _write_step_summary(rows)

    messages = failures(rows)
    if messages:
        for message in messages:
            print(message, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
