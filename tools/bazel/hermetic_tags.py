#!/usr/bin/env python3
"""Write the hermeticity ledger's tags into go_test rules.

test/bazel-hermeticity.toml lists the go_test packages whose results must
never be served from the cache (tests that read the network, the calendar,
or other state a Bazel action key does not cover). This script makes each
listed package's go_test rules (every one: the ledger, like the static
scan, is per package) carry exactly the ledger's managed tags, and strips
managed tags from every other go_test, so BUILD.bazel cannot drift from the
ledger and gazelle regeneration cannot lose them. Tags outside MANAGED_TAGS
are left alone.

Run after gazelle (see `make bazel-sync`); the CI "BUILD files in sync" job
fails when the result differs from what is committed. `--check` reports
drift without writing. The ledger's own rules (reasons, cache-exempt tags,
stale entries) are enforced by internal/testpolicy/bazelhermetic.
"""

from __future__ import annotations

import os
import re
import sys
import tomllib

LEDGER = "test/bazel-hermeticity.toml"
# Keep in sync with ManagedTags in internal/testpolicy/bazelhermetic/ledger.go.
MANAGED_TAGS = {"external", "no-cache", "no-remote-cache", "no-remote-exec", "requires-network"}
SKIP_DIRS = {".git", ".claude", ".worktrees", "node_modules", "third_party", ".gc", ".beads"}

GO_TEST_RE = re.compile(r"^go_test\(\n.*?^\)\n", re.DOTALL | re.MULTILINE)
TAGS_RE = re.compile(r"^    tags = \[(?P<body>[^\]]*)\],\n", re.MULTILINE)
# Any rule-level tags attribute, to catch forms TAGS_RE cannot rewrite.
TAGS_ATTR_RE = re.compile(r"^    tags\s*=", re.MULTILINE)
# A tags list body of string literals only: no comments, no expressions.
PLAIN_TAGS_BODY_RE = re.compile(r'\s*(?:"[^"\n]*"\s*,\s*)*(?:"[^"\n]*"\s*)?')
NAME_RE = re.compile(r'^    name = "(?P<name>[^"]+)"', re.MULTILINE)


class UnsupportedTags(ValueError):
    """A go_test's tags attribute is not a plain list of string literals."""


def ledger_tags() -> dict[str, list[str]]:
    with open(LEDGER, "rb") as f:
        data = tomllib.load(f)
    out: dict[str, list[str]] = {}
    for target in data.get("target", []):
        out[target["package"]] = list(target.get("tags", []))
    return out


def build_files() -> list[str]:
    found = []
    for dirpath, dirnames, files in os.walk("."):
        dirnames[:] = sorted(d for d in dirnames
                             if d not in SKIP_DIRS and not d.startswith("bazel-") and not d.startswith("."))
        if "BUILD.bazel" in files:
            found.append(os.path.normpath(dirpath))
    return found


def render_tags(tags: list[str]) -> str:
    if not tags:
        return ""
    if len(tags) == 1:
        return f'    tags = ["{tags[0]}"],\n'
    body = "".join(f'        "{tag}",\n' for tag in tags)
    return f"    tags = [\n{body}    ],\n"


def retag(block: str, wanted: list[str]) -> str:
    match = TAGS_RE.search(block)
    if (match is None and TAGS_ATTR_RE.search(block)) or (
            match is not None and not PLAIN_TAGS_BODY_RE.fullmatch(match.group("body"))):
        # Rewriting would duplicate the attribute or drop a comment, and
        # skipping it would leave a managed tag the ledger does not grant.
        name = NAME_RE.search(block)
        raise UnsupportedTags(
            f"go_test {name.group('name') if name else '(unnamed)'}: tags must be a plain list "
            "of string literals (no comments, trailing `# keep`, or expressions)")
    existing = re.findall(r'"([^"]+)"', match.group("body")) if match else []
    keep = [tag for tag in existing if tag not in MANAGED_TAGS]
    tags = sorted(set(keep) | set(wanted))
    rendered = render_tags(tags)
    if match:
        return block[:match.start()] + rendered + block[match.end():]
    if not rendered:
        return block
    # Before the closing paren of the rule.
    return block[:-2] + rendered + block[-2:]


def main(argv: list[str]) -> int:
    check = "--check" in argv
    os.chdir(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
    wanted_by_pkg = ledger_tags()
    seen: set[str] = set()
    drift: list[str] = []
    for pkg in build_files():
        path = os.path.join(pkg, "BUILD.bazel")
        src = open(path).read()
        blocks = list(GO_TEST_RE.finditer(src))
        if not blocks:
            continue
        rel = "" if pkg == "." else pkg
        wanted = wanted_by_pkg.get(rel, [])
        if rel in wanted_by_pkg:
            seen.add(rel)
        out = src
        try:
            for block in reversed(blocks):
                new = retag(block.group(0), wanted)
                out = out[:block.start()] + new + out[block.end():]
        except UnsupportedTags as err:
            print(f"{path}: {err}", file=sys.stderr)
            return 1
        if out != src:
            drift.append(path)
            if not check:
                with open(path, "w") as f:
                    f.write(out)
    missing = sorted(set(wanted_by_pkg) - seen)
    if missing:
        print(f"{LEDGER}: no go_test BUILD.bazel for {', '.join(missing)}", file=sys.stderr)
        return 1
    if check and drift:
        print("go_test tags differ from " + LEDGER + " in: " + ", ".join(drift)
              + "\nrun `python3 tools/bazel/hermetic_tags.py` (or `make bazel-sync`)", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
