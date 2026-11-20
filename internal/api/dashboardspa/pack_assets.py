#!/usr/bin/env python3
"""Pack canonical dashboard files deterministically; no dependency or network access."""
import argparse
import base64
import gzip
import json
from pathlib import Path


def pack(source):
    source = Path(source)
    files = []
    for path in sorted(source.rglob("*")):
        if path.is_symlink():
            raise ValueError("dashboard assets must not contain symlinks")
        if path.is_file():
            name = "dist/" + path.relative_to(source).as_posix()
            files.append({"name": name, "data": base64.b64encode(path.read_bytes()).decode("ascii")})
    if not files:
        raise ValueError("dashboard dist is empty")
    raw = json.dumps({"version": 1, "files": files}, separators=(",", ":"), ensure_ascii=True).encode()
    # Fix the gzip OS byte too: Python versions/platforms differ when mtime=0.
    compressed = bytearray(gzip.compress(raw, compresslevel=9, mtime=0))
    compressed[9] = 255
    return bytes(compressed)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    packed = pack(args.input)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_bytes(packed)


if __name__ == "__main__":
    main()
