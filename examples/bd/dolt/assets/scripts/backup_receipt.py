#!/usr/bin/env python3
"""Atomically record the outcome of one managed Dolt backup sync."""

import os
import hashlib
import re
import sys
import tempfile
import time


def main() -> int:
    if len(sys.argv) != 5:
        return 2
    receipt_dir, db, outcome, manifest = sys.argv[1:]
    if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_-]*", db):
        return 2
    if outcome not in ("success", "failure"):
        return 2
    manifest_mtime = manifest_size = 0
    manifest_hash = "-"
    if outcome == "success" and os.path.isfile(manifest):
        info = os.stat(manifest)
        manifest_mtime, manifest_size = int(info.st_mtime), info.st_size
        digest = hashlib.sha256()
        with open(manifest, "rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        manifest_hash = digest.hexdigest()
    elif outcome == "success":
        outcome = "unverified"
    os.makedirs(receipt_dir, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{db}.", dir=receipt_dir)
    try:
        with os.fdopen(fd, "w", encoding="ascii") as stream:
            stream.write(f"v1 {outcome} {int(time.time())} {manifest_mtime} {manifest_size} {manifest_hash}\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, os.path.join(receipt_dir, db))
        directory_fd = os.open(receipt_dir, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    return 0


if __name__ == "__main__":
    sys.exit(main())
