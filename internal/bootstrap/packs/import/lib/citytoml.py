"""Surgical edits to city.toml.

The city.toml file is partly user-managed (e.g. [beads], [workspace].name)
and partly managed by gc import. Three sections in city.toml are managed
by this module:

  - [imports.X]                 — user-facing direct imports + constraints.
                                   Users CAN edit this section by hand;
                                   gc import re-emits it on add/remove/upgrade.
  - [packs.X]                   — machine-managed view of the resolved
                                   transitive closure for the v1 loader.
                                   Don't edit by hand.
  - [workspace].includes        — machine-managed list for the v1 loader.
                                   Don't edit by hand.

We can't use a full TOML rewriter because that would lose user formatting,
comments, and section ordering for the parts of city.toml that gc import
DOESN'T own. Instead we do targeted text edits:

  - Read the file as text.
  - Use tomllib to validate and find what's there.
  - For [imports.X] / [packs.X] blocks: find the exact line range and
    replace/insert/delete.
  - For includes: find the [workspace].includes line and rewrite the array.

This is more brittle than a TOML rewriter but works for the small set of
operations we need. We never touch any line that doesn't belong to a
managed construct.
"""

import re
import tomllib
from dataclasses import dataclass
from pathlib import Path
from typing import Optional


@dataclass
class PacksBlock:
    """A [packs.X] block in city.toml — the v1 schema's pack source declaration."""
    name: str
    source: str
    ref: str
    path: str = ""  # subpath within the repo


def read(path: Path) -> dict:
    """Parse city.toml into a dict using tomllib."""
    with open(path, "rb") as f:
        return tomllib.load(f)


def get_includes(city: dict) -> list[str]:
    return list(city.get("workspace", {}).get("includes", []))


def get_packs(city: dict) -> dict[str, PacksBlock]:
    out = {}
    for name, entry in city.get("packs", {}).items():
        out[name] = PacksBlock(
            name=name,
            source=entry.get("source", ""),
            ref=entry.get("ref", ""),
            path=entry.get("path", ""),
        )
    return out


def update_includes_and_packs(
    path: Path,
    new_includes: list[str],
    new_packs: dict[str, PacksBlock],
    removed_packs: set[str],
) -> None:
    """Rewrite city.toml in place with new includes list and pack blocks.

    - new_includes: the full new value of [workspace].includes (replaces existing).
    - new_packs: dict of name → PacksBlock to add or update.
    - removed_packs: set of names whose [packs.X] blocks should be deleted.

    Preserves all other content in the file (other sections, comments, formatting).
    """
    text = path.read_text()
    text = _rewrite_includes(text, new_includes)
    text = _delete_packs_blocks(text, removed_packs)
    text = _upsert_packs_blocks(text, new_packs)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(text)
    tmp.replace(path)


_INCLUDES_RE = re.compile(
    r"(?P<prefix>^\s*includes\s*=\s*)(?P<value>\[[^\]]*\])",
    re.MULTILINE,
)


def _rewrite_includes(text: str, new_includes: list[str]) -> str:
    """Replace the value of `includes = [...]` inside [workspace] with new list."""
    formatted = "[" + ", ".join(f'"{v}"' for v in new_includes) + "]"

    def replace(m: re.Match) -> str:
        return m.group("prefix") + formatted

    new_text, count = _INCLUDES_RE.subn(replace, text, count=1)
    if count == 0:
        # No existing includes line — add one inside [workspace], or create [workspace]
        if "[workspace]" in text:
            # Insert just after the [workspace] header
            new_text = re.sub(
                r"(\[workspace\]\n)",
                lambda m: m.group(1) + f"includes = {formatted}\n",
                text,
                count=1,
            )
        else:
            new_text = f"[workspace]\nincludes = {formatted}\n\n" + text
    return new_text


def _delete_packs_blocks(text: str, names: set[str]) -> str:
    """Remove `[packs.<name>]` blocks.

    A [packs.X] block as we write it consists of:
        [packs.<name>]
        source = "..."
        ref = "..."
        path = "..."        (optional)

    To avoid eating user content, we only delete the header line and
    immediately-following lines that look like simple `key = value` pairs.
    Any blank line, comment, or section header terminates the block.
    """
    if not names:
        return text
    lines = text.splitlines(keepends=True)
    out = []
    i = 0
    while i < len(lines):
        line = lines[i]
        m = re.match(r"^\s*\[packs\.([A-Za-z0-9_-]+)\]\s*$", line)
        if m and m.group(1) in names:
            # Skip the header
            i += 1
            # Skip key = value lines
            while i < len(lines) and re.match(r"^\s*[A-Za-z_][A-Za-z0-9_]*\s*=", lines[i]):
                i += 1
            # Eat one trailing blank line if present (the separator we wrote)
            if i < len(lines) and lines[i].strip() == "":
                i += 1
            continue
        out.append(line)
        i += 1
    return "".join(out)


def _upsert_packs_blocks(text: str, new_packs: dict[str, PacksBlock]) -> str:
    """For each name in new_packs, insert or replace its [packs.X] block."""
    if not new_packs:
        return text
    for name, block in new_packs.items():
        block_text = _format_packs_block(block)
        pattern = re.compile(
            r"^\s*\[packs\." + re.escape(name) + r"\]\s*$",
            re.MULTILINE,
        )
        m = pattern.search(text)
        if m:
            # Replace existing block — find its bounds (header line through next [ or EOF)
            start = m.start()
            # Walk forward to find the next section header or EOF
            after = text[m.end():]
            next_header = re.search(r"^\s*\[", after, re.MULTILINE)
            if next_header:
                end = m.end() + next_header.start()
            else:
                end = len(text)
            # Trim trailing blank line(s) so we don't accumulate them
            while end > 0 and text[end - 1] == "\n" and (end - 2 < 0 or text[end - 2] == "\n"):
                end -= 1
            text = text[:start] + block_text + "\n" + text[end:]
        else:
            # Append at end of file (with a leading blank line if needed)
            sep = "" if text.endswith("\n\n") or text == "" else ("\n" if text.endswith("\n") else "\n\n")
            text = text + sep + block_text + "\n"
    return text


def _format_packs_block(block: PacksBlock) -> str:
    lines = [f"[packs.{block.name}]"]
    lines.append(f'source = "{block.source}"')
    if block.ref:
        lines.append(f'ref = "{block.ref}"')
    if block.path:
        lines.append(f'path = "{block.path}"')
    return "\n".join(lines)


# ─────────────────────────────────────────────────────────────────────
# [imports.X] section management
# ─────────────────────────────────────────────────────────────────────


def update_imports(path: Path, imports: dict) -> None:
    """Rewrite the [imports.X] section of city.toml in place.

    `imports` is a dict mapping handle → ImportSpec (from lib.manifest).
    The full set replaces whatever was there before — any [imports.X]
    block not in the new dict is deleted.

    Creates city.toml if it doesn't exist (with just an [imports] section).
    """
    if path.exists():
        text = path.read_text()
    else:
        text = ""

    # Find every existing [imports.X] block and remove it
    existing_handles = _find_imports_handles(text)
    text = _delete_imports_blocks(text, existing_handles)

    # Insert the new blocks
    text = _insert_imports_blocks(text, imports)

    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.parent.mkdir(parents=True, exist_ok=True)
    tmp.write_text(text)
    tmp.replace(path)


def _find_imports_handles(text: str) -> set[str]:
    """Walk text, find every `[imports.X]` header line, return the set of X."""
    out = set()
    for line in text.splitlines():
        m = re.match(r"^\s*\[imports\.([A-Za-z0-9_-]+)\]\s*$", line)
        if m:
            out.add(m.group(1))
    return out


def _delete_imports_blocks(text: str, names: set[str]) -> str:
    """Remove `[imports.<name>]` blocks for every name in `names`.

    Same shape as _delete_packs_blocks: only removes the header and
    immediately-following key=value lines, then one trailing blank line.
    Comments and other section headers terminate the block.
    """
    if not names:
        return text
    lines = text.splitlines(keepends=True)
    out = []
    i = 0
    while i < len(lines):
        line = lines[i]
        m = re.match(r"^\s*\[imports\.([A-Za-z0-9_-]+)\]\s*$", line)
        if m and m.group(1) in names:
            i += 1
            while i < len(lines) and re.match(r"^\s*[A-Za-z_][A-Za-z0-9_]*\s*=", lines[i]):
                i += 1
            if i < len(lines) and lines[i].strip() == "":
                i += 1
            continue
        out.append(line)
        i += 1
    return "".join(out)


def _insert_imports_blocks(text: str, imports: dict) -> str:
    """Insert all the new [imports.X] blocks into text.

    The blocks are placed:
      - After any existing [imports.X] blocks (if any survive)
      - Otherwise, before the first [packs.X] block (if any)
      - Otherwise, at the end of the file

    Imports are sorted by handle for stable output.
    """
    if not imports:
        return text

    block_text = _format_imports_section(imports)

    # Strategy: find a good insertion point
    # 1. After the last [imports.*] block we wrote in the past (if there's a comment marker we can detect, etc.) — for v1 we just insert before the first [packs.*] header
    # 2. If no [packs.*] section exists, insert at end of file (with proper spacing)
    packs_match = re.search(r"^\s*\[packs\.", text, re.MULTILINE)
    if packs_match:
        insert_pos = packs_match.start()
        prefix = text[:insert_pos]
        # Normalize: trim any trailing blank lines from the prefix, then
        # add exactly one blank line before the imports block, and one
        # blank line after it before the [packs] block.
        prefix_trimmed = prefix.rstrip("\n")
        if prefix_trimmed:
            return prefix_trimmed + "\n\n" + block_text + "\n\n" + text[insert_pos:]
        else:
            return block_text + "\n\n" + text[insert_pos:]
    else:
        # Append at end of file
        if text == "":
            return block_text + "\n"
        if text.endswith("\n\n"):
            return text + block_text + "\n"
        elif text.endswith("\n"):
            return text + "\n" + block_text + "\n"
        else:
            return text + "\n\n" + block_text + "\n"


def _format_imports_section(imports: dict) -> str:
    """Format a dict of handle → ImportSpec as canonical [imports.X] blocks."""
    blocks = []
    for handle in sorted(imports.keys()):
        spec = imports[handle]
        lines = [f"[imports.{handle}]"]
        lines.append(f'source = "{spec.source}"')
        if spec.version:
            lines.append(f'version = "{spec.version}"')
        blocks.append("\n".join(lines))
    return "\n\n".join(blocks)
