#!/usr/bin/env python3
"""Generate the hermetic repository source tree for bazel tests.

Whole-repo scan guards (beadmeta vocabulary checks, contract identity
writers, api error-code census, session priming gates, ...) need the
complete source tree as *declared inputs* to run under `bazel test` —
and to remote-execute at all, since a remote worker's only filesystem
is the action's declared runfiles.

Bazel globs cannot cross package boundaries, so this script:

  1. appends a `bazel_repo_srcs` filegroup to every BUILD.bazel under
     the source roots (idempotently — a previous block is replaced), and
  2. writes the `//:repo_source_tree` aggregation into the root
     BUILD.bazel as an explicit label list, one per package.

Run after `bazel run //:gazelle` (see `make bazel-sync`). The label list
is deterministic; regenerating only changes it when packages appear or
disappear.

Deliberately excluded: docs/, contrib/ (already exported as their own
all_files filegroups), and dot/ignore directories.
"""

from __future__ import annotations

import os
import re
import sys

SOURCE_ROOTS = ("internal", "cmd", "pkg", "examples", "test", "scripts", "docs", "contrib")
PKG_FILEGROUP = "bazel_repo_srcs"
ROOT_TARGET = "repo_source_tree"
SKIP_TOPDIRS = {".claude", ".worktrees", ".git", "bazel-bin", "bazel-out",
                "bazel-testlogs", "engdocs", "frontend",
                "third_party", "bin", ".gc", ".beads", "node_modules"}
BLOCK_MARKER = "# --- bazel_repo_srcs (managed by tools/bazel/repo_tree.py) ---"


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


def refresh_pkg_block(path: str) -> None:
    """Replace or append the managed filegroup block in one BUILD file."""
    block = (
        f"\n{BLOCK_MARKER}\n\n"
        f"filegroup(\n"
        f'    name = "{PKG_FILEGROUP}",\n'
        f'    srcs = glob(["**"], exclude = ["BUILD.bazel"], allow_empty = True),\n'
        f'    visibility = ["//visibility:public"],\n'
        f")\n"
    )
    src = open(path).read()
    canonical = BLOCK_MARKER + "\n\n" + block[len(BLOCK_MARKER) + 2:]
    if BLOCK_MARKER in src:
        out = re.sub(
            re.escape(BLOCK_MARKER) + r"\nfilegroup\(\n(?:[^()]|\([^()]*\))*?\)\n",
            canonical,
            src,
            flags=re.DOTALL,
        )
        # drop any duplicate appended blocks beyond the first replacement
        first_end = out.find(BLOCK_MARKER) + len(canonical)
        rest = out[first_end:]
        while BLOCK_MARKER in rest:
            rest = re.sub(
                r"\n?" + re.escape(BLOCK_MARKER) + r"\nfilegroup\(\n(?:[^()]|\([^()]*\))*?\)\n?",
                "\n",
                rest,
                flags=re.DOTALL,
            )
        out = out[:first_end] + rest
    else:
        out = src.rstrip("\n") + "\n" + block
    if out != src:
        open(path, "w").write(out)


def refresh_root(labels: list[str]) -> None:
    """Write the //:repo_source_tree aggregation into the root BUILD."""
    body = "\n".join(f'        "//{pkg}:{PKG_FILEGROUP}",' for pkg in labels)
    block = (
        f"# {ROOT_TARGET}: every source package's files as one declared input\n"
        f"# set, for whole-repo scan guards that remote-execute. Managed by\n"
        f"# tools/bazel/repo_tree.py — regenerate via `make bazel-sync`.\n"
        f"filegroup(\n"
        f'    name = "{ROOT_TARGET}",\n'
        f"    srcs = [\n{body}\n"
        f'        ":go.mod",\n'
        f'        ":Makefile",\n'
        f'        ":TESTING.md",\n'
        f'        ":root_extras",\n'
        f"    ],\n"
        f'    visibility = ["//visibility:public"],\n'
        f")\n"
    )
    extras = (
        "# Root-level trees consumed by whole-repo scan guards. Managed by\n"
        "# tools/bazel/repo_tree.py.\n"
        'filegroup(\n'
        '    name = "root_extras",\n'
        '    srcs = glob([\n'
        '        ".devcontainer/**",\n'
        '        "schemas/**",\n        "release-gates/**",\n        "specs/**",\n        "tools/**",\n        "*.md",\n'
        '        "engdocs/**",\n'
        '        ".github/**",\n'
        '        ".githooks/**",\n'
        '        "deps.env",\n'
        '        ".gitignore",\n'
        '        ".golangci.yml",\n'
        '    ]),\n'
        '    visibility = ["//visibility:public"],\n'
        ')\n'
    )
    path = "BUILD.bazel"
    src = open(path).read()
    if f'name = "{ROOT_TARGET}"' in src:
        header = f"# {ROOT_TARGET}: every source package"
        out = re.sub(re.escape(header) + r".*?\n\)\n", block.rstrip("\n") + "\n", src, flags=re.DOTALL)
    else:
        out = src.rstrip("\n") + "\n\n" + block
    if 'name = "root_extras"' in out:
        out = re.sub(r"# Root-level trees consumed by whole-repo scan guards.*?\n\)\n",
                     extras.rstrip("\n") + "\n", out, flags=re.DOTALL)
    else:
        out = out.rstrip("\n") + "\n\n" + extras
    open(path, "w").write(out)


def ensure_root_builds() -> None:
    """SOURCE_ROOT dirs without a BUILD.bazel get one holding the managed
    filegroup, so loose files directly under them (e.g. test/test-resources
    .toml) join the tree."""
    import os
    mark = BLOCK_MARKER + "\n\n"
    block = mark + (
        'filegroup(\n'
        '    name = "' + PKG_FILEGROUP + '",\n'
        '    srcs = glob(["**"], exclude = ["BUILD.bazel"], allow_empty = True),\n'
        '    visibility = ["//visibility:public"],\n'
        ')\n'
    )
    for root_dir in SOURCE_ROOTS:
        p = os.path.join(root_dir, "BUILD.bazel")
        if not os.path.exists(p):
            open(p, "w").write(block)


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
            src = open(path).read()
        except FileNotFoundError:
            continue
        marker = "embed_files" if "embed_files" in replacement else "pack_files"
        if marker in src:
            continue  # already present
        if needle in src:
            open(path, "w").write(src.replace(needle, replacement, 1))


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
    print(f"{ROOT_TARGET}: {len(pkgs)} packages")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
