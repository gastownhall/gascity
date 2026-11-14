#!/usr/bin/env python3
"""Fail when a gazelle:exclude'd test file runs nowhere anyone is watching.

`# gazelle:exclude <file>_test.go` hands a test file out of Bazel's generated
go_test. Nothing then says where it runs, so it can stop running without a
single red build (ga-pftcylx found one by accident). Every excluded test file
must be accounted for in the same BUILD.bazel, one of three ways:

  1. A go_test (or variant) in that file still lists it in srcs: Bazel runs it.
  2. `# excluded-test-gate: <file> <make-target> [note]`: a non-Bazel step runs
     it. <make-target> must be a Makefile target that a workflow runs with
     `make`, so the named step is a gate and not a convenience target.
  3. `# excluded-test-reason: <file> <why>`: it runs nowhere, and a reviewer
     accepted that when this line was added.

A marker naming a file that is no longer excluded is stale and fails too, so
the list cannot outlive the directive it justifies.

Run by `make bazel-sync`, and so by CI's "BUILD files in sync" job: it walks
every BUILD.bazel, which a bazel test sandbox cannot see. The marker syntax
and the fixture cases are pinned by scripts/bazel_gazelle_exclude_guard_test.go.
"""

from __future__ import annotations

import glob
import os
import re
import sys

SKIP_DIRS = {".git", ".claude", ".worktrees", "node_modules", "third_party", ".gc", ".beads"}
EXCLUDE_RE = re.compile(r"^\s*#\s*gazelle:exclude\s+(\S+_test\.go)\s*$")
GATE_RE = re.compile(r"^\s*#\s*excluded-test-gate:\s+(\S+)(?:\s+(\S+))?")
REASON_RE = re.compile(r"^\s*#\s*excluded-test-reason:\s+(\S+)\s+(\S.*)$")
TEST_RULE_RE = re.compile(r"^\w*_test\(", re.MULTILINE)


def build_files() -> list[str]:
    found = []
    for dirpath, dirnames, files in os.walk("."):
        dirnames[:] = sorted(d for d in dirnames
                             if d not in SKIP_DIRS and not d.startswith("bazel-") and not d.startswith("."))
        if "BUILD.bazel" in files:
            found.append(os.path.normpath(os.path.join(dirpath, "BUILD.bazel")))
    return found


def test_rule_text(src: str) -> str:
    """The text of every *_test(...) rule, comments dropped."""
    code = "\n".join(line for line in src.splitlines() if not line.lstrip().startswith("#"))
    out = []
    for m in TEST_RULE_RE.finditer(code):
        depth, i = 0, m.end() - 1
        while i < len(code):
            depth += (code[i] == "(") - (code[i] == ")")
            i += 1
            if depth == 0:
                break
        out.append(code[m.start():i])
    return "\n".join(out)


def make_targets() -> set[str]:
    with open("Makefile") as f:
        return set(re.findall(r"^([A-Za-z0-9][\w.-]*):", f.read(), re.MULTILINE))


def workflow_text() -> str:
    paths = glob.glob(".github/workflows/*.yml") + glob.glob(".github/workflows/*.yaml")
    return "\n".join(open(p).read() for p in paths)


def gate_problem(target: str | None, targets: set[str], workflows: str) -> str | None:
    if not target:
        return "excluded-test-gate names no make target"
    if target not in targets:
        return f"excluded-test-gate target {target!r} is not a Makefile target"
    if not re.search(rf"\bmake\b[^\n]*(?<![\w-]){re.escape(target)}(?![\w-])", workflows):
        return f"excluded-test-gate target {target!r} is run by no workflow (make {target})"
    return None


def check(path: str, targets: set[str], workflows: str) -> list[str]:
    with open(path) as f:
        src = f.read()
    lines = src.splitlines()
    excluded = {m.group(1) for line in lines if (m := EXCLUDE_RE.match(line))}
    gates = {m.group(1): m.group(2) for line in lines if (m := GATE_RE.match(line))}
    reasons = {m.group(1) for line in lines if (m := REASON_RE.match(line))}
    rules = test_rule_text(src)
    problems = []
    for name in sorted(excluded):
        if f'"{name}"' in rules:
            continue
        if name in gates:
            if (problem := gate_problem(gates[name], targets, workflows)):
                problems.append(f"{path}: {name}: {problem}")
        elif name not in reasons:
            problems.append(
                f"{path}: {name} is gazelle:exclude'd and runs in no Bazel target, no named gate step, "
                f"and carries no reviewed reason. Add '# excluded-test-gate: {name} <make-target>' "
                f"or '# excluded-test-reason: {name} <why it runs nowhere>' beside the directive.")
    for name in sorted((set(gates) | reasons) - excluded):
        problems.append(f"{path}: {name} has an excluded-test marker but no '# gazelle:exclude {name}' (stale)")
    return problems


def main() -> int:
    os.chdir(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
    targets, workflows = make_targets(), workflow_text()
    problems = [p for path in build_files() for p in check(path, targets, workflows)]
    for p in problems:
        print(p, file=sys.stderr)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
