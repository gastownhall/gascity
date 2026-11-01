"""Compare the pinned mail image with and without the GitPython 3.2 cache."""

import argparse
import json
from pathlib import Path
import statistics
import subprocess
import sys


parser = argparse.ArgumentParser()
parser.add_argument("--image", required=True)
parser.add_argument("--baseline-wheel", required=True, type=Path)
parser.add_argument("--burst-script", required=True, type=Path)
args = parser.parse_args()

if not args.baseline_wheel.is_file() or not args.burst_script.is_file():
    raise SystemExit("baseline wheel and burst script must exist")

baseline_wheel_in_container = f"/tmp/{args.baseline_wheel.name}"


def run_arm(variant, trial):
    name = f"{variant}-{trial}"
    common = [
        "docker",
        "run",
        "--rm",
        "--cpus",
        "2",
        "--memory",
        "4g",
        "-v",
        f"{args.burst_script}:/tmp/mcp-mail-archive-burst.py:ro",
    ]
    if variant == "patched":
        command = common + [
            "--entrypoint",
            "python3",
            args.image,
            "/tmp/mcp-mail-archive-burst.py",
            name,
        ]
    elif variant == "baseline":
        command = common + [
            "-v",
            f"{args.baseline_wheel}:{baseline_wheel_in_container}:ro",
            "--entrypoint",
            "/bin/sh",
            args.image,
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
    else:
        raise AssertionError(variant)

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
    return value


pairs = []
for trial, order in enumerate(
    (("baseline", "patched"), ("patched", "baseline"), ("baseline", "patched")),
    start=1,
):
    pair = {variant: run_arm(variant, trial) for variant in order}
    ratios = {
        metric: pair["patched"][metric] / pair["baseline"][metric]
        for metric in ("wall", "commit_p95", "archive_wait_p95")
    }
    print(f"PAIR {trial} ratios={json.dumps(ratios, sort_keys=True)}", flush=True)
    pairs.append((pair, ratios))

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
