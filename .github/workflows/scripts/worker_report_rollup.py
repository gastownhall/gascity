#!/usr/bin/env python3

import argparse
import glob
import json
import os
import sys
from datetime import datetime, timezone


SCHEMA_VERSION = "gc.worker.conformance.rollup.v1"
KNOWN_STATUSES = [
    "pass",
    "fail",
    "unsupported",
    "environment_error",
    "provider_incident",
    "flaky_live",
    "not_certifiable_live",
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
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    paths = sorted(
        glob.glob(os.path.join(args.report_dir, "**", "*.json"), recursive=True)
    )

    expected_profiles = parse_expected_profiles(args.expected_profile)
    rollup = build_rollup(paths, args.report_dir, args.title, expected_profiles)
    if args.require_reports and not paths:
        rollup["summary"]["status"] = "fail"
        rollup["summary"]["failure_detail"] = (
            f"no worker reports found under {args.report_dir}"
        )
    if args.output:
        os.makedirs(os.path.dirname(args.output), exist_ok=True)
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
) -> dict:
    report_root = os.path.abspath(report_dir)
    reports = []
    failing_requirements = set()
    profiles = set()
    requirements = set()
    status_counts = {status: 0 for status in KNOWN_STATUSES}
    suite_failures = 0

    for path in paths:
        with open(path, encoding="utf-8") as handle:
            report = json.load(handle)
        summary = report.get("summary", {})
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
        for result in report.get("results") or []:
            profile = result.get("profile", "").strip()
            requirement = result.get("requirement", "").strip()
            if profile:
                profiles.add(profile)
            if requirement:
                requirements.add(requirement)

        reports.append(
            {
                "file": os.path.relpath(path, report_root),
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
            }
        )

    overall_status = rollup_status(status_counts)

    missing_profiles = sorted(
        profile for profile in expected_profiles if profile not in profiles
    )
    download_failures = {
        profile: outcome
        for profile, outcome in expected_profiles.items()
        if outcome != "success"
    }
    if missing_profiles or download_failures:
        overall_status = "fail"

    return {
        "schema_version": SCHEMA_VERSION,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "title": title,
        "summary": {
            "status": overall_status,
            "total_reports": len(reports),
            "passed_reports": status_counts["pass"],
            "failed_reports": status_counts["fail"],
            "unsupported_reports": status_counts["unsupported"],
            "environment_error_reports": status_counts["environment_error"],
            "provider_incident_reports": status_counts["provider_incident"],
            "flaky_live_reports": status_counts["flaky_live"],
            "not_certifiable_live_reports": status_counts["not_certifiable_live"],
            "status_counts": status_counts,
            "suite_failures": suite_failures,
            "profiles": sorted(profiles),
            "requirements": sorted(requirements),
            "failing_requirements": sorted(failing_requirements),
            "expected_profiles": sorted(expected_profiles),
            "missing_profiles": missing_profiles,
            "download_failures": download_failures,
        },
        "reports": reports,
    }


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
    failing = summary["failing_requirements"]
    if failing:
        out.write(f"- failing requirements: {', '.join(failing)}\n")
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


if __name__ == "__main__":
    raise SystemExit(main())
