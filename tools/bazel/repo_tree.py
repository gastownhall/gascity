#!/usr/bin/env python3
"""Generate the declared repository source trees for bazel tests.

Repository guards (whole-repo Go scans, docs link checks, CI-policy greps)
need the files they read as *declared inputs* to run under `bazel test` —
and to remote-execute at all, since a remote worker's only filesystem is
the action's declared runfiles.

Bazel globs cannot cross package boundaries, so this script:

  1. writes a managed block of three filegroups into every BUILD.bazel
     under the source roots (idempotently — a previous block is replaced):
       bazel_repo_srcs     every file in the package
       bazel_go_srcs       the package's non-test .go files
       bazel_go_test_srcs  the package's _test.go files
  2. writes the root aggregations, one label per package:
       //:repo_source_tree   every file (plus root_extras); invalidated by
                             any edit anywhere — only for guards that really
                             read docs, workflows and source together
       //:repo_go_srcs       every non-test .go file
       //:repo_go_test_srcs  every _test.go file

A test should declare the narrowest of these (or a package-level
filegroup) that covers what it reads, so unrelated edits keep it cached.

Run after `bazel run //:gazelle` (see `make bazel-sync`). Every block is
emitted in the exact form gazelle's formatter produces, so the
gazelle -> repo_tree sequence is a fixed point.

Deliberately excluded: dot directories and the SKIP_TOPDIRS below.
"""

from __future__ import annotations

import os
import sys

# tools/nogo is the only Bazel package tree under tools/; the rest of tools/
# stays in root_extras.
SOURCE_ROOTS = ("internal", "cmd", "pkg", "examples", "test", "scripts", "docs", "contrib", "tools/nogo")
SKIP_TOPDIRS = {".claude", ".worktrees", ".git", "bazel-bin", "bazel-out",
                "bazel-testlogs", "engdocs", "frontend",
                "third_party", "bin", ".gc", ".beads", "node_modules"}
BLOCK_MARKER = "# --- bazel_repo_srcs (managed by tools/bazel/repo_tree.py) ---"
ROOT_BEGIN = "# --- repo trees (managed by tools/bazel/repo_tree.py) ---"
ROOT_END = "# --- end repo trees ---"

PKG_ALL = "bazel_repo_srcs"
PKG_GO = "bazel_go_srcs"
PKG_GO_TEST = "bazel_go_test_srcs"
MANAGED_PKG_FILEGROUPS = (PKG_ALL, PKG_GO, PKG_GO_TEST)

PKG_BLOCK = f"""{BLOCK_MARKER}

filegroup(
    name = "{PKG_ALL}",
    srcs = glob(
        ["**"],
        allow_empty = True,
        exclude = ["BUILD.bazel"],
    ),
    visibility = ["//visibility:public"],
)

filegroup(
    name = "{PKG_GO}",
    srcs = glob(
        ["**/*.go"],
        allow_empty = True,
        exclude = ["**/*_test.go"],
    ),
    visibility = ["//visibility:public"],
)

filegroup(
    name = "{PKG_GO_TEST}",
    srcs = glob(
        ["**/*_test.go"],
        allow_empty = True,
    ),
    visibility = ["//visibility:public"],
)
"""

ROOT_EXTRAS = (
    ".devcontainer/**",
    "schemas/**",
    "release-gates/**",
    "specs/**",
    "tools/**",
    "*.md",
    "engdocs/**",
    ".github/**",
    ".githooks/**",
    "deps.env",
    ".gitignore",
    ".golangci.yml",
)


def packages() -> list[str]:
    """Every bazel package directory with a BUILD.bazel under SOURCE_ROOTS."""
    found = []
    for root_dir in SOURCE_ROOTS:
        for dirpath, dirnames, files in os.walk(root_dir):
            rel = os.path.relpath(dirpath)
            top = rel.split(os.sep)[0]
            if top in SKIP_TOPDIRS:
                dirnames[:] = []
                continue
            dirnames[:] = [d for d in dirnames if d not in SKIP_TOPDIRS and not d.startswith(".")]
            if "BUILD.bazel" in files:
                found.append(rel)
    return sorted(found)


def _skip_blank(src: str, pos: int) -> int:
    while pos < len(src) and src[pos] in " \t\n":
        pos += 1
    return pos


def _rule_end(src: str, pos: int) -> int:
    """End offset (past the trailing newline) of the call starting at pos."""
    depth = 0
    i = src.index("(", pos)
    while i < len(src):
        ch = src[i]
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth == 0:
                end = i + 1
                if end < len(src) and src[end] == "\n":
                    end += 1
                return end
        i += 1
    raise ValueError(f"unterminated rule at offset {pos}")


def _managed_filegroup_end(src: str, pos: int) -> int | None:
    """End of a managed filegroup starting at pos, or None if pos is not one."""
    if not src.startswith("filegroup(", pos):
        return None
    end = _rule_end(src, pos)
    body = src[pos:end]
    if any(f'name = "{name}"' in body for name in MANAGED_PKG_FILEGROUPS):
        return end
    return None


def _managed_span_end(src: str, start: int) -> int:
    """End of the managed region beginning at a BLOCK_MARKER at start: the
    marker plus every directly following managed filegroup, including any
    duplicated marker blocks left behind by older generator versions."""
    end = start + len(BLOCK_MARKER)
    while True:
        pos = _skip_blank(src, end)
        if src.startswith(BLOCK_MARKER, pos):
            end = pos + len(BLOCK_MARKER)
            continue
        fg_end = _managed_filegroup_end(src, pos)
        if fg_end is None:
            return end
        end = fg_end


def render_pkg_build(src: str) -> str:
    """BUILD content with the managed block replaced or appended."""
    start = src.find(BLOCK_MARKER)
    if start < 0:
        if not src.strip():
            return PKG_BLOCK
        return src.rstrip("\n") + "\n\n" + PKG_BLOCK
    end = _managed_span_end(src, start)
    rest = src[end:]
    if rest.strip():
        rest = "\n" + rest.lstrip("\n")
    else:
        rest = ""
    return src[:start] + PKG_BLOCK + rest


def refresh_pkg_block(path: str) -> None:
    with open(path) as f:
        src = f.read()
    out = render_pkg_build(src)
    if out != src:
        with open(path, "w") as f:
            f.write(out)


def _label_list(labels: list[str]) -> str:
    return "".join(f'        "{label}",\n' for label in labels)


def render_root_region(pkgs: list[str], root_go: list[str], root_go_test: list[str]) -> str:
    def pkg_labels(name: str) -> list[str]:
        return [f"//{pkg}:{name}" for pkg in pkgs]

    extras = "".join(f'        "{pattern}",\n' for pattern in ROOT_EXTRAS)
    return (
        f"{ROOT_BEGIN}\n"
        "\n"
        "# repo_source_tree: every source package's files plus the root-level\n"
        "# trees. Any edit anywhere invalidates its consumers, so declare it only\n"
        "# for guards that read docs, workflows and source together; prefer\n"
        "# repo_go_srcs / repo_go_test_srcs or a package-level filegroup.\n"
        "filegroup(\n"
        '    name = "repo_source_tree",\n'
        "    srcs = [\n"
        f"{_label_list(pkg_labels(PKG_ALL))}"
        '        ":go.mod",\n'
        '        ":Makefile",\n'
        '        ":TESTING.md",\n'
        '        ":root_extras",\n'
        "    ],\n"
        '    visibility = ["//visibility:public"],\n'
        ")\n"
        "\n"
        "# repo_go_srcs: every non-test .go file, for whole-repo Go source guards.\n"
        "filegroup(\n"
        '    name = "repo_go_srcs",\n'
        "    srcs = [\n"
        f"{_label_list(pkg_labels(PKG_GO) + [':' + f for f in root_go])}"
        "    ],\n"
        '    visibility = ["//visibility:public"],\n'
        ")\n"
        "\n"
        "# repo_go_test_srcs: every _test.go file, for guards over test sources.\n"
        "filegroup(\n"
        '    name = "repo_go_test_srcs",\n'
        "    srcs = [\n"
        f"{_label_list(pkg_labels(PKG_GO_TEST) + [':' + f for f in root_go_test])}"
        "    ],\n"
        '    visibility = ["//visibility:public"],\n'
        ")\n"
        "\n"
        "# Root-level trees aggregated into repo_source_tree.\n"
        "filegroup(\n"
        '    name = "root_extras",\n'
        "    srcs = glob([\n"
        f"{extras}"
        "    ]),\n"
        '    visibility = ["//visibility:public"],\n'
        ")\n"
        "\n"
        f"{ROOT_END}\n"
    )


def refresh_root(pkgs: list[str]) -> None:
    """Write the managed root aggregation region into the root BUILD."""
    root_go = sorted(f for f in os.listdir(".") if f.endswith(".go") and not f.endswith("_test.go"))
    root_go_test = sorted(f for f in os.listdir(".") if f.endswith("_test.go"))
    region = render_root_region(pkgs, root_go, root_go_test)
    path = "BUILD.bazel"
    with open(path) as f:
        src = f.read()
    start = src.find(ROOT_BEGIN)
    if start < 0:
        out = src.rstrip("\n") + "\n\n" + region
    else:
        end = src.index(ROOT_END, start) + len(ROOT_END)
        if end < len(src) and src[end] == "\n":
            end += 1
        out = src[:start] + region + src[end:]
    if out != src:
        with open(path, "w") as f:
            f.write(out)


def ensure_root_builds() -> None:
    """SOURCE_ROOT dirs without a BUILD.bazel get one holding the managed
    block, so loose files directly under them (e.g. test/test-resources
    .toml) join the tree."""
    for root_dir in SOURCE_ROOTS:
        if not os.path.isdir(root_dir):
            continue
        p = os.path.join(root_dir, "BUILD.bazel")
        if not os.path.exists(p):
            with open(p, "w") as f:
                f.write(PKG_BLOCK)


# Cross-package embed labels that gazelle cannot derive: go:embed patterns
# reaching into a subpackage (examples/bd embeds dolt/pack.toml from the
# examples/bd/dolt package; internal/bootstrap embeds packs/**). Gazelle's
# embedsrcs merge drops them on regeneration, so the sync generator restores
# them idempotently.
EMBED_LABEL_FIXES = {
    "examples/bd/BUILD.bazel": (
        '        "template-fragments/bead-worktree.template.md",\n    ],',
        '        "template-fragments/bead-worktree.template.md",\n        "//examples/bd/dolt:embed_files",\n    ],',
    ),
    "internal/bootstrap/BUILD.bazel": (
        '    visibility = ["//:__subpackages__"],\n    deps = [',
        '    visibility = ["//:__subpackages__"],\n    embedsrcs = ["//internal/bootstrap/packs/core:pack_files"],\n    deps = [',
    ),
}


def restore_embed_labels() -> None:
    for path, (needle, replacement) in EMBED_LABEL_FIXES.items():
        try:
            with open(path) as f:
                src = f.read()
        except FileNotFoundError:
            continue
        if replacement in src:
            continue  # already present
        if needle in src:
            with open(path, "w") as f:
                f.write(src.replace(needle, replacement, 1))


def main() -> int:
    if not os.path.exists("MODULE.bazel") or not os.path.exists("BUILD.bazel"):
        print("run from the repository root", file=sys.stderr)
        return 1
    ensure_root_builds()
    restore_embed_labels()
    pkgs = packages()
    for pkg in pkgs:
        refresh_pkg_block(os.path.join(pkg, "BUILD.bazel"))
    refresh_root(pkgs)
    print(f"repo trees: {len(pkgs)} packages")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
