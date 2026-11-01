"""Compare the pinned mail image with and without the GitPython 3.2 cache."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import statistics
import subprocess
import sys


parser = argparse.ArgumentParser()
parser.add_argument("--image", required=True)
parser.add_argument("--baseline-wheel", required=True, type=Path)
parser.add_argument("--burst-script", required=True, type=Path)
parser.add_argument("--samples-dir", required=True, type=Path)
args = parser.parse_args()

if not args.baseline_wheel.is_file() or not args.burst_script.is_file():
    raise SystemExit("baseline wheel and burst script must exist")
args.samples_dir.mkdir(parents=True, exist_ok=True)

image_inspect = subprocess.run(
    ["docker", "image", "inspect", "--format", "{{.Id}}", args.image],
    capture_output=True,
    text=True,
    check=True,
)
image_id = image_inspect.stdout.strip()
if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
    raise SystemExit(f"docker returned an invalid image ID: {image_id!r}")
image = image_id
wheel_sha256 = hashlib.sha256(args.baseline_wheel.read_bytes()).hexdigest()
metadata = {
    "source_sha": os.environ.get("GITHUB_SHA"),
    "pr_head_sha": os.environ.get("PR_HEAD_SHA"),
    "run_id": os.environ.get("GITHUB_RUN_ID"),
    "run_attempt": os.environ.get("GITHUB_RUN_ATTEMPT"),
    "repository": os.environ.get("GITHUB_REPOSITORY"),
    "host_platform": platform.platform(),
    "host_cpu_count": os.cpu_count(),
    "container_cpu_limit": "2",
    "container_memory_limit": "4g",
    "image_tag": args.image,
    "image_id": image_id,
    "baseline_wheel_sha256": wheel_sha256,
}
print("MAIL_ARCHIVE_METADATA " + json.dumps(metadata, sort_keys=True), flush=True)

baseline_wheel_in_container = f"/tmp/{args.baseline_wheel.name}"


def run_arm(variant, trial, order):
    name = f"{variant}-{trial}"
    sample_path = args.samples_dir / f"{name}.json"
    container_sample_path = f"/tmp/mcp-mail-samples/{name}.json"
    common = [
        "docker",
        "run",
        "--rm",
        "--group-add",
        str(os.getgid()),
        "--cpus",
        "2",
        "--memory",
        "4g",
        "-v",
        f"{args.burst_script}:/tmp/mcp-mail-archive-burst.py:ro",
        "-v",
        f"{args.samples_dir.resolve()}:/tmp/mcp-mail-samples",
        "--env",
        f"MAIL_SAMPLE_PATH={container_sample_path}",
        "--env",
        f"MAIL_IMAGE_ID={image_id}",
        "--env",
        f"MAIL_SOURCE_SHA={os.environ.get('GITHUB_SHA', '')}",
        "--env",
        f"MAIL_PR_HEAD_SHA={os.environ.get('PR_HEAD_SHA', '')}",
        "--env",
        f"MAIL_RUN_ID={os.environ.get('GITHUB_RUN_ID', '')}",
        "--env",
        f"MAIL_RUN_ATTEMPT={os.environ.get('GITHUB_RUN_ATTEMPT', '')}",
        "--env",
        f"MAIL_HOST_PLATFORM={platform.platform()}",
        "--env",
        f"MAIL_HOST_CPU_COUNT={os.cpu_count() or 0}",
        "--env",
        f"MAIL_BASELINE_WHEEL_SHA256={wheel_sha256}",
        "--env",
        "MAIL_RUNNER_CPU_LIMIT=2",
        "--env",
        "MAIL_RUNNER_MEMORY_LIMIT=4g",
        "--env",
        f"MAIL_VARIANT={variant}",
        "--env",
        f"MAIL_TRIAL={trial}",
        "--env",
        f"MAIL_ARM_ORDER={order}",
    ]
    if variant != "baseline":
        command = common + [
            "--entrypoint",
            "python3",
            image,
            "/tmp/mcp-mail-archive-burst.py",
            name,
        ]
    else:
        command = common + [
            "-v",
            f"{args.baseline_wheel}:{baseline_wheel_in_container}:ro",
            "--entrypoint",
            "/bin/sh",
            image,
            "-c",
            "set -eu; "
            "python3 -m pip install --no-deps --no-cache-dir "
            f"--target /tmp/gitpython-baseline {baseline_wheel_in_container}; "
            "mkdir -p /tmp/gitpython-no-cache; "
            ": > /tmp/gitpython-no-cache/sitecustomize.py; "
            "PYTHONPATH=/tmp/gitpython-no-cache:/tmp/gitpython-baseline "
            "python3 /tmp/mcp-mail-archive-burst.py \"$1\"",
            "sh",
            name,
        ]

    result = subprocess.run(command, capture_output=True, text=True, timeout=660)
    if result.stdout:
        print(result.stdout, end="", flush=True)
    if result.stderr:
        print(result.stderr, file=sys.stderr, end="", flush=True)
    if result.returncode:
        raise SystemExit(f"{name} container exited {result.returncode}")

    records = [line.removeprefix("RESULT ") for line in result.stdout.splitlines() if line.startswith("RESULT ")]
    if len(records) != 1:
        raise SystemExit(f"{name} emitted {len(records)} result records; expected one")
    value = json.loads(records[0])
    expected_version = "3.1.62" if variant == "baseline" else "3.2.1"
    if value["gitpython"] != expected_version:
        raise SystemExit(f"{name} used GitPython {value['gitpython']}, expected {expected_version}")
    for key in ("success", "unique_requests", "inbox_count", "unique_inbox_ids", "unique_subjects"):
        if value[key] != 400:
            raise SystemExit(f"{name} {key}={value[key]}, expected 400")
    if value["errors"]:
        raise SystemExit(f"{name} returned errors: {value['errors'][:3]}")
    for key in ("request_over_60s", "archive_wait_over_60s", "commit_over_60s"):
        if value[key] != 0:
            raise SystemExit(f"{name} {key}={value[key]}, expected zero")
    if not sample_path.is_file():
        raise SystemExit(f"{name} did not write its raw diagnostic sample file")
    sample_payload = json.loads(sample_path.read_text(encoding="utf-8"))
    if sample_payload.get("image_id") != image_id:
        raise SystemExit(f"{name} sample image ID does not match the pinned image")
    summary_sha256 = hashlib.sha256(json.dumps(value, sort_keys=True).encode("utf-8")).hexdigest()
    if sample_payload.get("summary_sha256") != summary_sha256:
        raise SystemExit(f"{name} sample summary does not match the RESULT record")
    if not sample_payload.get("samples"):
        raise SystemExit(f"{name} raw diagnostic sample list is empty")
    sample_counts = value.get("sample_counts_by_phase", {})
    if sample_counts.get("send", 0) == 0:
        raise SystemExit(f"{name} has no send-phase samples")
    operation_counts = value.get("sample_counts_by_operation_phase", {})
    send_request_indices = [
        sample.get("request_index")
        for sample in sample_payload["samples"]
        if sample.get("operation") == "request_total" and sample.get("phase") == "send"
    ]
    if sorted(send_request_indices) != list(range(400)) or operation_counts.get("request_total:send") != 400:
        raise SystemExit(f"{name} did not retain all 400 send request timings")
    if value.get("send_commit_count", 0) == 0 or value.get("send_archive_lock_wait_count", 0) == 0:
        raise SystemExit(f"{name} did not attribute commit and archive-lock timings to send operations")
    send_commit_samples = [
        sample for sample in sample_payload["samples"]
        if sample.get("operation") == "commit_direct" and sample.get("phase") == "send"
    ]
    if not send_commit_samples:
        raise SystemExit(f"{name} has no send-phase Git commit samples")
    for sample in send_commit_samples:
        indices = sample.get("request_index")
        if isinstance(indices, int):
            indices = [indices]
        if not isinstance(indices, list) or not indices or any(
            not isinstance(index, int) or index < 0 or index >= 400
            for index in indices
        ):
            raise SystemExit(f"{name} has a send commit without valid request attribution")
    if operation_counts.get("inbox_fetch:inbox_fetch") != 1:
        raise SystemExit(f"{name} did not retain the inbox-fetch timing sample")
    if any(sample.get("phase") not in {"setup", "send", "inbox_fetch", "mixed"} for sample in sample_payload["samples"]):
        raise SystemExit(f"{name} has an unknown diagnostic phase")
    return value


pairs = []
for trial, order in enumerate(
    (("baseline", "patched"), ("patched", "baseline"), ("baseline", "patched")),
    start=1,
):
    pair = {variant: run_arm(variant, trial, order.index(variant) + 1) for variant in order}
    ratios = {
        metric: pair["patched"][metric] / pair["baseline"][metric]
        for metric in ("wall", "commit_p95", "archive_wait_p95")
    }
    print(f"PAIR {trial} ratios={json.dumps(ratios, sort_keys=True)}", flush=True)
    pairs.append((pair, ratios))

# Measure same-variant variation on the same pinned image. These controls are
# diagnostic only; they do not change or replace the baseline-versus-patched
# gate above.
control_pairs = []
for trial, order in enumerate(
    (
        ("patched-control-a", "patched-control-b"),
        ("patched-control-b", "patched-control-a"),
        ("patched-control-a", "patched-control-b"),
    ),
    start=1,
):
    pair = {
        name: run_arm(name, trial, order.index(name) + 1)
        for name in order
    }
    ratios = {
        metric: pair["patched-control-b"][metric] / pair["patched-control-a"][metric]
        for metric in ("wall", "commit_p95", "archive_wait_p95")
    }
    print(f"PATCHED_CONTROL {trial} ratios={json.dumps(ratios, sort_keys=True)}", flush=True)
    control_pairs.append((pair, ratios))

control_medians = {
    metric: statistics.median(ratios[metric] for _, ratios in control_pairs)
    for metric in ("wall", "commit_p95", "archive_wait_p95")
}
print(f"PATCHED_CONTROL_MEDIAN_RATIOS {json.dumps(control_medians, sort_keys=True)}", flush=True)

medians = {
    metric: statistics.median(ratios[metric] for _, ratios in pairs)
    for metric in ("wall", "commit_p95", "archive_wait_p95")
}
print(f"MEDIAN_RATIOS {json.dumps(medians, sort_keys=True)}", flush=True)

limit = 1.25
over = {metric: ratio for metric, ratio in medians.items() if ratio > limit}
if over:
    raise SystemExit(f"GitPython cache exceeds {limit:.2f}x baseline median: {over}")
print(f"MAIL_ARCHIVE_PERFORMANCE PASS max_median_ratio={limit:.2f}", flush=True)
