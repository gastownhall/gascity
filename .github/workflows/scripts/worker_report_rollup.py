#!/usr/bin/env python3

import argparse
import glob
import json
import os
import sys
from collections import Counter
from datetime import datetime, timezone


SCHEMA_VERSION = "gc.worker.conformance.rollup.v2"
KNOWN_STATUSES = [
    "pass",
    "fail",
    "unsupported",
    "environment_error",
    "provider_incident",
    "flaky_live",
    "not_certifiable_live",
]
TOP_EVIDENCE_LIMIT = 10
TOP_EVIDENCE_PREVIEW_KEYS = 3
PLANNED_HOOKS = [
    {
        "name": "live_smoke",
        "suite": "worker-inference",
        "artifact": "worker-inference-summary-reports",
        "planned": True,
        "status": "planned",
    },
    {
        "name": "e2e_smoke",
        "suite": "worker-e2e-smoke",
        "artifact": "worker-e2e-smoke-summary-reports",
        "planned": True,
        "status": "planned",
    },
]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("report_dir")
    parser.add_argument("--output", default="")
    parser.add_argument("--title", default="Worker Conformance Rollup")
    parser.add_argument("--require-reports", action="store_true")
    parser.add_argument(
        "--expected-profile",
        action="append",
        default=[],
        help="Expected profile and download outcome in the form profile=outcome",
    )
    parser.add_argument(
        "--baseline",
        default="",
        help="Optional baseline report directory or report JSON file",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    paths = sorted(
        glob.glob(os.path.join(args.report_dir, "**", "*.json"), recursive=True)
    )

    expected_profiles = parse_expected_profiles(args.expected_profile)
    baseline_state = load_baseline_state(args.baseline) if args.baseline else None
    rollup = build_rollup(paths, args.report_dir, args.title, expected_profiles, baseline_state)
    if args.require_reports and not paths:
        rollup["summary"]["status"] = "fail"
        rollup["summary"]["failure_detail"] = (
            f"no worker reports found under {args.report_dir}"
        )
    if args.output:
        output_dir = os.path.dirname(args.output)
        if output_dir:
            os.makedirs(output_dir, exist_ok=True)
        with open(args.output, "w", encoding="utf-8") as handle:
            json.dump(rollup, handle, indent=2)
            handle.write("\n")

    summary_path = os.environ.get("GITHUB_STEP_SUMMARY", "").strip()
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as out:
            write_summary(out, rollup)
    if args.require_reports and not paths:
        print(rollup["summary"]["failure_detail"], file=sys.stderr)
        return 1
    return 0


def build_rollup(
    paths: list[str],
    report_dir: str,
    title: str,
    expected_profiles: dict[str, str],
    baseline_state: dict | None = None,
) -> dict:
    current_state = collect_state(paths, report_dir)
    summary = current_state["summary"]
    reports = current_state["reports"]
    result_status_by_key = current_state["result_status_by_key"]

    overall_status = rollup_status(summary["status_counts"])

    missing_profiles = sorted(
        profile for profile in expected_profiles if profile not in summary["profiles"]
    )
    download_failures = {
        profile: outcome
        for profile, outcome in expected_profiles.items()
        if outcome != "success"
    }
    if missing_profiles or download_failures:
        overall_status = "fail"

    rollup = {
        "schema_version": SCHEMA_VERSION,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "title": title,
        "summary": {
            "status": overall_status,
            "total_reports": summary["total_reports"],
            "passed_reports": summary["status_counts"]["pass"],
            "failed_reports": summary["status_counts"]["fail"],
            "unsupported_reports": summary["status_counts"]["unsupported"],
            "environment_error_reports": summary["status_counts"]["environment_error"],
            "provider_incident_reports": summary["status_counts"]["provider_incident"],
            "flaky_live_reports": summary["status_counts"]["flaky_live"],
            "not_certifiable_live_reports": summary["status_counts"]["not_certifiable_live"],
            "status_counts": summary["status_counts"],
            "suite_failures": summary["suite_failures"],
            "profiles": sorted(summary["profiles"]),
            "requirements": sorted(summary["requirements"]),
            "failing_requirements": sorted(summary["failing_requirements"]),
            "expected_profiles": sorted(expected_profiles),
            "missing_profiles": missing_profiles,
            "download_failures": download_failures,
            "top_evidence": summary["top_evidence"][:TOP_EVIDENCE_LIMIT],
            "top_evidence_keys": top_evidence_keys(summary["top_evidence"]),
            "hooks": PLANNED_HOOKS,
        },
        "reports": reports,
    }

    if baseline_state is not None:
        rollup["summary"]["baseline"] = summarize_baseline(baseline_state)
        rollup["summary"]["delta"] = build_delta(
            current_state,
            baseline_state,
        )

    return rollup


def collect_state(paths: list[str], report_dir: str) -> dict:
    report_root = os.path.abspath(report_dir)
    reports = []
    status_counts = {status: 0 for status in KNOWN_STATUSES}
    suite_failures = 0
    profiles = set()
    requirements = set()
    failing_requirements = set()
    top_evidence = []
    evidence_key_counts: Counter[str] = Counter()
    result_status_by_key = {}

    for path in paths:
        with open(path, encoding="utf-8") as handle:
            report = json.load(handle)
        summary = report.get("summary", {}) or {}
        metadata = report.get("metadata", {}) or {}
        status = summary.get("status", "unknown")
        if status in status_counts:
            status_counts[status] += 1
        if summary.get("suite_failed"):
            suite_failures += 1

        failing_requirements.update(summary.get("failing_requirements") or [])
        profile_filter = metadata.get("profile_filter", "").strip()
        if profile_filter and profile_filter != "all-profiles":
            profiles.add(profile_filter)

        results = report.get("results") or []
        for result in results:
            profile = str(result.get("profile", "")).strip()
            requirement = str(result.get("requirement", "")).strip()
            result_status = str(result.get("status", "unknown")).strip() or "unknown"
            if profile:
                profiles.add(profile)
            if requirement:
                requirements.add(requirement)
                result_status_by_key[result_key(profile, requirement)] = result_status

        report_relpath = os.path.relpath(path, report_root)
        digests = extract_top_evidence(report, report_relpath)
        top_evidence.extend(digests)
        for digest in digests:
            for key in digest.get("keys", []):
                evidence_key_counts[key] += 1

        reports.append(
            {
                "file": report_relpath,
                "suite": report.get("suite", ""),
                "run_id": report.get("run_id", ""),
                "profile_filter": profile_filter,
                "status": status,
                "passed": summary.get("passed", 0),
                "failed": summary.get("failed", 0),
                "unsupported": summary.get("unsupported", 0),
                "environment_errors": summary.get("environment_errors", 0),
                "provider_incidents": summary.get("provider_incidents", 0),
                "flaky_live": summary.get("flaky_live", 0),
                "not_certifiable_live": summary.get("not_certifiable_live", 0),
                "suite_failed": bool(summary.get("suite_failed")),
                "failure_detail": summary.get("failure_detail", ""),
                "failing_requirements": summary.get("failing_requirements") or [],
                "top_evidence": digests,
            }
        )

    sorted_top_evidence = sort_top_evidence(top_evidence)[:TOP_EVIDENCE_LIMIT]
    summary = {
        "status_counts": status_counts,
        "suite_failures": suite_failures,
        "profiles": profiles,
        "requirements": requirements,
        "failing_requirements": failing_requirements,
        "total_reports": len(reports),
        "top_evidence": sorted_top_evidence,
        "top_evidence_keys": [
            {"key": key, "count": count}
            for key, count in sorted(
                evidence_key_counts.items(),
                key=lambda item: (-item[1], item[0]),
            )
        ],
    }
    return {
        "summary": summary,
        "reports": reports,
        "result_status_by_key": result_status_by_key,
    }


def load_baseline_state(path: str) -> dict:
    path = path.strip()
    if not path:
        raise SystemExit("baseline path must not be empty")
    if os.path.isdir(path):
        baseline_paths = sorted(
            glob.glob(os.path.join(path, "**", "*.json"), recursive=True)
        )
        return collect_state(baseline_paths, path)

    with open(path, encoding="utf-8") as handle:
        payload = json.load(handle)
    if isinstance(payload, dict) and "results" in payload:
        return collect_state([path], os.path.dirname(path) or ".")
    raise SystemExit(
        f"unsupported baseline input {path!r}; expected report directory or report JSON"
    )


def summarize_baseline(state: dict) -> dict:
    summary = state["summary"]
    return {
        "status": rollup_status(summary["status_counts"]),
        "total_reports": summary["total_reports"],
        "status_counts": summary["status_counts"],
        "profiles": sorted(summary["profiles"]),
        "requirements": sorted(summary["requirements"]),
        "failing_requirements": sorted(summary["failing_requirements"]),
    }


def build_delta(current_state: dict, baseline_state: dict) -> dict:
    current_summary = current_state["summary"]
    baseline_summary = baseline_state["summary"]
    current_map = current_state["result_status_by_key"]
    baseline_map = baseline_state["result_status_by_key"]

    deltas = {
        status: int(current_summary["status_counts"].get(status, 0) or 0)
        - int(baseline_summary["status_counts"].get(status, 0) or 0)
        for status in KNOWN_STATUSES
    }
    keys = sorted(set(current_map) | set(baseline_map))
    newly_passing = []
    newly_failing = []
    changed = []
    for key in keys:
        current_status = current_map.get(key)
        baseline_status = baseline_map.get(key)
        if current_status == baseline_status:
            continue
        profile, requirement = split_result_key(key)
        if current_status == "pass" and baseline_status != "pass":
            newly_passing.append(
                {
                    "profile": profile,
                    "requirement": requirement,
                    "from": baseline_status or "missing",
                    "to": current_status,
                }
            )
        elif current_status == "fail" and baseline_status != "fail":
            newly_failing.append(
                {
                    "profile": profile,
                    "requirement": requirement,
                    "from": baseline_status or "missing",
                    "to": current_status,
                }
            )
        changed.append(
            {
                "profile": profile,
                "requirement": requirement,
                "from": baseline_status or "missing",
                "to": current_status or "missing",
            }
        )

    return {
        "total_reports": current_summary["total_reports"] - baseline_summary["total_reports"],
        "status_counts": deltas,
        "newly_passing_requirements": sorted(
            newly_passing,
            key=lambda item: (item["profile"], item["requirement"]),
        ),
        "newly_failing_requirements": sorted(
            newly_failing,
            key=lambda item: (item["profile"], item["requirement"]),
        ),
        "changed_support_classifications": sorted(
            changed,
            key=lambda item: (item["profile"], item["requirement"]),
        ),
    }


def sort_top_evidence(entries: list[dict]) -> list[dict]:
    def sort_key(entry: dict) -> tuple:
        return (
            evidence_severity(str(entry.get("status", "unknown"))),
            str(entry.get("profile", "")),
            str(entry.get("requirement", "")),
            str(entry.get("report", "")),
        )

    return sorted(entries, key=sort_key)


def extract_top_evidence(report: dict, report_file: str) -> list[dict]:
    summary = report.get("summary", {}) or {}
    top = summary.get("top_evidence") or []
    if not top:
        top = derive_top_evidence(report.get("results") or [])

    digests = []
    for item in top:
        keys = item.get("keys") or []
        digests.append(
            {
                "report": report_file,
                "profile": str(item.get("profile", "")).strip(),
                "requirement": str(item.get("requirement", "")).strip(),
                "status": str(item.get("status", "unknown")).strip() or "unknown",
                "detail": str(item.get("detail", "")).strip(),
                "keys": [str(key) for key in keys],
                "excerpt": str(item.get("excerpt", "")).strip(),
            }
        )
    return digests


def derive_top_evidence(results: list[dict]) -> list[dict]:
    digests = []
    for result in results:
        status = str(result.get("status", "unknown")).strip() or "unknown"
        evidence = result.get("evidence") or {}
        if not isinstance(evidence, dict) or not evidence:
            continue
        keys = sorted(str(key) for key in evidence)
        digests.append(
            {
                "profile": str(result.get("profile", "")).strip(),
                "requirement": str(result.get("requirement", "")).strip(),
                "status": status,
                "detail": str(result.get("detail", "")).strip(),
                "keys": keys,
                "excerpt": format_evidence_excerpt(evidence, keys, TOP_EVIDENCE_PREVIEW_KEYS),
            }
        )
    return sort_top_evidence(digests)


def format_evidence_excerpt(evidence: dict, keys: list[str], limit: int) -> str:
    if not keys or limit <= 0:
        return ""
    parts = []
    for key in keys[:limit]:
        value = truncate_text(str(evidence.get(key, "")), 96)
        parts.append(f'{key}="{value}"')
    return "; ".join(parts)


def truncate_text(value: str, limit: int) -> str:
    if limit <= 0:
        return ""
    if len(value) <= limit:
        return value
    if limit <= 3:
        return value[:limit]
    return value[: limit - 3] + "..."


def top_evidence_keys(entries: list[dict]) -> list[dict]:
    counts = Counter()
    for entry in entries:
        for key in entry.get("keys", []):
            counts[key] += 1
    return [
        {"key": key, "count": count}
        for key, count in sorted(counts.items(), key=lambda item: (-item[1], item[0]))
    ]


def rollup_status(status_counts: dict[str, int]) -> str:
    if status_counts["fail"] > 0:
        return "fail"
    if status_counts["flaky_live"] > 0:
        return "flaky_live"
    if status_counts["provider_incident"] > 0:
        return "provider_incident"
    if status_counts["environment_error"] > 0:
        return "environment_error"
    if status_counts["pass"] > 0:
        return "pass"
    if status_counts["not_certifiable_live"] > 0:
        return "not_certifiable_live"
    if status_counts["unsupported"] > 0:
        return "unsupported"
    return "unsupported"


def parse_expected_profiles(values: list[str]) -> dict[str, str]:
    expected = {}
    for value in values:
        profile, sep, outcome = value.partition("=")
        profile = profile.strip()
        outcome = outcome.strip()
        if not sep or not profile:
            raise SystemExit(f"invalid --expected-profile value: {value!r}")
        expected[profile] = outcome or "unknown"
    return expected


def write_summary(out, rollup: dict) -> None:
    summary = rollup["summary"]
    out.write(f"### {rollup['title']}\n")
    out.write(
        f"- status: `{summary['status']}` "
        f"({format_counts(summary)})\n"
    )
    if summary["profiles"]:
        out.write(f"- profiles: {', '.join(summary['profiles'])}\n")
    expected = summary.get("expected_profiles") or []
    if expected:
        out.write(f"- expected profiles: {', '.join(expected)}\n")
    missing = summary.get("missing_profiles") or []
    if missing:
        out.write(f"- missing profiles: {', '.join(missing)}\n")
    download_failures = summary.get("download_failures") or {}
    if download_failures:
        failures = ", ".join(
            f"{profile}={outcome}" for profile, outcome in sorted(download_failures.items())
        )
        out.write(f"- download failures: {failures}\n")
    if summary.get("baseline"):
        baseline = summary["baseline"]
        out.write(
            f"- baseline: `{baseline['status']}` "
            f"({format_counts({'status_counts': baseline['status_counts']})})\n"
        )
    if summary.get("delta"):
        delta = summary["delta"]
        out.write(
            f"- delta reports: {delta['total_reports']} "
            f"({format_delta_counts(delta['status_counts'])})\n"
        )
        if delta["newly_passing_requirements"]:
            out.write(
                "- newly passing requirements: "
                + ", ".join(
                    f"{item['profile']} {item['requirement']}"
                    for item in delta["newly_passing_requirements"]
                )
                + "\n"
            )
        if delta["newly_failing_requirements"]:
            out.write(
                "- newly failing requirements: "
                + ", ".join(
                    f"{item['profile']} {item['requirement']}"
                    for item in delta["newly_failing_requirements"]
                )
                + "\n"
            )
        if delta["changed_support_classifications"]:
            out.write(
                "- changed support classifications: "
                + ", ".join(
                    f"{item['profile']} {item['requirement']}:{item['from']}→{item['to']}"
                    for item in delta["changed_support_classifications"]
                )
                + "\n"
            )
    hooks = summary.get("hooks") or []
    if hooks:
        out.write(
            "- planned hooks: "
            + ", ".join(
                f"{hook['name']} ({hook['suite']})" for hook in hooks
            )
            + "\n"
        )
    failing = summary["failing_requirements"]
    if failing:
        out.write(f"- failing requirements: {', '.join(failing)}\n")
    evidence_keys = summary.get("top_evidence_keys") or []
    if evidence_keys:
        out.write(
            "- top evidence keys: "
            + ", ".join(
                f"{item['key']}={item['count']}" for item in evidence_keys[:5]
            )
            + "\n"
        )
    for report in rollup["reports"]:
        out.write(
            f"- `{report['file']}`: {report['status']} "
            f"({format_report_counts(report)})\n"
        )
        if report["failure_detail"]:
            out.write(f"  failure detail: {report['failure_detail']}\n")
        if report["failing_requirements"]:
            out.write(
                "  failing requirements: "
                + ", ".join(report["failing_requirements"])
                + "\n"
            )
        if report["top_evidence"]:
            out.write(
                "  top evidence: "
                + " | ".join(
                    format_report_evidence(entry) for entry in report["top_evidence"][:2]
                )
                + "\n"
            )


def format_counts(summary: dict) -> str:
    status_counts = summary.get("status_counts") or {}
    ordered = [
        ("pass", "pass reports"),
        ("fail", "fail reports"),
        ("unsupported", "unsupported reports"),
        ("environment_error", "environment_error reports"),
        ("provider_incident", "provider_incident reports"),
        ("flaky_live", "flaky_live reports"),
        ("not_certifiable_live", "not_certifiable_live reports"),
    ]
    parts = []
    for key, label in ordered:
        value = int(status_counts.get(key, 0) or 0)
        if value > 0 or key in {"pass", "fail", "unsupported"}:
            parts.append(f"{value} {label}")
    return " / ".join(parts)


def format_delta_counts(status_counts: dict[str, int]) -> str:
    ordered = [
        ("pass", "pass"),
        ("fail", "fail"),
        ("unsupported", "unsupported"),
        ("environment_error", "environment_error"),
        ("provider_incident", "provider_incident"),
        ("flaky_live", "flaky_live"),
        ("not_certifiable_live", "not_certifiable_live"),
    ]
    parts = []
    for key, label in ordered:
        value = int(status_counts.get(key, 0) or 0)
        if value != 0:
            parts.append(f"{value:+d} {label}")
    return " / ".join(parts) if parts else "0"


def format_report_counts(report: dict) -> str:
    return format_counts(
        {
            "status_counts": {
                "pass": int(report.get("passed", 0) or 0),
                "fail": int(report.get("failed", 0) or 0),
                "unsupported": int(report.get("unsupported", 0) or 0),
                "environment_error": int(report.get("environment_errors", 0) or 0),
                "provider_incident": int(report.get("provider_incidents", 0) or 0),
                "flaky_live": int(report.get("flaky_live", 0) or 0),
                "not_certifiable_live": int(report.get("not_certifiable_live", 0) or 0),
            }
        }
    )


def format_report_evidence(entry: dict) -> str:
    pieces = [
        f"{entry.get('profile', '')} {entry.get('requirement', '')} {entry.get('status', 'unknown')}".strip()
    ]
    detail = entry.get("detail", "")
    if detail:
        pieces.append(detail)
    excerpt = entry.get("excerpt", "")
    if excerpt:
        pieces.append(excerpt)
    return " :: ".join(pieces)


def result_key(profile: str, requirement: str) -> str:
    return f"{profile}\0{requirement}"


def split_result_key(key: str) -> tuple[str, str]:
    profile, requirement = key.split("\0", 1)
    return profile, requirement


def evidence_severity(status: str) -> int:
    order = {
        "fail": 0,
        "environment_error": 1,
        "provider_incident": 2,
        "flaky_live": 3,
        "not_certifiable_live": 4,
        "unsupported": 5,
    }
    return order.get(status, 99)


if __name__ == "__main__":
    raise SystemExit(main())
