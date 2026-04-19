#!/usr/bin/env python3

import argparse
import shutil
import subprocess


PACKAGE_BY_PROVIDER = {
    "claude": "@anthropic-ai/claude-code",
    "codex": "@openai/codex",
    "gemini": "@google/gemini-cli",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)
    install = subparsers.add_parser("install")
    install.add_argument("--profile", required=True)
    install.add_argument("--force", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if args.command != "install":
        raise SystemExit(f"unsupported command: {args.command}")
    provider = args.profile.split("/", 1)[0].strip().lower()
    package = PACKAGE_BY_PROVIDER.get(provider)
    if not package:
        raise SystemExit(f"unsupported worker-inference profile: {args.profile!r}")
    if shutil.which(provider) and not args.force:
        print(f"{provider} already present in PATH; skipping install")
        return 0
    subprocess.run(["npm", "install", "-g", package], check=True)
    if not shutil.which(provider):
        raise SystemExit(f"{provider} was not found in PATH after installing {package}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
