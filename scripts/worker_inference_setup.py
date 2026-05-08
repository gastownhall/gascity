#!/usr/bin/env python3

import argparse
import os
from pathlib import Path
import shutil
import subprocess


NPM_PACKAGE_BY_PROVIDER = {
    "codex": ("@openai/codex", "CODEX_CLI_VERSION", "0.125.0"),
    "gemini": ("@google/gemini-cli", "GEMINI_CLI_VERSION", "0.40.0"),
    "opencode": ("opencode-ai", "OPENCODE_CLI_VERSION", "1.14.33"),
}
CLAUDE_CODE_VERSION = "2.1.123"


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
    if provider not in {"claude", *NPM_PACKAGE_BY_PROVIDER}:
        raise SystemExit(f"unsupported worker-inference profile: {args.profile!r}")
    if shutil.which(provider) and not args.force:
        print(f"{provider} already present in PATH; skipping install")
        return 0

    if provider == "claude":
        version = os.environ.get("CLAUDE_CODE_VERSION", CLAUDE_CODE_VERSION)
        repo_root = Path(__file__).resolve().parents[1]
        installer = repo_root / ".github" / "scripts" / "install-claude-native.sh"
        subprocess.run([str(installer), version], check=True)
    else:
        package, env_var, default_version = NPM_PACKAGE_BY_PROVIDER[provider]
        version = os.environ.get(env_var, default_version)
        subprocess.run(["npm", "install", "-g", f"{package}@{version}"], check=True)

    if not shutil.which(provider):
        raise SystemExit(f"{provider} was not found in PATH after installation")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
