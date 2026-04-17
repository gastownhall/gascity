"""Cache management for gc import.

Two caches:
  - User-level: ~/.gc/cache/repos/<sha>/  (hidden download accelerator)
  - City-level: <city>/.gc/cache/packs/<name>/  (loader-readable)
"""

import hashlib
import os
import shutil
from pathlib import Path


def user_accelerator_root() -> Path:
    """Return the path to the user-level download accelerator root.

    Honors the GC_HOME environment variable for testability.
    """
    home = os.environ.get("GC_HOME")
    if home:
        return Path(home) / "cache" / "repos"
    return Path.home() / ".gc" / "cache" / "repos"


def city_pack_cache(city_root: Path) -> Path:
    """Return the path to a city's pack cache directory."""
    return city_root / ".gc" / "cache" / "packs"


def ensure_dirs(*paths: Path) -> None:
    for p in paths:
        p.mkdir(parents=True, exist_ok=True)


def hash_directory(path: Path) -> str:
    """Compute a deterministic content hash of a directory tree.

    Walks the tree in sorted order, hashes (relative_path || NUL || file_bytes)
    for each file. Symlinks and the .git directory are ignored.
    """
    h = hashlib.sha256()
    if not path.exists():
        return ""
    files = []
    for root, dirs, filenames in os.walk(path):
        dirs.sort()
        if ".git" in dirs:
            dirs.remove(".git")
        for fn in sorted(filenames):
            full = Path(root) / fn
            if full.is_symlink():
                continue
            rel = full.relative_to(path)
            files.append((str(rel), full))
    for rel, full in sorted(files):
        h.update(rel.encode("utf-8"))
        h.update(b"\x00")
        with open(full, "rb") as f:
            for chunk in iter(lambda: f.read(65536), b""):
                h.update(chunk)
    return "sha256:" + h.hexdigest()


def remove_pack_from_cache(city_root: Path, name: str) -> None:
    target = city_pack_cache(city_root) / name
    if target.exists():
        shutil.rmtree(target)
