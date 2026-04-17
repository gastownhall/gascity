"""Implicit imports — the lexical-splice-of-[imports] feature.

Every city automatically gets a small set of packs from
~/.gc/implicit-import.toml unless it opts out. The default file
contains exactly one entry: `maintenance`.

The file format is identical to a [imports] fragment of city.toml /
pack.toml. Only the [imports] table is read; anything else in the
file is ignored with a warning. The merge rule is "city wins on
collision" — an explicit [imports.X] in the city's own city.toml
shadows the implicit one.

This file is not user-facing config. It's set by gc-import on first
run with a default, and not normally touched. If gc-import ships a
new default in a future release, the file is overwritten on the
next first-run check (TBD; v1 leaves the file alone after first
write).

See doc-packman.md "Implicit imports" section for the full design.
"""

import os
import sys
import tomllib
from pathlib import Path
from typing import Optional

from . import manifest


# The single default entry written to ~/.gc/implicit-import.toml on
# first run. v1 has exactly one entry: maintenance. The source is the
# canonical maintenance pack location (working name; final TBD).
DEFAULT_FILE_CONTENTS = """\
# ~/.gc/implicit-import.toml — managed by gc-import.
#
# This file is not user configuration. It is set by gc-import on
# first run with a baseline list of packs that every city imports
# automatically. Treat it like vendor-installed config under /etc/:
# present, predictable, hands-off. To opt out per-city, set
# implicit_imports = false at the top level of your city.toml.

[imports.maintenance]
source = "https://github.com/gastownhall/maintenance"
version = "^1"
"""


def implicit_file_path() -> Path:
    """Return the path to ~/.gc/implicit-import.toml.

    Honors GC_HOME for testability — the same env var the cache module
    uses to relocate ~/.gc/ during tests.
    """
    home = os.environ.get("GC_HOME")
    if home:
        return Path(home) / "implicit-import.toml"
    return Path.home() / ".gc" / "implicit-import.toml"


def ensure_default_file() -> Path:
    """If ~/.gc/implicit-import.toml doesn't exist, write the default.

    Returns the path either way. Idempotent: only writes if absent.
    """
    path = implicit_file_path()
    if not path.exists():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(DEFAULT_FILE_CONTENTS)
    return path


def read_implicit_imports() -> dict[str, manifest.ImportSpec]:
    """Read the [imports] section of ~/.gc/implicit-import.toml.

    Returns a dict of handle -> ImportSpec, identical in shape to
    what manifest.read() returns from a city.toml.

    Anything in the implicit file outside the [imports] table is
    ignored with a warning to stderr — the implicit file's contract
    is "contribute imports, nothing else."

    Returns an empty dict if the file is missing (caller should
    typically run ensure_default_file() first to populate it).
    """
    path = implicit_file_path()
    if not path.exists():
        return {}

    with open(path, "rb") as f:
        data = tomllib.load(f)

    # Warn about any non-[imports] top-level tables. They're ignored.
    extra_keys = [k for k in data.keys() if k != "imports"]
    if extra_keys:
        print(
            f"warning: {path} contains unsupported top-level table(s) "
            f"{', '.join(repr(k) for k in extra_keys)} — ignoring. "
            f"Only [imports] is read from this file.",
            file=sys.stderr,
        )

    imports: dict[str, manifest.ImportSpec] = {}
    for handle, entry in data.get("imports", {}).items():
        if not isinstance(entry, dict):
            continue
        source = entry.get("source")
        if source is None:
            source = entry.get("url")
        if source is None:
            source = entry.get("path")
        spec = manifest.ImportSpec(
            handle=handle,
            source=source,
            version=entry.get("version"),
        )
        try:
            spec.validate()
        except ValueError as e:
            print(
                f"warning: {path} has an invalid implicit import: {e}. Skipping.",
                file=sys.stderr,
            )
            continue
        imports[handle] = spec
    return imports


def splice_into_city(
    city_imports: dict[str, manifest.ImportSpec],
    *,
    opt_out: bool = False,
) -> dict[str, manifest.ImportSpec]:
    """Merge implicit imports into the city's own imports.

    Rules:
    - If opt_out is True (city has implicit_imports = false), the
      implicit list is skipped entirely. The city's imports are
      returned unchanged.
    - Otherwise, ensure the default file exists, read its [imports],
      and merge them into the city's imports with "city wins on
      collision" semantics.
    - Implicit handles that conflict with a city handle are silently
      dropped (the user's explicit choice wins).

    Returns a new dict; does not mutate the input.
    """
    if opt_out:
        return dict(city_imports)

    ensure_default_file()
    implicit = read_implicit_imports()

    # City wins on collision: start with implicit, overlay city.
    merged: dict[str, manifest.ImportSpec] = {}
    for handle, spec in implicit.items():
        merged[handle] = spec
    for handle, spec in city_imports.items():
        merged[handle] = spec
    return merged


def is_implicit_handle(handle: str) -> bool:
    """Return True iff the handle is in the current implicit list.

    Used by gc import list to decide whether to show the (implicit)
    marker, and by gc import remove to refuse removal of implicit
    entries.
    """
    return handle in read_implicit_imports()


def read_opt_out_flag(city_toml_path: Path) -> bool:
    """Read implicit_imports from the top level of city.toml.

    Returns True iff implicit_imports = false is set. (We return
    True for the opt-out case so callers can pass it directly to
    splice_into_city's opt_out parameter.)

    Returns False (i.e., implicit imports are enabled) if the file
    doesn't exist or doesn't set the flag.
    """
    if not city_toml_path.exists():
        return False
    with open(city_toml_path, "rb") as f:
        data = tomllib.load(f)
    return data.get("implicit_imports") is False
