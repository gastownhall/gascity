"""Consistent output formatting for gc import commands."""

import sys
from pathlib import Path
from typing import Optional


def info(msg: str) -> None:
    print(msg)


def step(msg: str, indent: int = 0) -> None:
    print(("  " * indent) + msg)


def warn(msg: str) -> None:
    print(f"warning: {msg}", file=sys.stderr)


def error(msg: str) -> None:
    print(f"error: {msg}", file=sys.stderr)


def die(msg: str, code: int = 1) -> None:
    error(msg)
    sys.exit(code)


def find_city_root(start: Optional[Path] = None) -> Path:
    """Walk up from `start` (or cwd) looking for the nearest city.toml.

    Errors out if no city.toml is found anywhere in the ancestor chain.
    """
    p = (start or Path.cwd()).resolve()
    while True:
        if (p / "city.toml").exists():
            return p
        if p.parent == p:
            die("not in a Gas City — no city.toml found in any parent directory")
        p = p.parent
