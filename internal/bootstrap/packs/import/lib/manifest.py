"""Read/write the [imports] section of city.toml.

The user-facing manifest of direct imports lives inline in city.toml as:

    [imports.gastown]
    source = "https://github.com/example/gastown"
    version = "^1.2"

    [imports.helper]
    source = "../helper"

This is the v1 schema. In v2, the same syntax moves to pack.toml at the
city root. The package manager owns the [imports] section but does not own
the rest of city.toml — surgical text edits in lib/citytoml.py preserve
the user's other sections, comments, and formatting.

Read here accepts legacy `url` / `path` keys as one-wave compatibility
aliases. Write here always emits canonical `source`.
"""

import tomllib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional


def is_path_source(source: Optional[str]) -> bool:
    """Return True iff a source string should be treated as a local path."""
    return bool(source) and source.startswith(("/", ".", "~"))


@dataclass
class ImportSpec:
    handle: str
    source: Optional[str] = None
    version: Optional[str] = None  # the constraint string, not the resolved version

    def is_path(self) -> bool:
        return is_path_source(self.source)

    def is_git(self) -> bool:
        return self.source is not None and not self.is_path()

    @property
    def url(self) -> Optional[str]:
        """Read-only compatibility alias for one-wave migration support."""
        if self.is_path():
            return None
        return self.source

    @property
    def path(self) -> Optional[str]:
        """Read-only compatibility alias for one-wave migration support."""
        if self.is_path():
            return self.source
        return None

    def validate(self) -> None:
        if not self.source:
            raise ValueError(f"import {self.handle!r} has no source")


@dataclass
class Manifest:
    imports: dict[str, ImportSpec] = field(default_factory=dict)


def read(city_toml_path: Path) -> Manifest:
    """Read the [imports] section out of a city.toml file.

    Returns an empty manifest if city.toml doesn't exist or has no [imports].
    """
    if not city_toml_path.exists():
        return Manifest()
    with open(city_toml_path, "rb") as f:
        data = tomllib.load(f)
    imports: dict[str, ImportSpec] = {}
    for handle, entry in data.get("imports", {}).items():
        if not isinstance(entry, dict):
            continue
        source = entry.get("source")
        if source is None:
            source = entry.get("url")
        if source is None:
            source = entry.get("path")
        spec = ImportSpec(
            handle=handle,
            source=source,
            version=entry.get("version"),
        )
        spec.validate()
        imports[handle] = spec
    return Manifest(imports=imports)


def write(m: Manifest, city_toml_path: Path) -> None:
    """Write the [imports] section back into city.toml.

    Delegates to citytoml.update_imports for the surgical text edit.
    """
    # Imported lazily to avoid a circular import (citytoml may want manifest types)
    from . import citytoml
    citytoml.update_imports(city_toml_path, m.imports)
