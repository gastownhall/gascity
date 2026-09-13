#!/usr/bin/env python3

import copy
import glob
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from collections import Counter


DEFAULT_COMMAND = [
    "go",
    "test",
    "-tags",
    "acceptance_c",
    "-timeout",
    "45m",
    "-v",
    "./test/acceptance/worker_inference/...",
]

PASSING_STATUSES = {"pass", "flaky_live"}
RETRYABLE_STATUSES = {"provider_incident", "environment_error"}
KNOWN_STATUSES = [
    "pass",
    "fail",
    "unsupported",
    "environment_error",
    "provider_incident",
    "flaky_live",
    "not_certifiable_live",
]
DELAYED_RETRY_TERMS = [
    "approaching rate limits",
    "capacity",
    "overloaded",
    "rate limit",
    "rate_limit",
    "service unavailable",
    "temporarily unavailable",
    "too many requests",
    "try again later",
    "usage limit",
]
IMMEDIATE_RETRY_TERMS = [
    "bead store did not become ready",
    "broken pipe",
    "connection refused",
    "error connecting",
    "failed to connect",
    "no tmux server",
    "server exited unexpectedly",
    "session is initializing",
    "transport",
]
NONRETRYABLE_ENVIRONMENT_TERMS = [
    "api key is invalid",
    "authentication_error",
    "choose the text style",
    "invalid api key",
    "login required",
    "oauth token has expired",
    "please run /login",
    "trust this folder",
    "workspace trust",
]
TOP_EVIDENCE_LIMIT = 5
TOP_EVIDENCE_PREVIEW_KEYS = 3


def main() -> int:
    report_dir = os.environ.get("GC_WORKER_REPORT_DIR", "").strip()
    if not report_dir:
        return run_attempt(DEFAULT_COMMAND, {})

    profile = os.environ.get("PROFILE", "").strip() or "all-profiles"
    immediate_delay = env_seconds("GC_WORKER_INFERENCE_RETRY_IMMEDIATE_DELAY_SECONDS", 0)
    delayed_delay = env_seconds("GC_WORKER_INFERENCE_RETRY_DELAY_SECONDS", 90)

    with tempfile.TemporaryDirectory(prefix=f"worker-inference-retry-{sanitize(profile)}-") as root:
        attempt1_dir = os.path.join(root, "attempt-1")
        attempt2_dir = os.path.join(root, "attempt-2")

        attempt1_exit = run_attempt(DEFAULT_COMMAND, {"GC_WORKER_REPORT_DIR": attempt1_dir})
        attempt1_reports = load_reports(attempt1_dir)
        if not attempt1_reports:
            return attempt1_exit

        plan = build_retry_plan(attempt1_reports, immediate_delay, delayed_delay)
        if plan is None:
            write_reports(attempt1_reports, report_dir)
            return exit_code_for_reports(attempt1_reports, attempt1_exit)

        print(
            f"worker-inference retry: {plan['strategy']} retry after "
            f"{plan['delay_seconds']}s for {', '.join(plan['reasons'])}"
        )
        if plan["delay_seconds"] > 0:
            time.sleep(plan["delay_seconds"])

        attempt2_exit = run_attempt(DEFAULT_COMMAND, {"GC_WORKER_REPORT_DIR": attempt2_dir})
        attempt2_reports = load_reports(attempt2_dir)
        if not attempt2_reports:
            write_reports(attempt1_reports, report_dir)
            return attempt2_exit if attempt2_exit != 0 else attempt1_exit

        merged_reports = merge_retry_reports(
            attempt1_reports,
            attempt2_reports,
            plan,
            attempt1_exit,
            attempt2_exit,
        )
        write_reports(merged_reports, report_dir)
        return exit_code_for_reports(merged_reports, attempt2_exit)


def env_seconds(name: str, default: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer number of seconds: {raw!r}") from exc
    if value < 0:
        raise SystemExit(f"{name} must be >= 0: {raw!r}")
    return value


def run_attempt(command: list[str], env_updates: dict[str, str]) -> int:
    env = os.environ.copy()
    env.update(env_updates)
    os.makedirs(env_updates.get("GC_WORKER_REPORT_DIR", "") or ".", exist_ok=True)
    completed = subprocess.run(command, env=env, check=False)
    return int(completed.returncode)


def load_reports(report_dir: str) -> dict[str, dict]:
    reports = {}
    for path in sorted(glob.glob(os.path.join(report_dir, "*.json"))):
        with open(path, encoding="utf-8") as handle:
            reports[os.path.basename(path)] = json.load(handle)
    return reports


def build_retry_plan(
    reports: dict[str, dict],
    immediate_delay: int = 0,
    delayed_delay: int = 90,
) -> dict | None:
    modes = set()
    reasons = []
    saw_failure = False
    for report in reports.values():
        for result in report.get("results") or []:
            status = str(result.get("status", "")).strip()
            if status == "pass":
                continue
            saw_failure = True
            if status not in RETRYABLE_STATUSES:
                return None
            mode = classify_retry_mode(result)
            if mode is None:
                return None
            modes.add(mode)
            reasons.append(f"{result.get('profile', '')} {result.get('requirement', '')} {mode}".strip())
    if not saw_failure or not modes:
        return None
    strategy = "delayed" if "delayed" in modes else "immediate"
    return {
        "strategy": strategy,
        "delay_seconds": delayed_delay if strategy == "delayed" else immediate_delay,
        "reasons": reasons,
    }


def classify_retry_mode(result: dict) -> str | None:
    status = str(result.get("status", "")).strip()
    haystack = result_haystack(result)
    if contains_any(haystack, DELAYED_RETRY_TERMS):
        return "delayed"
    if status != "environment_error" and status != "provider_incident":
        return None
    if contains_any(haystack, NONRETRYABLE_ENVIRONMENT_TERMS):
        return None
    if contains_any(haystack, IMMEDIATE_RETRY_TERMS):
        return "immediate"
    return None


def result_haystack(result: dict) -> str:
    parts = [str(result.get("detail", ""))]
    evidence = result.get("evidence") or {}
    parts.extend(str(value) for value in evidence.values())
    return "\n".join(parts).lower()


def contains_any(haystack: str, needles: list[str]) -> bool:
    return any(needle in haystack for needle in needles)


def merge_retry_reports(
    initial_reports: dict[str, dict],
    retry_reports: dict[str, dict],
    plan: dict,
    initial_exit: int,
    retry_exit: int,
) -> dict[str, dict]:
    merged = {}
    names = sorted(set(initial_reports) | set(retry_reports))
    for name in names:
        initial = initial_reports.get(name)
        retry = retry_reports.get(name)
        if initial and retry:
            merged[name] = merge_report_attempts(initial, retry, plan, initial_exit, retry_exit)
            continue
        report = copy.deepcopy(retry or initial)
        if report is None:
            continue
        report["metadata"] = merge_report_metadata(
            report.get("metadata") or {},
            plan,
            initial_exit,
            retry_exit,
            (initial or {}).get("summary", {}).get("status", ""),
            (retry or {}).get("summary", {}).get("status", ""),
        )
        merged[name] = rebuild_report(report)
    return merged


def merge_report_attempts(
    initial: dict,
    retry: dict,
    plan: dict,
    initial_exit: int,
    retry_exit: int,
) -> dict:
    merged = copy.deepcopy(retry)
    merged["results"] = merge_result_sets(initial.get("results") or [], retry.get("results") or [], plan)
    merged["metadata"] = merge_report_metadata(
        retry.get("metadata") or initial.get("metadata") or {},
        plan,
        initial_exit,
        retry_exit,
        initial.get("summary", {}).get("status", ""),
        retry.get("summary", {}).get("status", ""),
    )
    merged["summary"] = compute_summary(
        merged["results"],
        suite_failed=bool((retry.get("summary") or {}).get("suite_failed")),
        failure_detail=str((retry.get("summary") or {}).get("failure_detail", "")).strip(),
    )
    return merged


def merge_report_metadata(
    metadata: dict,
    plan: dict,
    initial_exit: int,
    retry_exit: int,
    initial_status: str,
    retry_status: str,
) -> dict:
    merged = dict(metadata)
    merged["retry_performed"] = "true"
    merged["retry_strategy"] = plan["strategy"]
    merged["retry_reasons"] = "; ".join(plan.get("reasons") or [])
    merged["retry_initial_exit_code"] = str(initial_exit)
    merged["retry_final_exit_code"] = str(retry_exit)
    if initial_status:
        merged["retry_initial_report_status"] = initial_status
    if retry_status:
        merged["retry_final_report_status"] = retry_status
    return merged


def merge_result_sets(initial_results: list[dict], retry_results: list[dict], plan: dict) -> list[dict]:
    initial_by_key = {result_key(result): copy.deepcopy(result) for result in initial_results}
    retry_by_key = {result_key(result): copy.deepcopy(result) for result in retry_results}

    merged = []
    for key in sorted(set(initial_by_key) | set(retry_by_key)):
        initial = initial_by_key.get(key)
        retry = retry_by_key.get(key)
        if initial and retry:
            merged.append(merge_result_attempts(initial, retry, plan))
        elif retry:
            merged.append(copy.deepcopy(retry))
        elif initial:
            merged.append(merge_retry_failure(initial, initial, plan))
    return merged


def merge_result_attempts(initial: dict, retry: dict, plan: dict) -> dict:
    if str(initial.get("status", "")) in RETRYABLE_STATUSES and str(retry.get("status", "")) == "pass":
        merged = copy.deepcopy(retry)
        merged["status"] = "flaky_live"
        merged["detail"] = (
            f"retry-pass after initial {initial.get('status', 'unknown')}: "
            f"{str(initial.get('detail', '')).strip() or 'transient live failure'}"
        )
        merged["evidence"] = merge_retry_evidence(initial, retry, plan)
        return merged
    if str(initial.get("status", "")) in RETRYABLE_STATUSES:
        return merge_retry_failure(initial, retry, plan)
    return copy.deepcopy(retry)


def merge_retry_failure(initial: dict, retry: dict, plan: dict) -> dict:
    merged = copy.deepcopy(retry)
    detail = str(merged.get("detail", "")).strip()
    initial_detail = str(initial.get("detail", "")).strip()
    if detail:
        merged["detail"] = (
            f"{detail} (after {plan['strategy']} retry; initial "
            f"{initial.get('status', 'unknown')}: {initial_detail or 'transient live failure'})"
        )
    else:
        merged["detail"] = (
            f"retry did not clear initial {initial.get('status', 'unknown')}: "
            f"{initial_detail or 'transient live failure'}"
        )
    merged["evidence"] = merge_retry_evidence(initial, retry, plan)
    return merged


def merge_retry_evidence(initial: dict, retry: dict, plan: dict) -> dict:
    evidence = {}
    retry_evidence = copy.deepcopy((retry or {}).get("evidence") or {})
    evidence.update(retry_evidence)
    evidence["retry_performed"] = "true"
    evidence["retry_strategy"] = plan["strategy"]
    evidence["retry_initial_status"] = str(initial.get("status", "")).strip()
    evidence["retry_initial_detail"] = str(initial.get("detail", "")).strip()
    if retry:
        evidence["retry_final_status"] = str(retry.get("status", "")).strip()
        evidence["retry_final_detail"] = str(retry.get("detail", "")).strip()
    for key, value in ((initial.get("evidence") or {}).items()):
        evidence[f"retry_initial_{key}"] = value
    return evidence


def rebuild_report(report: dict) -> dict:
    rebuilt = copy.deepcopy(report)
    rebuilt["summary"] = compute_summary(
        rebuilt.get("results") or [],
        suite_failed=bool((rebuilt.get("summary") or {}).get("suite_failed")),
        failure_detail=str((rebuilt.get("summary") or {}).get("failure_detail", "")).strip(),
    )
    return rebuilt


def compute_summary(results: list[dict], suite_failed: bool = False, failure_detail: str = "") -> dict:
    counts = Counter()
    profiles = set()
    requirements = set()
    failing_profiles = set()
    failing_requirements = set()

    for result in results:
        status = str(result.get("status", "")).strip()
        counts[status] += 1
        profile = str(result.get("profile", "")).strip()
        requirement = str(result.get("requirement", "")).strip()
        if profile:
            profiles.add(profile)
        if requirement:
            requirements.add(requirement)
        if status == "fail":
            if profile:
                failing_profiles.add(profile)
            if requirement:
                failing_requirements.add(requirement)

    summary = {
        "status": summary_status(counts),
        "total": len(results),
        "passed": counts["pass"],
        "failed": counts["fail"],
        "unsupported": counts["unsupported"],
        "environment_errors": counts["environment_error"],
        "provider_incidents": counts["provider_incident"],
        "flaky_live": counts["flaky_live"],
        "not_certifiable_live": counts["not_certifiable_live"],
        "profiles": len(profiles),
        "requirements": len(requirements),
        "failing_profiles": sorted(failing_profiles),
        "failing_requirements": sorted(failing_requirements),
        "top_evidence": top_evidence(results, TOP_EVIDENCE_LIMIT),
    }
    if suite_failed:
        summary["suite_failed"] = True
        summary["failure_detail"] = failure_detail
        if summary["status"] != "fail":
            summary["status"] = "fail"
    return summary


def summary_status(counts: Counter) -> str:
    if counts["fail"] > 0:
        return "fail"
    if counts["flaky_live"] > 0:
        return "flaky_live"
    if counts["provider_incident"] > 0:
        return "provider_incident"
    if counts["environment_error"] > 0:
        return "environment_error"
    if counts["pass"] > 0:
        return "pass"
    if counts["not_certifiable_live"] > 0:
        return "not_certifiable_live"
    if counts["unsupported"] > 0:
        return "unsupported"
    return "unsupported"


def top_evidence(results: list[dict], limit: int) -> list[dict]:
    digests = []
    for result in results:
        status = str(result.get("status", "")).strip()
        evidence = result.get("evidence") or {}
        if status == "pass" or not evidence:
            continue
        keys = sorted(evidence)
        digests.append(
            {
                "profile": result.get("profile", ""),
                "requirement": result.get("requirement", ""),
                "status": status,
                "detail": result.get("detail", ""),
                "keys": keys,
                "excerpt": evidence_excerpt(evidence, keys, TOP_EVIDENCE_PREVIEW_KEYS),
            }
        )
    digests.sort(
        key=lambda digest: (
            evidence_severity(str(digest.get("status", ""))),
            str(digest.get("profile", "")),
            str(digest.get("requirement", "")),
        )
    )
    return digests[:limit]


def evidence_excerpt(evidence: dict, keys: list[str], limit: int) -> str:
    preview = []
    for key in keys[:limit]:
        preview.append(f'{key}="{truncate_value(str(evidence.get(key, "")), 96)}"')
    return "; ".join(preview)


def truncate_value(value: str, limit: int) -> str:
    if limit <= 0 or len(value) <= limit:
        return value
    if limit <= 3:
        return value[:limit]
    return value[: limit - 3] + "..."


def evidence_severity(status: str) -> int:
    if status == "fail":
        return 0
    if status == "environment_error":
        return 1
    if status == "provider_incident":
        return 2
    if status == "flaky_live":
        return 3
    if status == "not_certifiable_live":
        return 4
    if status == "unsupported":
        return 5
    return 99


def result_key(result: dict) -> tuple[str, str]:
    return (
        str(result.get("profile", "")).strip(),
        str(result.get("requirement", "")).strip(),
    )


def write_reports(reports: dict[str, dict], report_dir: str) -> None:
    os.makedirs(report_dir, exist_ok=True)
    for path in glob.glob(os.path.join(report_dir, "*.json")):
        os.remove(path)
    for name, payload in sorted(reports.items()):
        out_path = os.path.join(report_dir, name)
        with open(out_path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, indent=2)
            handle.write("\n")


def exit_code_for_reports(reports: dict[str, dict], fallback: int) -> int:
    if not reports:
        return fallback
    statuses = {
        str((report.get("summary") or {}).get("status", "")).strip()
        for report in reports.values()
    }
    return 0 if statuses and statuses.issubset(PASSING_STATUSES) else 1


def sanitize(value: str) -> str:
    value = value.strip().lower()
    if not value:
        return "unknown"
    out = []
    last_dash = False
    for ch in value:
        if ch.isalnum():
            out.append(ch)
            last_dash = False
        elif not last_dash:
            out.append("-")
            last_dash = True
    return "".join(out).strip("-") or "unknown"


if __name__ == "__main__":
    raise SystemExit(main())
