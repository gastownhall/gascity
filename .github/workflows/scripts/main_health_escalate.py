#!/usr/bin/env python3
"""File (or find) a P0 issue when a push to main breaks the build.

Invoked by the main-health workflow after a build/test step fails on a push
to main. Escalation only: this never reverts or otherwise touches the
breaking commit, it just gets a human paged via a tracked issue. Re-runs for
the same broken SHA (e.g. a flaky re-trigger) must not file duplicates, so
every issue body carries a marker keyed to the SHA and gh is searched for it
before creating a new one.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from typing import Callable

RunFunc = Callable[..., "subprocess.CompletedProcess[str]"]

ESCALATION_MARKER = "main-health-escalation-sha"
P0_LABEL = "P0"

_MERGE_PR_RE = re.compile(r"^Merge pull request #(\d+) from ")


class MissingContextError(Exception):
    """Raised when required GitHub Actions environment variables are absent."""


def parse_pr_number(message: str) -> str:
    """Extract the PR number from a merge commit message, or "" if none."""
    match = _MERGE_PR_RE.match(message)
    if not match:
        return ""
    return match.group(1)


def build_issue_title(sha: str) -> str:
    """Build the issue title, identifying the break by short SHA only."""
    return f"main branch build broken at {sha[:12]}"


def build_issue_body(sha: str, author: str, pr_number: str, run_url: str) -> str:
    """Build the issue body: context for a human, plus the dedup marker."""
    lines = [
        f"The build on `main` is broken as of commit `{sha}`.",
        "",
        f"**Author:** {author}",
    ]
    if pr_number:
        lines.append(f"**PR:** #{pr_number}")
    lines.extend(
        [
            f"**Workflow run:** {run_url}",
            "",
            "This issue was filed automatically; nothing has been reverted. "
            "A human needs to triage and fix (or revert) the breaking change.",
            "",
            f"<!-- {ESCALATION_MARKER}:{sha} -->",
        ]
    )
    return "\n".join(lines)


def find_existing_issue(sha: str, run: RunFunc = subprocess.run) -> str | None:
    """Return the URL of an already-filed escalation issue for this SHA, if any."""
    argv = [
        "gh",
        "issue",
        "list",
        "--search",
        f"{ESCALATION_MARKER}:{sha}",
        "--state",
        "all",
        "--json",
        "url",
    ]
    result = run(argv, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        return None
    try:
        issues = json.loads(result.stdout or "[]")
    except json.JSONDecodeError:
        return None
    if not issues:
        return None
    return issues[0]["url"]


def create_issue(title: str, body: str, run: RunFunc = subprocess.run) -> str:
    """Create the escalation issue and return its URL."""
    argv = [
        "gh",
        "issue",
        "create",
        "--title",
        title,
        "--body",
        body,
        "--label",
        P0_LABEL,
    ]
    result = run(argv, capture_output=True, text=True, check=True)
    return result.stdout.strip()


def escalate(sha: str, run_url: str, run: RunFunc = subprocess.run) -> str:
    """File (or find) the escalation issue for a broken-main SHA; return its URL."""
    existing = find_existing_issue(sha, run=run)
    if existing:
        return existing

    author = run(
        ["git", "log", "-1", "--format=%an <%ae>"], capture_output=True, text=True, check=True
    ).stdout.strip()
    message = run(
        ["git", "log", "-1", "--format=%B"], capture_output=True, text=True, check=True
    ).stdout.strip()
    pr_number = parse_pr_number(message)

    title = build_issue_title(sha)
    body = build_issue_body(sha=sha, author=author, pr_number=pr_number, run_url=run_url)
    return create_issue(title, body, run=run)


def gather_context(env: dict[str, str]) -> tuple[str, str]:
    """Pull the commit SHA and workflow run URL out of GitHub Actions env vars."""
    sha = env.get("GITHUB_SHA", "")
    server_url = env.get("GITHUB_SERVER_URL", "")
    repository = env.get("GITHUB_REPOSITORY", "")
    run_id = env.get("GITHUB_RUN_ID", "")
    if not sha:
        raise MissingContextError("GITHUB_SHA is required")
    if not server_url or not repository or not run_id:
        raise MissingContextError(
            "GITHUB_SERVER_URL, GITHUB_REPOSITORY, and GITHUB_RUN_ID are required"
        )
    run_url = f"{server_url}/{repository}/actions/runs/{run_id}"
    return sha, run_url


def main(env: dict[str, str] | None = None, run: RunFunc = subprocess.run) -> int:
    if env is None:
        env = dict(os.environ)
    try:
        sha, run_url = gather_context(env)
    except MissingContextError as exc:
        print(f"main_health_escalate: {exc}", file=sys.stderr)
        return 1
    url = escalate(sha=sha, run_url=run_url, run=run)
    print(url)
    return 0


if __name__ == "__main__":
    sys.exit(main())
