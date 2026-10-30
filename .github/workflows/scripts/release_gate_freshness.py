#!/usr/bin/env python3
"""Merge-time freshness check for release-gate criterion 6 (ga-wnziq7.1).

A release-gates/*.md record claims, as a point in time, that the reviewed
branch "diverges cleanly from main": it pins a ``Base ref: <ref>@<sha>`` and
cites the literal ``git merge-tree --write-tree <base> <head>`` and the tree
it produced. Nothing else re-reads that claim once the base moves, so a record
can reach main already false. This check, run by ci.yml's
``release-gate-freshness`` job on a PR that adds or modifies a record, re-runs
the cited merge-tree with the base ref resolved to its CURRENT tip and fails
when:

  * the record has no ``Base ref: <ref>@<40-hex sha>`` line,
  * its criterion 6 row cites no merge-tree invocation, no commit SHA head
    (``HEAD`` is not one) or no tree hash,
  * the base ref no longer resolves, or the cited head is not a commit in this
    checkout (the branch was rebased or amended after the gate ran),
  * the merge is no longer clean, or produces a different tree than recorded.

The cited head, not the PR's HEAD, is merged: the PR's HEAD contains the
record file itself, so its merge tree could never equal one recorded before
the record existed. Equal trees therefore mean exactly "the base has not moved
in a way that changes the merge", which is the staleness the record must not
carry. Only the records in the PR's own diff are examined.

Usage: release_gate_freshness.py --base-sha <pr base> --head-sha <pr head>
(run from the repository root; both commits and the base ref must be fetched).
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass


class RecordError(Exception):
    """A record is missing something the check needs to falsify its claim."""


@dataclass(frozen=True)
class Record:
    base_ref: str
    base_sha: str
    head: str
    tree: str


# A ref may not start with "-": the text is PR-controlled and reaches git argv.
_BASE_REF = re.compile(r"\bBase ref:\s*`?(?P<ref>[^\s@`-][^\s@`]*)@(?P<sha>[0-9a-f]{40})\b", re.IGNORECASE)
_ROW_6 = re.compile(r"^\|\s*6\b.*$", re.MULTILINE)
_MERGE_TREE = re.compile(r"git merge-tree --write-tree(?:\s+--[a-z-]+)*\s+[^\s`]+\s+(?P<head>[^\s`]+)")
_TREE = re.compile(r"\btree[\s:`*]*(?P<tree>[0-9a-f]{40})\b")
_COMMIT = re.compile(r"[0-9a-f]{7,40}")


def parse_record(text: str) -> Record:
    """Extract the pinned base and criterion 6 evidence, or say what is missing."""
    base = _BASE_REF.search(text)
    if not base:
        raise RecordError("no `Base ref: <ref>@<40-hex sha>` line; criterion 6 cannot be falsified without a pinned base")
    row = _ROW_6.search(text)
    call = _MERGE_TREE.search(row.group(0)) if row else None
    if not call:
        raise RecordError("criterion 6 row cites no `git merge-tree --write-tree <base> <head>` invocation")
    head = call.group("head")
    if not _COMMIT.fullmatch(head):
        raise RecordError(f"criterion 6 cites head {head!r}, not a commit SHA; HEAD or a branch name cannot be re-derived later")
    tree = _TREE.search(row.group(0))
    if not tree:
        raise RecordError("criterion 6 row reports no 40-hex tree hash after `tree`")
    return Record(base.group("ref"), base.group("sha"), head, tree.group("tree"))


def _git(repo: str, *args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["git", "-C", repo, *args], capture_output=True, text=True, check=False)


def _resolve(repo: str, rev: str) -> str | None:
    proc = _git(repo, "rev-parse", "--verify", "--quiet", f"{rev}^{{commit}}")
    return proc.stdout.strip() if proc.returncode == 0 else None


def check_record(repo: str, path: str, text: str) -> list[str]:
    """Return one failure message per way the record's criterion 6 no longer holds."""
    try:
        rec = parse_record(text)
    except RecordError as err:
        return [f"{path}: {err}"]
    pinned = f"{rec.base_ref}@{rec.base_sha}"
    tip = _resolve(repo, rec.base_ref) or _resolve(repo, f"origin/{rec.base_ref}")
    if not tip:
        return [f"{path}: base ref {rec.base_ref!r} (pinned {pinned}) no longer resolves; cannot re-derive criterion 6"]
    head = _resolve(repo, rec.head)
    if not head:
        return [f"{path}: cited head {rec.head} is not a commit in this checkout (rebased or amended since the gate ran); re-gate"]
    proc = _git(repo, "merge-tree", "--write-tree", "--name-only", tip, head)
    lines = proc.stdout.splitlines()
    if proc.returncode not in (0, 1) or not lines:
        return [f"{path}: git merge-tree failed ({proc.returncode}): {proc.stderr.strip()}"]
    if proc.returncode == 1:
        conflicts = lines[1 : lines.index("")] if "" in lines else lines[1:]
        return [
            f"{path}: criterion 6 is not clean against {rec.base_ref} tip {tip} (record pinned {pinned}, tree {rec.tree}); "
            f"conflicts in: {', '.join(conflicts) or 'unknown'}; re-gate"
        ]
    if lines[0] != rec.tree:
        return [
            f"{path}: criterion 6 is stale: base {rec.base_ref} moved {rec.base_sha} -> {tip}; "
            f"merge tree {rec.tree} (recorded) != {lines[0]} (now); re-gate"
        ]
    return []


def changed_records(repo: str, base: str, head: str) -> list[str]:
    """Release-gate records the PR adds or modifies (merge-base of base and head to head)."""
    proc = _git(repo, "diff", "-z", "--name-only", "--diff-filter=AM", f"{base}...{head}", "--", "release-gates/*.md")
    if proc.returncode != 0:
        raise RuntimeError(f"git diff {base}...{head} failed: {proc.stderr.strip()}")
    return [p for p in proc.stdout.split("\0") if p]


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--base-sha", required=True, help="the pull request's base commit")
    parser.add_argument("--head-sha", required=True, help="the pull request's head commit")
    args = parser.parse_args(argv)

    records = changed_records(".", args.base_sha, args.head_sha)
    if not records:
        print("release-gate-freshness: no release-gates/*.md added or modified; nothing to check")
        return 0
    failures: list[str] = []
    for path in records:
        shown = _git(".", "show", f"{args.head_sha}:{path}")
        if shown.returncode != 0:
            failures.append(f"{path}: cannot read at {args.head_sha}: {shown.stderr.strip()}")
            continue
        failures.extend(check_record(".", path, shown.stdout))
    for failure in failures:
        print(f"release-gate-freshness: FAIL {failure}", file=sys.stderr)
    print(f"release-gate-freshness: {len(records)} record(s) checked, {len(failures)} failed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
