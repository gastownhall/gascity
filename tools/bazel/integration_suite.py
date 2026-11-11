#!/usr/bin/env python3
"""Generate //test:integration_packages, the integration lane's package tier.

`go test -tags integration` with GC_FAST_UNIT=0 (scripts/test-integration-
shard's packages-* shards) differs from the unit lane only in packages that

  (a) hold a .go file whose //go:build line names the integration tag (the
      root `gazelle:build_tags integration` directive lists such files in the
      package's go_test srcs, and rules_go compiles them only under
      --config=integration), or
  (b) gate tests on GC_FAST_UNIT=0 through processgrouptest's
      RequireRealProcessSignals or herdrtest's RequireLive, which
      .bazelrc's test:integration config turns on.

Every other package's go_test builds and behaves identically in both builds,
so the unit lane already covers it. This script writes a test_suite naming
every go_test in the (a) and (b) packages into a managed block of
test/BUILD.bazel, plus every test_suite there named *_integration_solo_tests
(a package's long integration-build tests, each run alone in a target of its
own, e.g. //cmd/gc:gc_integration_solo_tests). //test/integration and the
acceptance tiers have lanes of their own and are left out.

Run after gazelle (see `make bazel-sync`); the CI "BUILD files in sync" job
fails when the result differs from what is committed, so a new
integration-tagged file anywhere joins the lane without a hand edit.
`--check` reports drift without writing.
"""

from __future__ import annotations

import os
import re
import sys

SUITE_BUILD = "test/BUILD.bazel"
SUITE_NAME = "integration_packages"
BEGIN = "# --- integration_packages (managed by tools/bazel/integration_suite.py) ---"
END = "# --- end integration_packages ---"
# Lanes of their own (bazel.yml: integration, acceptance).
OWN_LANES = ("test/integration", "test/acceptance")
SKIP_DIRS = {".git", ".claude", ".worktrees", "node_modules", "third_party", ".gc", ".beads",
             "testdata", "engdocs", "frontend", "bin"}

GO_BUILD_RE = re.compile(r"^//go:build (?P<expr>.+)$", re.MULTILINE)
INTEGRATION_TAG_RE = re.compile(r"(?<![\w.])integration(?![\w.])")
# A gate call as a statement of its own (not text inside a string literal).
FAST_UNIT_GATE_RE = re.compile(
    r"^\s+(?:processgrouptest\.RequireRealProcessSignals|herdrtest\.RequireLive)\(", re.MULTILINE)
GO_TEST_NAME_RE = re.compile(r'^go_test\(\n    name = "(?P<name>[^"]+)"', re.MULTILINE)
SOLO_SUITE_NAME_RE = re.compile(r'^test_suite\(\n    name = "(?P<name>[^"]+_integration_solo_tests)"', re.MULTILINE)


def build_constraint(src: str) -> str:
    """The //go:build expression in the file's header (before `package`)."""
    for line in src.splitlines():
        if line.startswith("package "):
            break
        m = GO_BUILD_RE.match(line)
        if m:
            return m.group("expr")
    return ""


def in_tier(pkg: str, files: list[str]) -> bool:
    for name in files:
        if not name.endswith(".go"):
            continue
        with open(os.path.join(pkg, name), encoding="utf-8") as f:
            src = f.read()
        if INTEGRATION_TAG_RE.search(build_constraint(src)):
            return True
        if name.endswith("_test.go") and FAST_UNIT_GATE_RE.search(src):
            return True
    return False


def suite_labels() -> list[str]:
    labels = []
    for dirpath, dirnames, files in os.walk("."):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS and not d.startswith((".", "bazel-")))
        pkg = os.path.normpath(dirpath)
        if pkg == "." or any(pkg == lane or pkg.startswith(lane + "/") for lane in OWN_LANES):
            continue
        if "BUILD.bazel" not in files or not in_tier(pkg, files):
            continue
        with open(os.path.join(pkg, "BUILD.bazel"), encoding="utf-8") as f:
            build = f.read()
        names = GO_TEST_NAME_RE.findall(build) + SOLO_SUITE_NAME_RE.findall(build)
        labels.extend(f"//{pkg}:{name}" for name in names)
    return sorted(labels)


def render(labels: list[str]) -> str:
    tests = "".join(f'        "{label}",\n' for label in labels)
    return (f"{BEGIN}\n\n"
            "# bazel.yml's integration lane runs this under --config=integration.\n"
            "# manual: `bazel test //...` (the unit lane) does not expand it.\n"
            "test_suite(\n"
            f'    name = "{SUITE_NAME}",\n'
            '    tags = ["manual"],\n'
            f"    tests = [\n{tests}    ],\n"
            ")\n\n"
            f"{END}\n")


def main(argv: list[str]) -> int:
    check = "--check" in argv
    os.chdir(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
    with open(SUITE_BUILD, encoding="utf-8") as f:
        src = f.read()
    block = render(suite_labels())
    start, end = src.find(BEGIN), src.find(END)
    if start >= 0 and end > start:
        out = src[:start] + block + src[end + len(END) + 1:]
    elif start < 0 and end < 0:
        # Before repo_tree.py's managed block, which must stay last.
        marker = src.find("# --- bazel_repo_srcs (managed by tools/bazel/repo_tree.py) ---")
        pos = marker if marker >= 0 else len(src)
        out = src[:pos] + block + "\n" + src[pos:]
    else:
        print(f"{SUITE_BUILD}: unbalanced {BEGIN!r} / {END!r} markers", file=sys.stderr)
        return 1
    if out == src:
        return 0
    if check:
        print(f"{SUITE_BUILD}: //test:{SUITE_NAME} is stale; run `python3 tools/bazel/integration_suite.py` "
              "(or `make bazel-sync`)", file=sys.stderr)
        return 1
    with open(SUITE_BUILD, "w", encoding="utf-8") as f:
        f.write(out)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
