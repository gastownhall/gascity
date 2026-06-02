#!/usr/bin/env python3
r"""CI suite-coverage classification and aggregation.

Companion to the ``changes`` job in ``.github/workflows/ci.yml``. That job
gates downstream jobs on dorny/paths-filter outputs, so a change confined to
one filter group skips jobs in other groups. A subtle cross-cutting change
(e.g. an ``internal/beads`` edit that breaks a worker test) can therefore land
without the affected jobs ever running. The ``shared`` filter closes that gap:
when a cross-cutting core path changes it is folded into every gated output, so
all downstream jobs run as a union ("full suite") regardless of their own
filter result.

This module is the deterministic mechanism behind that wiring and the metric
that measures it. No semantic judgement lives here — callers pass the
already-computed filter result (a boolean string) and these functions only do
arithmetic and string matching:

  * ``classify_mode`` — label a run ``full`` or ``filtered``.
  * ``paths_match`` — dorny-compatible glob matching, used by the wiring test
    to simulate which filters a changed-file set triggers.
  * ``aggregate`` — compute the share of runs that took each path.

Usage:

  # In CI (the ``changes`` job), record this run's classification:
  #   python3 ci_suite_coverage.py classify "$SHARED_FILTER_RESULT"
  # Writes ``suite_mode`` to $GITHUB_OUTPUT and a row to $GITHUB_STEP_SUMMARY.

  # Aggregate the metric across recent main-branch merges. Collect the
  # per-run modes (emitted by the classify step as ``ci_suite_mode=<mode>``
  # notices) and pipe them in, one token per line:
  #   gh run list --workflow=CI --branch=main --json databaseId --jq '.[].databaseId' \
  #     | while read -r id; do \
  #         gh run view "$id" --log 2>/dev/null | sed -n 's/.*ci_suite_mode=\([a-z]*\).*/\1/p' | head -n1; \
  #       done \
  #     | python3 ci_suite_coverage.py
"""

from __future__ import annotations

import json
import os
import sys
from typing import Iterable, Mapping

FULL = "full"
FILTERED = "filtered"

# Values dorny/paths-filter writes for a matched/unmatched filter.
TRUE = "true"


def classify_mode(shared_matched: bool) -> str:
    """Return the suite mode for a run.

    ``full`` means a cross-cutting core path changed, so every gated job ran.
    ``filtered`` means only path-scoped jobs ran.
    """
    return FULL if shared_matched else FILTERED


def _match_one(path: str, pattern: str) -> bool:
    """Match a single repo-relative path against one dorny-style glob.

    Supports the pattern shapes used by the CI filters: a ``prefix/**``
    directory glob, a ``**/*.ext`` suffix glob, and a literal path. This is a
    deterministic subset of picomatch sufficient to mirror dorny's behavior for
    the filters in ``ci.yml``; it is not a general glob engine.
    """
    if pattern.endswith("/**"):
        prefix = pattern[: -len("/**")]
        return path == prefix or path.startswith(prefix + "/")
    if pattern.startswith("**/"):
        suffix = pattern[len("**/") :]
        if suffix.startswith("*"):
            return path.endswith(suffix[1:])
        return path == suffix or path.endswith("/" + suffix)
    return path == pattern


def paths_match(changed_files: Iterable[str], globs: Iterable[str]) -> bool:
    """Return True if any changed file matches any glob in the filter."""
    globs = list(globs)
    return any(_match_one(path, glob) for path in changed_files for glob in globs)


def aggregate(modes: Iterable[str]) -> Mapping[str, object]:
    """Summarize a sequence of run modes into coverage percentages."""
    modes = list(modes)
    total = len(modes)
    full = sum(1 for mode in modes if mode == FULL)
    filtered = sum(1 for mode in modes if mode == FILTERED)
    unknown = total - full - filtered

    def pct(count: int) -> float:
        return round(100.0 * count / total, 1) if total else 0.0

    return {
        "total": total,
        "full": full,
        "filtered": filtered,
        "unknown": unknown,
        "full_pct": pct(full),
        "filtered_pct": pct(filtered),
    }


def _emit_classification(shared_result: str) -> None:
    """Record this run's suite mode for the metric.

    Writes the ``suite_mode`` job output and a human-readable row to the step
    summary, and prints a grep-able ``ci_suite_mode=<mode>`` notice that the
    aggregation step harvests from run logs.
    """
    mode = classify_mode(shared_result.strip().lower() == TRUE)

    github_output = os.environ.get("GITHUB_OUTPUT")
    if github_output:
        with open(github_output, "a", encoding="utf-8") as handle:
            handle.write(f"suite_mode={mode}\n")

    step_summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if step_summary:
        with open(step_summary, "a", encoding="utf-8") as handle:
            handle.write("## CI Suite Coverage\n\n")
            handle.write(f"- mode: `{mode}`\n")
            handle.write(f"- cross-cutting core path changed: `{shared_result}`\n")

    # A workflow notice annotation, also grep-able from `gh run view --log`.
    print(f"::notice title=CI suite coverage::ci_suite_mode={mode}")


def main(argv: list[str]) -> int:
    if len(argv) >= 2 and argv[1] == "classify":
        if len(argv) != 3:
            print("usage: ci_suite_coverage.py classify <shared-filter-result>", file=sys.stderr)
            return 2
        _emit_classification(argv[2])
        return 0

    # Default: aggregate mode tokens read from stdin, one per line.
    modes = [line.strip() for line in sys.stdin if line.strip()]
    print(json.dumps(aggregate(modes), indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
