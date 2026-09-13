#!/usr/bin/env python3
import json
import os
import subprocess
import sys


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except Exception:
        payload = {}

    session_id = str(payload.get("session_id") or "").strip()
    cwd = str(payload.get("cwd") or os.getcwd()).strip() or os.getcwd()

    env = os.environ.copy()
    home = env.get("HOME", "")
    env["PATH"] = f"{home}/go/bin:{home}/.local/bin:/opt/homebrew/bin:/usr/local/bin:" + env.get("PATH", "")
    env["GC_MANAGED_SESSION_HOOK"] = "1"
    env["GC_HOOK_EVENT_NAME"] = "SessionStart"
    env["GC_PROVIDER_SESSION_ID_REQUIRED"] = "kimi"
    if session_id:
        env["GC_PROVIDER_SESSION_ID"] = session_id

    proc = subprocess.run(["gc", "prime", "--hook"], cwd=cwd, env=env)
    return proc.returncode


if __name__ == "__main__":
    raise SystemExit(main())
