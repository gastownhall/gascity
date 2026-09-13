#!/usr/bin/env python3
"""Select GitHub Actions runners for Gas City workflows."""

from __future__ import annotations

import os
from pathlib import Path


ALLOWLIST_PATH = Path(".github/blacksmith-allowlist.txt")

BLACKSMITH_RUNNERS = {
    "runner_2vcpu": "blacksmith-2vcpu-ubuntu-2404",
    "runner_8vcpu": "blacksmith-8vcpu-ubuntu-2404",
    "runner_16vcpu": "blacksmith-16vcpu-ubuntu-2404",
    "runner_32vcpu": "blacksmith-32vcpu-ubuntu-2404",
    "runner_macos": "blacksmith-12vcpu-macos-15",
}

GITHUB_RUNNERS = {
    "runner_2vcpu": "ubuntu-latest",
    "runner_8vcpu": "ubuntu-latest",
    "runner_16vcpu": "ubuntu-latest",
    "runner_32vcpu": "ubuntu-latest",
    "runner_macos": "macos-15",
}


def load_allowlist(path: Path = ALLOWLIST_PATH) -> set[str]:
    """Load the Blacksmith pull request author allowlist."""
    allowlist: set[str] = set()
    if not path.exists():
        return allowlist
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.split("#", 1)[0].strip()
        if line:
            allowlist.add(line.lower())
    return allowlist


def select_runners(
    event_name: str,
    author: str,
    allowlist: set[str],
    *,
    force_blacksmith: bool = False,
) -> tuple[bool, str, dict[str, str]]:
    """Return whether to use Blacksmith, the reason, and runner labels."""
    normalized_event = event_name.strip()
    normalized_author = author.strip()
    if force_blacksmith:
        return True, "Blacksmith forced by workflow input", BLACKSMITH_RUNNERS
    if normalized_event == "pull_request" and normalized_author.lower() in allowlist:
        return True, "pull request author is in .github/blacksmith-allowlist.txt", BLACKSMITH_RUNNERS
    if normalized_event != "pull_request":
        return (
            False,
            f"Blacksmith is limited to approved pull requests; using GitHub-hosted runners for {normalized_event or '<unknown>'}",
            GITHUB_RUNNERS,
        )
    return (
        False,
        f"author {normalized_author or '<unknown>'} is not on the Blacksmith allowlist; using GitHub-hosted runners",
        GITHUB_RUNNERS,
    )


def append_outputs(use_blacksmith: bool, reason: str, runners: dict[str, str]) -> None:
    """Append selected policy fields to GITHUB_OUTPUT."""
    output_path = os.environ["GITHUB_OUTPUT"]
    with open(output_path, "a", encoding="utf-8") as output:
        output.write(f"use_blacksmith={str(use_blacksmith).lower()}\n")
        output.write(f"reason={reason}\n")
        for name, runner in runners.items():
            output.write(f"{name}={runner}\n")


def append_summary(use_blacksmith: bool, reason: str, event_name: str, author: str) -> None:
    """Append a human-readable runner policy summary."""
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not summary_path:
        return
    backend = "Blacksmith" if use_blacksmith else "GitHub-hosted"
    with open(summary_path, "a", encoding="utf-8") as summary:
        summary.write("## Runner policy\n\n")
        summary.write(f"- backend: `{backend}`\n")
        summary.write(f"- use_blacksmith: `{str(use_blacksmith).lower()}`\n")
        summary.write(f"- reason: {reason}\n")
        if event_name == "pull_request":
            summary.write(f"- author: `{author or '<unknown>'}`\n")


def main() -> None:
    event_name = os.environ["EVENT_NAME"]
    author = os.environ.get("PR_AUTHOR", "").strip()
    force_blacksmith = os.environ.get("FORCE_BLACKSMITH", "").strip().lower() == "true"
    use_blacksmith, reason, runners = select_runners(
        event_name,
        author,
        load_allowlist(),
        force_blacksmith=force_blacksmith,
    )
    append_outputs(use_blacksmith, reason, runners)
    append_summary(use_blacksmith, reason, event_name, author)


if __name__ == "__main__":
    main()
