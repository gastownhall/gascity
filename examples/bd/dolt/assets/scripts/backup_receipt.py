#!/usr/bin/env python3
"""Atomically record one bd backup sync and its source/manifest evidence."""

import hashlib
import os
import re
import sys
import tempfile
import time


def main() -> int:
    if len(sys.argv) != 9:
        return 2
    receipt_dir, db, outcome, manifest, source_head, previous_manifest_hash, backup_head, verified_hash = sys.argv[1:]
    receipt_dir = os.path.abspath(receipt_dir)
    if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_-]*", db):
        return 2
    if outcome not in ("success", "failure", "skipped", "unverified"):
        return 2
    valid_head = re.fullmatch(r"[0-9a-v]{32}", source_head) is not None
    manifest_mtime = manifest_size = 0
    manifest_hash = "-"
    if outcome == "success" and valid_head and os.path.isfile(manifest):
        info = os.stat(manifest)
        manifest_mtime, manifest_size = int(info.st_mtime), info.st_size
        digest = hashlib.sha256()
        with open(manifest, "rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        manifest_hash = digest.hexdigest()
        if backup_head != source_head or verified_hash != manifest_hash:
            outcome = "unverified"
        elif previous_manifest_hash == manifest_hash:
            outcome = "noop"
    elif outcome == "success":
        outcome = "unverified"
    if not valid_head:
        source_head = "-"
    if not re.fullmatch(r"[0-9a-v]{32}", backup_head):
        backup_head = "-"
    destination_hash = hashlib.sha256(os.fsencode(os.path.realpath(os.path.dirname(manifest)))).hexdigest()
    os.makedirs(receipt_dir, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{db}.", dir=receipt_dir)
    try:
        with os.fdopen(fd, "w", encoding="ascii") as stream:
            stream.write(
                f"v3 {outcome} {int(time.time())} {manifest_mtime} "
                f"{manifest_size} {manifest_hash} {source_head} {backup_head} {destination_hash}\n"
            )
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, os.path.join(receipt_dir, db))
        directory_fd = os.open(receipt_dir, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
        parent_fd = os.open(os.path.dirname(receipt_dir), os.O_RDONLY)
        try:
            os.fsync(parent_fd)
        finally:
            os.close(parent_fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    return 1 if outcome == "unverified" else 0


if __name__ == "__main__":
    sys.exit(main())
