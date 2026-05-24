#!/usr/bin/env python3
"""Collect weekly public project metrics and render the wiki dashboard."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import math
import os
import statistics
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


SCHEMA_VERSION = "gascity.project_dashboard.v1"
DEFAULT_REPO = "gastownhall/gascity"
WINDOW_DAYS = 7
COMMUNITY_WINDOW_DAYS = 30
STALE_PR_DAYS = 14
STALE_ISSUE_DAYS = 30
AGENT_LOGIN_MARKERS = (
    "[bot]",
    "bot",
    "github-actions",
    "dependabot",
    "renovate",
    "codecov",
    "codex",
    "claude",
    "gemini",
    "opencode",
)
MAINTAINER_ASSOCIATIONS = {"COLLABORATOR", "MEMBER", "OWNER"}


def utcnow() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0)


def parse_time(value: str | None) -> dt.datetime | None:
    if not value:
        return None
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def iso_date(days_ago: int = 0, now: dt.datetime | None = None) -> str:
    now = now or utcnow()
    return (now - dt.timedelta(days=days_ago)).date().isoformat()


def median(values: list[float]) -> float | None:
    if not values:
        return None
    return round(float(statistics.median(values)), 2)


def percentage(numerator: int, denominator: int) -> float | None:
    if denominator == 0:
        return None
    return round((numerator / denominator) * 100, 1)


def human_number(value: Any) -> str:
    if value is None:
        return "n/a"
    if isinstance(value, bool):
        return "yes" if value else "no"
    if isinstance(value, float):
        if math.isnan(value):
            return "n/a"
        return f"{value:.1f}".rstrip("0").rstrip(".")
    if isinstance(value, int):
        return f"{value:,}"
    return str(value)


def display_unit(value: Any, unit: str) -> str:
    if value == 1 and unit.endswith("s"):
        return unit[:-1]
    return unit


def metric(value: Any, unit: str = "", source: str = "", note: str = "") -> dict[str, Any]:
    payload: dict[str, Any] = {"value": value}
    if unit:
        payload["unit"] = unit
    if source:
        payload["source"] = source
    if note:
        payload["note"] = note
    return payload


class GitHubClient:
    def __init__(self, token: str, api_url: str = "https://api.github.com") -> None:
        if not token:
            raise ValueError("GITHUB_TOKEN is required")
        self.token = token
        self.api_url = api_url.rstrip("/")

    def request_json(
        self,
        path: str,
        params: dict[str, Any] | None = None,
        optional: bool = False,
    ) -> Any:
        url = path if path.startswith("https://") else f"{self.api_url}{path}"
        if params:
            url = f"{url}?{urllib.parse.urlencode(params)}"
        request = urllib.request.Request(
            url,
            headers={
                "Accept": "application/vnd.github+json",
                "Authorization": f"Bearer {self.token}",
                "X-GitHub-Api-Version": "2022-11-28",
                "User-Agent": "gascity-project-dashboard",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                body = response.read()
        except urllib.error.HTTPError as error:
            if optional and error.code in {403, 404, 451}:
                return None
            raise
        if not body:
            return None
        return json.loads(body.decode("utf-8"))

    def request_page(
        self,
        path: str,
        params: dict[str, Any] | None = None,
        optional: bool = False,
    ) -> tuple[Any, str | None]:
        url = path if path.startswith("https://") else f"{self.api_url}{path}"
        if params:
            url = f"{url}?{urllib.parse.urlencode(params)}"
        request = urllib.request.Request(
            url,
            headers={
                "Accept": "application/vnd.github+json",
                "Authorization": f"Bearer {self.token}",
                "X-GitHub-Api-Version": "2022-11-28",
                "User-Agent": "gascity-project-dashboard",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                body = response.read()
                link = response.headers.get("Link")
        except urllib.error.HTTPError as error:
            if optional and error.code in {403, 404, 451}:
                return None, None
            raise
        next_url = parse_next_link(link)
        return json.loads(body.decode("utf-8")) if body else None, next_url

    def paginate(
        self,
        path: str,
        params: dict[str, Any] | None = None,
        max_pages: int = 10,
        optional: bool = False,
    ) -> list[Any]:
        items: list[Any] = []
        next_path: str | None = path
        next_params = dict(params or {})
        for _ in range(max_pages):
            if not next_path:
                break
            page, next_url = self.request_page(next_path, next_params, optional=optional)
            if page is None:
                break
            if isinstance(page, list):
                items.extend(page)
            else:
                items.append(page)
            next_path = next_url
            next_params = {}
        return items


def parse_next_link(link_header: str | None) -> str | None:
    if not link_header:
        return None
    for part in link_header.split(","):
        url_part, _, rel_part = part.partition(";")
        if 'rel="next"' in rel_part:
            return url_part.strip()[1:-1]
    return None


def is_agent_user(user: dict[str, Any] | None) -> bool:
    if not user:
        return True
    login = str(user.get("login") or "").lower()
    user_type = str(user.get("type") or "").lower()
    if user_type == "bot":
        return True
    return any(marker in login for marker in AGENT_LOGIN_MARKERS)


def is_human_user(user: dict[str, Any] | None) -> bool:
    return not is_agent_user(user)


def is_external_author(item: dict[str, Any]) -> bool:
    association = str(item.get("author_association") or "").upper()
    return association not in MAINTAINER_ASSOCIATIONS


def search_count(client: GitHubClient, repo: str, query: str) -> int:
    result = client.request_json("/search/issues", {"q": f"repo:{repo} {query}", "per_page": 1})
    return int(result.get("total_count", 0))


def search_items(
    client: GitHubClient,
    repo: str,
    query: str,
    per_page: int = 100,
    max_pages: int = 3,
) -> list[dict[str, Any]]:
    result = client.paginate(
        "/search/issues",
        {"q": f"repo:{repo} {query}", "per_page": per_page},
        max_pages=max_pages,
    )
    items: list[dict[str, Any]] = []
    for page in result:
        if isinstance(page, dict) and "items" in page:
            items.extend(page["items"])
        elif isinstance(page, dict):
            items.append(page)
    return items


def latest_workflow_status(
    client: GitHubClient,
    repo: str,
    workflow_name: str,
) -> dict[str, Any]:
    workflows = client.paginate(f"/repos/{repo}/actions/workflows", {"per_page": 100})
    workflow_id = None
    for page in workflows:
        for workflow in page.get("workflows", []) if isinstance(page, dict) else []:
            if workflow.get("name") == workflow_name or workflow.get("path", "").endswith(workflow_name):
                workflow_id = workflow.get("id")
                break
        if workflow_id:
            break
    if not workflow_id:
        return {"workflow": workflow_name, "conclusion": None, "updated_at": None}
    runs = client.request_json(
        f"/repos/{repo}/actions/workflows/{workflow_id}/runs",
        {"per_page": 1, "branch": "main"},
        optional=True,
    )
    if not runs or not runs.get("workflow_runs"):
        return {"workflow": workflow_name, "conclusion": None, "updated_at": None}
    run = runs["workflow_runs"][0]
    return {
        "workflow": workflow_name,
        "conclusion": run.get("conclusion") or run.get("status"),
        "updated_at": run.get("updated_at"),
        "html_url": run.get("html_url"),
    }


def collect_workflow_metrics(client: GitHubClient, repo: str, since: str) -> dict[str, Any]:
    runs = client.paginate(
        f"/repos/{repo}/actions/runs",
        {
            "created": f">={since}",
            "status": "completed",
            "per_page": 100,
        },
        max_pages=5,
        optional=True,
    )
    completed: list[dict[str, Any]] = []
    for page in runs:
        if isinstance(page, dict):
            completed.extend(page.get("workflow_runs", []))
    successful = [run for run in completed if run.get("conclusion") == "success"]
    durations: list[float] = []
    for run in completed:
        started_at = parse_time(run.get("run_started_at") or run.get("created_at"))
        updated_at = parse_time(run.get("updated_at"))
        if started_at and updated_at and updated_at >= started_at:
            durations.append((updated_at - started_at).total_seconds() / 60)
    return {
        "ci_completed_runs_7d": len(completed),
        "ci_success_rate_7d": percentage(len(successful), len(completed)),
        "ci_median_duration_minutes_7d": median(durations),
        "workflow_latest": [
            latest_workflow_status(client, repo, "CI"),
            latest_workflow_status(client, repo, "Nightly"),
            latest_workflow_status(client, repo, "Homebrew Tap Smoke"),
            latest_workflow_status(client, repo, "OpenSSF Scorecard"),
        ],
    }


def collect_first_response_metrics(
    client: GitHubClient,
    repo: str,
    since: str,
    max_items: int = 10,
) -> dict[str, Any]:
    items = search_items(client, repo, f"created:>={since}", per_page=100, max_pages=2)
    durations: list[float] = []
    sampled = 0
    for item in items[:max_items]:
        author = item.get("user") or {}
        if not is_human_user(author):
            continue
        created_at = parse_time(item.get("created_at"))
        number = item.get("number")
        if not created_at or not number:
            continue
        author_login = author.get("login")
        candidates: list[dt.datetime] = []
        comments = client.paginate(
            f"/repos/{repo}/issues/{number}/comments",
            {"per_page": 100},
            max_pages=1,
            optional=True,
        )
        for comment in comments:
            user = comment.get("user")
            if is_human_user(user) and user.get("login") != author_login:
                comment_time = parse_time(comment.get("created_at"))
                if comment_time:
                    candidates.append(comment_time)
        if "pull_request" in item:
            reviews = client.paginate(
                f"/repos/{repo}/pulls/{number}/reviews",
                {"per_page": 100},
                max_pages=1,
                optional=True,
            )
            for review in reviews:
                user = review.get("user")
                if is_human_user(user) and user.get("login") != author_login:
                    review_time = parse_time(review.get("submitted_at"))
                    if review_time:
                        candidates.append(review_time)
        sampled += 1
        if candidates:
            first = min(candidates)
            if first >= created_at:
                durations.append((first - created_at).total_seconds() / 3600)
    return {
        "ttfr_median_hours_30d": median(durations),
        "ttfr_sample_size_30d": sampled,
        "ttfr_responded_count_30d": len(durations),
    }


def collect_pr_community_metrics(
    client: GitHubClient,
    repo: str,
    since: str,
    max_prs: int = 20,
) -> dict[str, Any]:
    merged_prs = search_items(client, repo, f"is:pr is:merged merged:>={since}", max_pages=3)
    human_merged = [item for item in merged_prs if is_human_user(item.get("user"))]
    external_merged = [item for item in human_merged if is_external_author(item)]

    external_merge_hours: list[float] = []
    for item in external_merged:
        created_at = parse_time(item.get("created_at"))
        closed_at = parse_time(item.get("closed_at"))
        if not created_at or not closed_at:
            continue
        external_merge_hours.append((closed_at - created_at).total_seconds() / 3600)

    reviewer_counts: dict[str, int] = {}
    agent_prs = 0
    agent_prs_with_human_review = 0
    for item in merged_prs[:max_prs]:
        number = item.get("number")
        if not number:
            continue
        reviews = client.paginate(
            f"/repos/{repo}/pulls/{number}/reviews",
            {"per_page": 100},
            max_pages=1,
            optional=True,
        )
        human_reviewers: set[str] = set()
        for review in reviews:
            user = review.get("user")
            if is_human_user(user):
                login = user.get("login")
                if login:
                    reviewer_counts[login] = reviewer_counts.get(login, 0) + 1
                    human_reviewers.add(login)
        if is_agent_user(item.get("user")):
            agent_prs += 1
            if human_reviewers:
                agent_prs_with_human_review += 1

    return {
        "external_contribution_share_30d": percentage(len(external_merged), len(human_merged)),
        "merged_prs_30d": len(merged_prs),
        "human_merged_prs_30d": len(human_merged),
        "external_merged_prs_30d": len(external_merged),
        "external_pr_merge_median_hours_30d": median(external_merge_hours),
        "external_pr_merge_sample_size_30d": len(external_merge_hours),
        "reviewer_concentration_factor_30d": concentration_factor(reviewer_counts),
        "unique_reviewers_30d": len(reviewer_counts),
        "agent_pr_human_review_coverage_30d": percentage(
            agent_prs_with_human_review,
            agent_prs,
        ),
        "agent_merged_prs_30d": agent_prs,
    }


def concentration_factor(counts: dict[str, int]) -> int | None:
    total = sum(counts.values())
    if total == 0:
        return None
    running = 0
    for index, count in enumerate(sorted(counts.values(), reverse=True), start=1):
        running += count
        if running >= total * 0.5:
            return index
    return None


def collect_issue_category_metrics(
    client: GitHubClient,
    repo: str,
    since: str,
) -> dict[str, Any]:
    issues = client.paginate(
        f"/repos/{repo}/issues",
        {"state": "all", "since": f"{since}T00:00:00Z", "per_page": 100},
        max_pages=5,
    )
    non_pr_issues = [issue for issue in issues if "pull_request" not in issue]
    created_recent = [issue for issue in non_pr_issues if str(issue.get("created_at", ""))[:10] >= since]
    docs_install = 0
    bugs = 0
    regressions = 0
    for issue in created_recent:
        labels = {str(label.get("name", "")).lower() for label in issue.get("labels", [])}
        title = str(issue.get("title") or "").lower()
        haystack = " ".join(sorted(labels)) + " " + title
        if any(token in haystack for token in ("doc", "install", "quickstart", "tutorial")):
            docs_install += 1
        if "bug" in haystack:
            bugs += 1
        if "regression" in haystack:
            regressions += 1
    return {
        "docs_install_issues_created_30d": docs_install,
        "bug_issues_created_30d": bugs,
        "regression_issues_created_30d": regressions,
    }


def collect_release_metrics(client: GitHubClient, repo: str, now: dt.datetime) -> dict[str, Any]:
    releases = client.request_json(f"/repos/{repo}/releases", {"per_page": 20}, optional=True) or []
    total_downloads = 0
    latest: dict[str, Any] | None = None
    if releases:
        latest = releases[0]
    for release in releases:
        for asset in release.get("assets", []):
            total_downloads += int(asset.get("download_count") or 0)
    latest_downloads = 0
    latest_age_days = None
    latest_tag = None
    if latest:
        latest_tag = latest.get("tag_name")
        for asset in latest.get("assets", []):
            latest_downloads += int(asset.get("download_count") or 0)
        published_at = parse_time(latest.get("published_at"))
        if published_at:
            latest_age_days = (now - published_at).days
    return {
        "release_downloads_total": total_downloads,
        "latest_release_downloads": latest_downloads,
        "latest_release_age_days": latest_age_days,
        "latest_release_tag": latest_tag,
    }


def collect_security_metrics(client: GitHubClient, repo: str) -> dict[str, Any]:
    dependabot_alerts = client.paginate(
        f"/repos/{repo}/dependabot/alerts",
        {"state": "open", "per_page": 100},
        max_pages=5,
        optional=True,
    )
    code_alerts = client.paginate(
        f"/repos/{repo}/code-scanning/alerts",
        {"state": "open", "per_page": 100},
        max_pages=5,
        optional=True,
    )
    return {
        "dependabot_alerts_open": len(dependabot_alerts) if dependabot_alerts is not None else None,
        "code_scanning_alerts_open": len(code_alerts) if code_alerts is not None else None,
    }


def collect_local_metrics(repo_root: Path) -> dict[str, Any]:
    pack_files = sorted(repo_root.glob("examples/**/pack.toml"))
    formula_files = sorted(repo_root.glob("examples/**/*.formula.toml"))
    tutorial_files = sorted((repo_root / "docs" / "tutorials").glob("*.md"))
    quickstart = repo_root / "docs" / "getting-started" / "quickstart.md"
    installation = repo_root / "docs" / "getting-started" / "installation.md"
    return {
        "pack_count": len(pack_files),
        "formula_count": len(formula_files),
        "tutorial_doc_count": len(tutorial_files),
        "quickstart_docs_present": quickstart.exists(),
        "installation_docs_present": installation.exists(),
    }


def collect_snapshot(repo: str, repo_root: Path, token: str, now: dt.datetime | None = None) -> dict[str, Any]:
    now = now or utcnow()
    client = GitHubClient(token)
    since_7 = iso_date(WINDOW_DAYS, now)
    since_30 = iso_date(COMMUNITY_WINDOW_DAYS, now)
    stale_pr_before = iso_date(STALE_PR_DAYS, now)
    stale_issue_before = iso_date(STALE_ISSUE_DAYS, now)

    repo_payload = client.request_json(f"/repos/{repo}")
    traffic_views = client.request_json(f"/repos/{repo}/traffic/views", optional=True) or {}
    traffic_clones = client.request_json(f"/repos/{repo}/traffic/clones", optional=True) or {}
    top_referrers = client.request_json(f"/repos/{repo}/traffic/popular/referrers", optional=True) or []
    top_paths = client.request_json(f"/repos/{repo}/traffic/popular/paths", optional=True) or []
    profile = client.request_json(f"/repos/{repo}/community/profile", optional=True) or {}

    workflow_metrics = collect_workflow_metrics(client, repo, since_7)
    community_metrics = collect_first_response_metrics(client, repo, since_30)
    community_metrics.update(collect_pr_community_metrics(client, repo, since_30))
    issue_category_metrics = collect_issue_category_metrics(client, repo, since_30)
    release_metrics = collect_release_metrics(client, repo, now)
    security_metrics = collect_security_metrics(client, repo)
    local_metrics = collect_local_metrics(repo_root)

    metrics: dict[str, dict[str, Any]] = {
        "github_stars": metric(repo_payload.get("stargazers_count"), "count", "GitHub repository API"),
        "github_forks": metric(repo_payload.get("forks_count"), "count", "GitHub repository API"),
        "open_issues": metric(repo_payload.get("open_issues_count"), "count", "GitHub repository API"),
        "traffic_views_14d": metric(traffic_views.get("count"), "views", "GitHub traffic API"),
        "traffic_views_14d_uniques": metric(traffic_views.get("uniques"), "visitors", "GitHub traffic API"),
        "traffic_clones_14d": metric(traffic_clones.get("count"), "clones", "GitHub traffic API"),
        "traffic_clones_14d_uniques": metric(traffic_clones.get("uniques"), "cloners", "GitHub traffic API"),
        "release_downloads_total": metric(release_metrics["release_downloads_total"], "downloads", "GitHub releases API"),
        "latest_release_downloads": metric(release_metrics["latest_release_downloads"], "downloads", "GitHub releases API"),
        "latest_release_age_days": metric(release_metrics["latest_release_age_days"], "days", "GitHub releases API"),
        "community_profile_score": metric(profile.get("health_percentage"), "percent", "GitHub community profile API"),
        "ci_success_rate_7d": metric(workflow_metrics["ci_success_rate_7d"], "percent", "GitHub Actions API"),
        "ci_completed_runs_7d": metric(workflow_metrics["ci_completed_runs_7d"], "runs", "GitHub Actions API"),
        "ci_median_duration_minutes_7d": metric(workflow_metrics["ci_median_duration_minutes_7d"], "minutes", "GitHub Actions API"),
        "ttfr_median_hours_30d": metric(community_metrics["ttfr_median_hours_30d"], "hours", "GitHub issues and PR APIs"),
        "ttfr_sample_size_30d": metric(community_metrics["ttfr_sample_size_30d"], "items", "GitHub issues and PR APIs"),
        "external_contribution_share_30d": metric(community_metrics["external_contribution_share_30d"], "percent", "GitHub PR API"),
        "external_pr_merge_median_hours_30d": metric(community_metrics["external_pr_merge_median_hours_30d"], "hours", "GitHub PR API"),
        "external_pr_merge_sample_size_30d": metric(community_metrics["external_pr_merge_sample_size_30d"], "PRs", "GitHub PR API"),
        "reviewer_concentration_factor_30d": metric(community_metrics["reviewer_concentration_factor_30d"], "reviewers", "GitHub PR reviews API"),
        "unique_reviewers_30d": metric(community_metrics["unique_reviewers_30d"], "reviewers", "GitHub PR reviews API"),
        "agent_pr_human_review_coverage_30d": metric(community_metrics["agent_pr_human_review_coverage_30d"], "percent", "GitHub PR reviews API"),
        "agent_merged_prs_30d": metric(community_metrics["agent_merged_prs_30d"], "PRs", "GitHub PR API"),
        "issues_opened_7d": metric(search_count(client, repo, f"is:issue created:>={since_7}"), "issues", "GitHub search API"),
        "issues_closed_7d": metric(search_count(client, repo, f"is:issue closed:>={since_7}"), "issues", "GitHub search API"),
        "prs_opened_7d": metric(search_count(client, repo, f"is:pr created:>={since_7}"), "PRs", "GitHub search API"),
        "prs_closed_7d": metric(search_count(client, repo, f"is:pr closed:>={since_7}"), "PRs", "GitHub search API"),
        "prs_merged_7d": metric(search_count(client, repo, f"is:pr is:merged merged:>={since_7}"), "PRs", "GitHub search API"),
        "open_prs": metric(search_count(client, repo, "is:pr is:open"), "PRs", "GitHub search API"),
        "stale_prs_14d": metric(search_count(client, repo, f"is:pr is:open updated:<{stale_pr_before}"), "PRs", "GitHub search API"),
        "stale_issues_30d": metric(search_count(client, repo, f"is:issue is:open updated:<{stale_issue_before}"), "issues", "GitHub search API"),
        "docs_install_issues_created_30d": metric(issue_category_metrics["docs_install_issues_created_30d"], "issues", "GitHub issues API"),
        "bug_issues_created_30d": metric(issue_category_metrics["bug_issues_created_30d"], "issues", "GitHub issues API"),
        "regression_issues_created_30d": metric(issue_category_metrics["regression_issues_created_30d"], "issues", "GitHub issues API"),
        "dependabot_alerts_open": metric(security_metrics["dependabot_alerts_open"], "alerts", "Dependabot alerts API"),
        "code_scanning_alerts_open": metric(security_metrics["code_scanning_alerts_open"], "alerts", "Code scanning alerts API"),
        "pack_count": metric(local_metrics["pack_count"], "packs", "repository checkout"),
        "formula_count": metric(local_metrics["formula_count"], "formulas", "repository checkout"),
        "pack_changes_7d": metric(len(client.paginate(f"/repos/{repo}/commits", {"path": "examples", "since": f"{since_7}T00:00:00Z", "per_page": 100}, optional=True)), "commits", "GitHub commits API"),
        "tutorial_doc_count": metric(local_metrics["tutorial_doc_count"], "docs", "repository checkout"),
        "quickstart_docs_present": metric(local_metrics["quickstart_docs_present"], "boolean", "repository checkout"),
        "installation_docs_present": metric(local_metrics["installation_docs_present"], "boolean", "repository checkout"),
    }

    return {
        "schema_version": SCHEMA_VERSION,
        "snapshot_date": now.date().isoformat(),
        "generated_at": now.isoformat().replace("+00:00", "Z"),
        "repository": repo,
        "windows": {
            "activity_days": WINDOW_DAYS,
            "community_days": COMMUNITY_WINDOW_DAYS,
            "traffic_days": 14,
        },
        "latest_release": {
            "tag": release_metrics["latest_release_tag"],
            "age_days": release_metrics["latest_release_age_days"],
            "downloads": release_metrics["latest_release_downloads"],
        },
        "workflow_latest": workflow_metrics["workflow_latest"],
        "top_referrers": top_referrers[:10],
        "top_paths": top_paths[:10],
        "metrics": metrics,
    }


def load_snapshots(path: Path) -> list[dict[str, Any]]:
    if not path.exists():
        return []
    snapshots: list[dict[str, Any]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        stripped = line.strip()
        if stripped:
            snapshots.append(json.loads(stripped))
    return snapshots


def write_snapshots(path: Path, snapshots: list[dict[str, Any]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    ordered = sorted(snapshots, key=lambda item: item["snapshot_date"])
    content = "".join(json.dumps(item, sort_keys=True, separators=(",", ":")) + "\n" for item in ordered)
    path.write_text(content, encoding="utf-8")


def upsert_snapshot(snapshots: list[dict[str, Any]], snapshot: dict[str, Any]) -> list[dict[str, Any]]:
    filtered = [item for item in snapshots if item.get("snapshot_date") != snapshot["snapshot_date"]]
    filtered.append(snapshot)
    return sorted(filtered, key=lambda item: item["snapshot_date"])


def metric_value(snapshot: dict[str, Any], key: str) -> Any:
    return (snapshot.get("metrics", {}).get(key) or {}).get("value")


def value_series(snapshots: list[dict[str, Any]], key: str, limit: int = 8) -> list[Any]:
    values = []
    for snapshot in snapshots[-limit:]:
        values.append(metric_value(snapshot, key))
    return values


def sparkline(values: list[Any]) -> str:
    numeric = [float(value) for value in values if isinstance(value, (int, float)) and not isinstance(value, bool)]
    if not numeric:
        return "n/a"
    blocks = "▁▂▃▄▅▆▇█"
    low = min(numeric)
    high = max(numeric)
    if low == high:
        return blocks[3] * len(numeric)
    rendered = []
    for value in numeric:
        index = round((value - low) / (high - low) * (len(blocks) - 1))
        rendered.append(blocks[index])
    return "".join(rendered)


def delta_text(values: list[Any]) -> str:
    numeric = [float(value) for value in values if isinstance(value, (int, float)) and not isinstance(value, bool)]
    if len(numeric) < 2:
        return "n/a"
    delta = numeric[-1] - numeric[0]
    if delta > 0:
        return f"+{human_number(delta)}"
    if delta < 0:
        return human_number(delta)
    return "0"


def status_for(key: str, value: Any) -> str:
    if value is None:
        return "No data"
    if key in {"ci_success_rate_7d", "agent_pr_human_review_coverage_30d"}:
        return "Healthy" if value >= 90 else "Watch" if value >= 75 else "Needs attention"
    if key == "community_profile_score":
        return "Healthy" if value >= 90 else "Watch" if value >= 80 else "Needs attention"
    if key == "ttfr_median_hours_30d":
        return "Healthy" if value <= 48 else "Watch" if value <= 96 else "Needs attention"
    if key == "latest_release_age_days":
        return "Healthy" if value <= 30 else "Watch" if value <= 60 else "Needs attention"
    if key in {"stale_prs_14d", "regression_issues_created_30d"}:
        return "Healthy" if value == 0 else "Watch" if value <= 5 else "Needs attention"
    if key in {"dependabot_alerts_open", "code_scanning_alerts_open"}:
        return "Healthy" if value == 0 else "Watch" if value <= 5 else "Needs attention"
    if isinstance(value, bool):
        return "Healthy" if value else "Needs attention"
    return "Track"


def render_metric_table(
    snapshots: list[dict[str, Any]],
    rows: list[tuple[str, str]],
) -> list[str]:
    latest = snapshots[-1]
    lines = ["| Metric | Current | Trend | Delta | Status |", "|---|---:|---|---:|---|"]
    for key, label in rows:
        current = metric_value(latest, key)
        values = value_series(snapshots, key, limit=8)
        unit = (latest.get("metrics", {}).get(key) or {}).get("unit", "")
        current_text = human_number(current)
        if current is not None and unit == "percent":
            current_text = f"{current_text}%"
        elif current is not None and unit and unit not in {"boolean", "count"}:
            current_text = f"{current_text} {display_unit(current, unit)}"
        lines.append(
            f"| {label} | {current_text} | {sparkline(values)} | {delta_text(values)} | {status_for(key, current)} |"
        )
    return lines


def render_workflows(snapshot: dict[str, Any]) -> list[str]:
    lines = ["| Workflow | Latest conclusion | Updated |", "|---|---|---|"]
    for workflow in snapshot.get("workflow_latest", []):
        name = workflow.get("workflow") or "Unknown"
        conclusion = workflow.get("conclusion") or "n/a"
        updated = workflow.get("updated_at") or "n/a"
        url = workflow.get("html_url")
        if url:
            conclusion = f"[{conclusion}]({url})"
        lines.append(f"| {name} | {conclusion} | {updated} |")
    return lines


def render_top_table(items: list[dict[str, Any]], label_key: str) -> list[str]:
    if not items:
        return ["No data available."]
    lines = ["| Source | Views | Unique visitors |", "|---|---:|---:|"]
    for item in items[:5]:
        label = item.get(label_key) or item.get("path") or "unknown"
        lines.append(f"| `{label}` | {human_number(item.get('count'))} | {human_number(item.get('uniques'))} |")
    return lines


def render_dashboard(snapshots: list[dict[str, Any]]) -> str:
    if not snapshots:
        return "\n".join(
            [
                "# Project Dashboard",
                "",
                "No metric snapshots have been collected yet.",
                "",
            ]
        )
    latest = snapshots[-1]
    repo = latest.get("repository", DEFAULT_REPO)
    release = latest.get("latest_release", {})
    lines: list[str] = [
        "<!-- Generated by .github/workflows/project-dashboard.yml. Do not edit this wiki page by hand. -->",
        "# Project Dashboard",
        "",
        f"Last updated: `{latest.get('generated_at')}` for `{repo}`.",
        "",
        (
            f"Latest release: `{release.get('tag') or 'n/a'}`; "
            f"snapshot history: `{len(snapshots)}` weekly sample"
            f"{'' if len(snapshots) == 1 else 's'}."
        ),
        "",
        "## Executive Signals",
        "",
    ]
    lines.extend(
        render_metric_table(
            snapshots,
            [
                ("traffic_views_14d_uniques", "Unique visitors, 14d"),
                ("traffic_clones_14d_uniques", "Unique cloners, 14d"),
                ("release_downloads_total", "Release downloads, total"),
                ("ci_success_rate_7d", "CI success rate, 7d"),
                ("ttfr_median_hours_30d", "Median first human response, 30d"),
                ("community_profile_score", "Community profile score"),
            ],
        )
    )
    lines.extend(
        [
            "",
            "## Reach and Adoption",
            "",
        ]
    )
    lines.extend(
        render_metric_table(
            snapshots,
            [
                ("github_stars", "Stars"),
                ("github_forks", "Forks"),
                ("traffic_views_14d", "Repository views, 14d"),
                ("traffic_clones_14d", "Repository clones, 14d"),
                ("latest_release_downloads", "Latest release downloads"),
                ("latest_release_age_days", "Latest release age"),
            ],
        )
    )
    lines.extend(
        [
            "",
            "## Product Confidence",
            "",
        ]
    )
    lines.extend(
        render_metric_table(
            snapshots,
            [
                ("ci_completed_runs_7d", "Completed workflow runs, 7d"),
                ("ci_median_duration_minutes_7d", "Median workflow duration, 7d"),
                ("quickstart_docs_present", "Quickstart docs present"),
                ("installation_docs_present", "Installation docs present"),
                ("tutorial_doc_count", "Tutorial docs"),
            ],
        )
    )
    lines.extend(["", "### Latest Workflow Status", ""])
    lines.extend(render_workflows(latest))
    lines.extend(["", "## Community", ""])
    lines.extend(
        render_metric_table(
            snapshots,
            [
                ("issues_opened_7d", "Issues opened, 7d"),
                ("issues_closed_7d", "Issues closed, 7d"),
                ("prs_opened_7d", "PRs opened, 7d"),
                ("prs_merged_7d", "PRs merged, 7d"),
                ("open_prs", "Open PRs"),
                ("stale_prs_14d", "Stale open PRs"),
                ("external_contribution_share_30d", "External contribution share, 30d"),
                ("external_pr_merge_median_hours_30d", "External PR merge time, 30d"),
                ("reviewer_concentration_factor_30d", "Reviewer concentration factor, 30d"),
                ("agent_pr_human_review_coverage_30d", "Human-reviewed agent PRs, 30d"),
            ],
        )
    )
    lines.extend(["", "## Quality and Security", ""])
    lines.extend(
        render_metric_table(
            snapshots,
            [
                ("stale_issues_30d", "Stale open issues"),
                ("docs_install_issues_created_30d", "Docs/install issues created, 30d"),
                ("bug_issues_created_30d", "Bug issues created, 30d"),
                ("regression_issues_created_30d", "Regression issues created, 30d"),
                ("dependabot_alerts_open", "Open Dependabot alerts"),
                ("code_scanning_alerts_open", "Open code scanning alerts"),
            ],
        )
    )
    lines.extend(["", "## Ecosystem", ""])
    lines.extend(
        render_metric_table(
            snapshots,
            [
                ("pack_count", "Example packs"),
                ("formula_count", "Example formulas"),
                ("pack_changes_7d", "Example/pack commits, 7d"),
            ],
        )
    )
    lines.extend(["", "## Discovery", "", "### Top Referrers", ""])
    lines.extend(render_top_table(latest.get("top_referrers", []), "referrer"))
    lines.extend(["", "### Top Paths", ""])
    lines.extend(render_top_table(latest.get("top_paths", []), "path"))
    lines.extend(
        [
            "",
            "## Notes",
            "",
            "- Traffic metrics come from GitHub's recent traffic window and are persisted here so they can trend over time.",
            "- Agent and bot filtering is heuristic; users with bot-like names are excluded from human-response metrics.",
            "- Security alert counts can show `n/a` when the token used by the workflow cannot read the alert APIs.",
            "- Raw snapshots are stored in `docs/project-dashboard/snapshots.jsonl` on the main branch.",
            "",
        ]
    )
    return "\n".join(lines)


def collect_command(args: argparse.Namespace) -> int:
    snapshot_path = Path(args.snapshot_file)
    repo_root = Path(args.repo_root)
    token = os.environ.get("GITHUB_TOKEN", "")
    snapshot = collect_snapshot(args.repo, repo_root, token)
    snapshots = upsert_snapshot(load_snapshots(snapshot_path), snapshot)
    write_snapshots(snapshot_path, snapshots)
    if args.output:
        Path(args.output).write_text(render_dashboard(snapshots), encoding="utf-8")
    return 0


def render_command(args: argparse.Namespace) -> int:
    snapshots = load_snapshots(Path(args.snapshot_file))
    Path(args.output).parent.mkdir(parents=True, exist_ok=True)
    Path(args.output).write_text(render_dashboard(snapshots), encoding="utf-8")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    collect = subparsers.add_parser("collect", help="Collect a new snapshot and render the dashboard")
    collect.add_argument("--repo", default=os.environ.get("GITHUB_REPOSITORY", DEFAULT_REPO))
    collect.add_argument("--repo-root", default=".")
    collect.add_argument("--snapshot-file", required=True)
    collect.add_argument("--output")
    collect.set_defaults(func=collect_command)

    render = subparsers.add_parser("render", help="Render the dashboard from existing snapshots")
    render.add_argument("--snapshot-file", required=True)
    render.add_argument("--output", required=True)
    render.set_defaults(func=render_command)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    sys.exit(main())
