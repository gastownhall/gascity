"""Bounded memoization for GitPython 3.2.1's pure repository-path validator.

This module is installed as sitecustomize.py only in the MCP mail image. It
fails closed if the pinned GitPython version or the expected validator call
sites change. Every uncached input still goes through GitPython's validator.
"""

from functools import lru_cache
import importlib
from importlib.metadata import version
import os
import sys


_EXPECTED_GITPYTHON = "3.2.1"
if version("GitPython") != _EXPECTED_GITPYTHON:
    raise RuntimeError(
        "MCP mail path cache requires GitPython "
        f"{_EXPECTED_GITPYTHON}, found {version('GitPython')}"
    )

_git_util = importlib.import_module("git.util")
_ORIGINAL = _git_util._validate_repo_path


@lru_cache(maxsize=8192)
def _validate_cached(path: str, mode: int | None, platform: str) -> None:
    # In GitPython 3.2.1 this validator depends only on the exact path string,
    # entry mode, os.name and fixed module constants. It performs no I/O.
    return _ORIGINAL(path, mode)


def validate_repo_path(path, mode=None) -> None:
    """Cache only bounded, exact built-in inputs; preserve all other checks."""
    if type(path) is str and len(path) <= 256 and (mode is None or type(mode) is int):
        return _validate_cached(path, mode, os.name)
    return _ORIGINAL(path, mode)


def _patch_loaded_modules() -> tuple[str, ...]:
    patched: list[str] = []
    for name, module in tuple(sys.modules.items()):
        if name.startswith("git") and getattr(module, "_validate_repo_path", None) is _ORIGINAL:
            module._validate_repo_path = validate_repo_path
            patched.append(name)

    required = {
        "git.util",
        "git.index.base",
        "git.index.fun",
        "git.objects.fun",
        "git.objects.submodule.base",
    }
    missing = sorted(required - set(patched))
    if missing:
        raise RuntimeError(f"GitPython validator call sites were not patched: {missing}")
    return tuple(sorted(patched))


_PATCHED_MODULES = _patch_loaded_modules()


def cache_info():
    return _validate_cached.cache_info()
