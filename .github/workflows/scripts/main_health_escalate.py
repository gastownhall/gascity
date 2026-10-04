#!/usr/bin/env python3
"""File (or find) a P0 issue when a push to main breaks the build.

Invoked by the main-health workflow after a build/test step fails on a push
to main. Escalation only: this never reverts or otherwise touches the
breaking commit, it just gets a human paged via a tracked issue. One open
escalation issue stands for a broken main: while it is open, each further
failing push is added to it as a comment instead of filing another P0, so a
main that stays broken across several merges still pages once. Issues the
watchdog filed are recognised by a marker in the body.
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
P0_LABEL = "priority/p0"
# Open P0 issues scanned for the watchdog's own: far above any realistic open-P0 count.
ISSUE_LIST_LIMIT = "100"

_MARKER_PREFIX = f"<!-- {ESCALATION_MARKER}:"
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


def _detail_lines(author: str, pr_number: str, run_url: str) -> list[str]:
    """The author / PR / workflow-run lines shared by issue and comment bodies."""
    lines = [f"**Author:** {author}"]
    if pr_number:
        lines.append(f"**PR:** #{pr_number}")
    lines.append(f"**Workflow run:** {run_url}")
    return lines


def build_issue_body(sha: str, author: str, pr_number: str, run_url: str) -> str:
    """Build the issue body: context for a human, plus the marker find_open_escalation_issue looks for."""
    lines = [
        f"The build on `main` is broken as of commit `{sha}`.",
        "",
        *_detail_lines(author, pr_number, run_url),
        "",
        "This issue was filed automatically; nothing has been reverted. "
        "A human needs to triage and fix (or revert) the breaking change.",
        "",
        f"{_MARKER_PREFIX}{sha} -->",
    ]
    return "\n".join(lines)


def build_comment_body(sha: str, author: str, pr_number: str, run_url: str) -> str:
    """Build the comment added to the open issue when a later push also breaks main."""
    lines = [
        f"`main` is still broken: commit `{sha}` also fails the build.",
        "",
        *_detail_lines(author, pr_number, run_url),
    ]
    return "\n".join(lines)


def _warn_lookup_failed(detail: str) -> None:
    print(f"main_health_escalate: could not list open escalation issues: {detail}", file=sys.stderr)


def find_open_escalation_issue(run: RunFunc = subprocess.run) -> str | None:
    """Return the URL of the open escalation issue this watchdog filed, if any.

    A failed lookup is reported on stderr and treated as "none open": a lost
    alert is worse than a possible duplicate, and a lookup that fails for a
    systemic reason makes the create that follows fail loudly as well.
    """
    argv = [
        "gh",
        "issue",
        "list",
        "--label",
        P0_LABEL,
        "--state",
        "open",
        "--limit",
        ISSUE_LIST_LIMIT,
        "--json",
        "url,body",
    ]
    result = run(argv, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        _warn_lookup_failed(result.stderr.strip())
        return None
    try:
        issues = json.loads(result.stdout or "[]")
    except json.JSONDecodeError as exc:
        _warn_lookup_failed(f"gh did not return JSON: {exc}")
        return None
    for issue in issues:
        if _MARKER_PREFIX in issue["body"]:
            return issue["url"]
    return None


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


def comment_on_issue(issue_url: str, body: str, run: RunFunc = subprocess.run) -> None:
    """Add a comment to an existing issue."""
    argv = ["gh", "issue", "comment", issue_url, "--body", body]
    run(argv, capture_output=True, text=True, check=True)


def escalate(sha: str, run_url: str, run: RunFunc = subprocess.run) -> str:
    """Report a broken-main SHA; return the URL of the issue filed or commented on.

    While an escalation issue is open, a further failing push is added to it as
    a comment rather than filing another P0.
    """
    author = run(
        ["git", "log", "-1", "--format=%an <%ae>"], capture_output=True, text=True, check=True
    ).stdout.strip()
    message = run(
        ["git", "log", "-1", "--format=%B"], capture_output=True, text=True, check=True
    ).stdout.strip()
    pr_number = parse_pr_number(message)

    existing = find_open_escalation_issue(run=run)
    if existing:
        comment = build_comment_body(sha=sha, author=author, pr_number=pr_number, run_url=run_url)
        comment_on_issue(existing, comment, run=run)
        return existing

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
