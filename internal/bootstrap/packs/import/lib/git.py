"""Subprocess wrappers around git for gc import.

Functions return parsed Python data; errors are raised as GitError.
No third-party deps — just calls to the system `git` binary.
"""

import hashlib
import re
import shutil
import subprocess
from pathlib import Path
from typing import Optional


class GitError(Exception):
    pass


def _run(args: list[str], cwd: Optional[Path] = None, capture: bool = True) -> str:
    try:
        result = subprocess.run(
            ["git"] + args,
            cwd=str(cwd) if cwd else None,
            check=True,
            capture_output=capture,
            text=True,
        )
        return result.stdout if capture else ""
    except subprocess.CalledProcessError as e:
        msg = f"git {' '.join(args)} failed (exit {e.returncode})"
        if capture and e.stderr:
            msg += f": {e.stderr.strip()}"
        raise GitError(msg) from e
    except FileNotFoundError:
        raise GitError("git is not installed or not in PATH")


def split_url_and_subpath(url: str) -> tuple[str, str]:
    """Split a URL like https://host/org/repo/sub/path into (repo_url, subpath).

    For URLs without a subpath, returns (url, "").

    Heuristic: anything after the second path segment for github/gitlab style
    URLs is treated as a subpath. SSH URLs (`git@host:org/repo`) likewise.
    """
    # SSH form: git@host:org/repo[/subpath]
    m = re.match(r"^(git@[^:]+:[^/]+/[^/]+?)(?:/(.*))?$", url)
    if m:
        return m.group(1), (m.group(2) or "").strip("/")

    # HTTPS form: https://host/org/repo[/subpath]
    m = re.match(r"^(https?://[^/]+/[^/]+/[^/]+?)(?:/(.*))?$", url)
    if m:
        repo = m.group(1)
        # Strip a trailing .git if present, then re-add for the canonical form
        if repo.endswith(".git"):
            repo = repo[:-4]
        return repo, (m.group(2) or "").strip("/")

    # Fallback: treat the whole thing as a repo URL with no subpath
    return url, ""


def url_hash(url: str, commit: str) -> str:
    """Stable hash for the (URL, commit) pair, used as the accelerator key."""
    h = hashlib.sha256()
    h.update(url.encode("utf-8"))
    h.update(b"@")
    h.update(commit.encode("utf-8"))
    return h.hexdigest()


def ls_remote_tags(url: str) -> list[tuple[str, str]]:
    """Return [(tag_name, commit_sha)] for every tag on the remote.

    Strips refs/tags/ prefix and ^{} dereference markers.
    """
    out = _run(["ls-remote", "--tags", url])
    tags: dict[str, str] = {}
    for line in out.splitlines():
        if not line.strip():
            continue
        parts = line.split()
        if len(parts) != 2:
            continue
        sha, ref = parts
        if not ref.startswith("refs/tags/"):
            continue
        name = ref[len("refs/tags/"):]
        # ^{} marks the dereferenced commit for annotated tags; prefer it
        if name.endswith("^{}"):
            tags[name[:-3]] = sha
        else:
            tags.setdefault(name, sha)
    return list(tags.items())


def clone(url: str, dest: Path, ref: Optional[str] = None) -> None:
    """Clone a repo into `dest`. Optionally check out `ref` after clone."""
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists():
        raise GitError(f"clone destination already exists: {dest}")
    _run(["clone", "--quiet", url, str(dest)], capture=False)
    if ref:
        _run(["checkout", "--quiet", ref], cwd=dest, capture=False)


def fetch_to_accelerator(url: str, commit: str, accelerator_root: Path) -> Path:
    """Ensure the (url, commit) pair is in the user-level accelerator.

    Returns the path to the accelerator clone (always `accelerator_root/<hash>/`).
    If a clone already exists for this hash, it's reused as-is.
    """
    h = url_hash(url, commit)
    target = accelerator_root / h
    if target.exists() and (target / ".git").exists():
        return target
    if target.exists():
        # Stale partial — wipe and retry
        shutil.rmtree(target)
    clone(url, target, ref=commit)
    return target


def commit_for_tag(tags: list[tuple[str, str]], tag_name: str) -> Optional[str]:
    """Look up the commit SHA for a given tag in the output of ls_remote_tags."""
    for name, sha in tags:
        if name == tag_name:
            return sha
    return None


def materialize(src_dir: Path, dest_dir: Path, subpath: str = "") -> None:
    """Copy a pack from the accelerator into a city's cache.

    `src_dir` is the accelerator clone root. `subpath` is an optional
    subdirectory inside the repo (for multi-pack monorepos). `dest_dir`
    is the city-level cache directory for the pack.
    """
    if subpath:
        source = src_dir / subpath
    else:
        source = src_dir
    if not source.exists():
        raise GitError(f"source path does not exist: {source}")
    if dest_dir.exists():
        shutil.rmtree(dest_dir)
    dest_dir.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(source, dest_dir, ignore=shutil.ignore_patterns(".git"))
