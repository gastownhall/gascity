#!/usr/bin/env python3
"""Aggregate a GC_BD_TRACE text log into an idle bd-call-rate report.

Gas City's BdStore tracer (internal/beads/bdstore.go) appends lines when
GC_BD_TRACE=<path> is set:

    <RFC3339Nano> status=start|… dur=… dir=… cmd=bd args=["list" …] err=""

This script turns that log into the #2463-style table (calls, /sec, %) so an
operator can prove an idle city stays bounded after the order-tracking
self-feed fix (internal/orders checkEvent filter).

Usage:
    python3 aggregate.py TRACE.txt
    cat TRACE.txt | python3 aggregate.py
    python3 aggregate.py --self-test

Bound (acceptance heuristic for a fresh idle city, no user work):
    sustained bd calls/sec should stay well below tens-of-procs/s host
    saturation. This script exits 2 when --max-per-sec is set and exceeded.
"""

from __future__ import annotations

import argparse
import re
import sys
from collections import Counter
from collections.abc import Iterable
from datetime import datetime
from typing import Any

_LINE_RE = re.compile(
    r"^(?P<ts>\S+)\s+status=(?P<status>\S+)\s+dur=(?P<dur>\S+)\s+"
    r"dir=(?P<dir>\S+)\s+cmd=(?P<cmd>\S+)\s+args=(?P<args>\".*\"|\[.*\])\s+"
    r"err=(?P<err>\".*\")\s*$"
)
_MICROSECOND_DIGITS = 6


def _parse_ts(ts: str) -> datetime:
    ts = ts.replace("Z", "+00:00")
    if "." in ts:
        head, frac = ts.split(".", 1)
        off = ""
        for sep in ("+", "-"):
            if sep in frac:
                frac, off = frac.split(sep, 1)
                off = sep + off
                break
        frac = (frac + "0" * _MICROSECOND_DIGITS)[:_MICROSECOND_DIGITS]
        ts = f"{head}.{frac}{off}"
    return datetime.fromisoformat(ts)


def _subcommand(args_field: str) -> str:
    # args is either Go %q of a []string (["list" "--json"]) or a quoted blob.
    inner = args_field.strip()
    if inner.startswith('"') and inner.endswith('"'):
        inner = inner[1:-1]
    # Prefer first token that looks like a bd subcommand.
    tokens = re.findall(r"[A-Za-z][\w-]*", inner)
    return tokens[0] if tokens else "(none)"


def parse_lines(lines: Iterable[str]) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    for raw in lines:
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        m = _LINE_RE.match(line)
        if not m:
            continue
        # Count each invocation once at "start" (or any non-duplicate status).
        if m.group("status") not in ("start", "ok", "error", "timeout"):
            continue
        if m.group("status") != "start":
            # Prefer start lines; fall back only when start is absent later.
            continue
        if m.group("cmd") != "bd":
            continue
        records.append(
            {
                "ts": m.group("ts"),
                "sub": _subcommand(m.group("args")),
            }
        )
    return records


def aggregate(records: list[dict[str, Any]]) -> dict[str, Any]:
    by_sub: Counter[str] = Counter()
    times: list[datetime] = []
    ts_dropped = 0
    for r in records:
        by_sub[r["sub"]] += 1
        try:
            times.append(_parse_ts(r["ts"]))
        except ValueError:
            ts_dropped += 1
    total = sum(by_sub.values())
    window_s = 0.0
    if len(times) >= 2:
        window_s = (max(times) - min(times)).total_seconds()
    rate = (total / window_s) if window_s > 0 else 0.0
    return {
        "total": total,
        "window_s": window_s,
        "rate": rate,
        "ts_dropped": ts_dropped,
        "by_sub": by_sub,
    }


def format_report(report: dict[str, Any]) -> str:
    total = report["total"]
    window_s = report["window_s"]
    rate = report["rate"]
    lines = [
        f"total_bd_starts={total}",
        f"window_s={window_s:.3f}",
        f"bd_starts_per_sec={rate:.3f}",
        "",
        f"{'subcmd':<16} {'calls':>8} {'/sec':>8} {'%':>7}",
        f"{'-'*16} {'-'*8} {'-'*8} {'-'*7}",
    ]
    for sub, calls in report["by_sub"].most_common():
        per = (calls / window_s) if window_s > 0 else 0.0
        pct = (100.0 * calls / total) if total else 0.0
        lines.append(f"{sub:<16} {calls:8d} {per:8.3f} {pct:6.1f}%")
    if report["ts_dropped"]:
        lines.append(f"\n# warning: dropped {report['ts_dropped']} unparseable timestamps")
    return "\n".join(lines) + "\n"


def self_test() -> None:
    sample = """
2026-08-19T12:00:00.000000000Z status=start dur=0s dir=/tmp/city cmd=bd args=["list" "--json"] err=""
2026-08-19T12:00:00.000000000Z status=ok dur=12ms dir=/tmp/city cmd=bd args=["list" "--json"] err=""
2026-08-19T12:00:01.000000000Z status=start dur=0s dir=/tmp/city cmd=bd args=["query" "x"] err=""
2026-08-19T12:00:02.000000000Z status=start dur=0s dir=/tmp/city cmd=bd args=["show" "gc-1"] err=""
""".strip().splitlines()
    report = aggregate(parse_lines(sample))
    assert report["total"] == 3, report
    assert abs(report["window_s"] - 2.0) < 1e-6, report
    assert abs(report["rate"] - 1.5) < 1e-6, report
    assert report["by_sub"]["list"] == 1
    assert report["by_sub"]["query"] == 1
    assert report["by_sub"]["show"] == 1
    print("self-test ok")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("trace", nargs="?", help="GC_BD_TRACE log path (default: stdin)")
    ap.add_argument(
        "--max-per-sec",
        type=float,
        default=None,
        help="exit 2 if bd_starts_per_sec exceeds this bound",
    )
    ap.add_argument("--self-test", action="store_true")
    args = ap.parse_args()
    if args.self_test:
        self_test()
        return 0
    if args.trace:
        with open(args.trace, encoding="utf-8") as f:
            records = parse_lines(f)
    else:
        records = parse_lines(sys.stdin)
    report = aggregate(records)
    sys.stdout.write(format_report(report))
    if args.max_per_sec is not None and report["rate"] > args.max_per_sec:
        print(
            f"FAIL: bd_starts_per_sec={report['rate']:.3f} exceeds --max-per-sec={args.max_per_sec}",
            file=sys.stderr,
        )
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
