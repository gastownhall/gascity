#!/usr/bin/env python3
"""Aggregate a GC_BD_TRACE_JSON trace into a bd-call-rate report.

Phase 0 of the `idle-controller-call-rate` design (engdocs/design/). The
instrumentation is gc's own JSONL tracer (internal/beads/bdtrace.go, #2485),
enabled by setting GC_BD_TRACE_JSON=<path> before `gc start`. This script turns
that JSONL into:

  1. by-subcommand  — calls, /sec, % (reproduces the #2463 table)
  2. by-scope       — order-dispatch / tick-body / bead-event-watcher / hook:* /
                      cli-command / unknown (the attribution #2463 lacked)
  3. by-tick-trigger — patrol vs poke vs ...
  4. scope x subcommand cross-tab

Rate is computed over the observed window (max ts - min ts). For an idle-rate
baseline, run the city idle (no agent work) for several minutes, then point this
at the trace file.

Usage:
    python3 aggregate.py TRACE.jsonl
    cat TRACE.jsonl | python3 aggregate.py
    python3 aggregate.py --self-test
"""

from __future__ import annotations

import argparse
import json
import sys
from collections import Counter
from collections.abc import Iterable, Mapping, Sequence
from datetime import datetime
from typing import Any

# RFC3339Nano carries nanosecond precision; datetime supports at most
# microseconds, so the fraction is trimmed to this many digits.
_MICROSECOND_DIGITS = 6


def _warn(msg: str) -> None:
    """Emit a diagnostic to stderr so malformed input is never dropped silently."""
    print(f"aggregate.py: {msg}", file=sys.stderr)


def _parse_ts(ts: str) -> datetime:
    """Parse an RFC3339Nano UTC timestamp ("...Z") into a datetime.

    ``datetime.fromisoformat`` only accepts a trailing "Z" and a variable-length
    fraction on Python 3.11+, so the string is normalized by hand (swap "Z" for
    "+00:00" and trim the fraction to microseconds) to stay correct on 3.9/3.10.
    Raises ``ValueError`` on a malformed timestamp.
    """
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


def aggregate(records: Iterable[Mapping[str, Any]]) -> dict[str, Any]:
    """Pure aggregation over trace dicts. Returns a report dict.

    Records whose ``ts`` is missing or unparseable are still counted in the call
    totals but cannot contribute to the time window; their number is returned as
    ``ts_dropped`` so the caller can warn that the derived rate is computed over
    fewer timestamps than calls (and is therefore conservative, not inflated).
    """
    by_sub: Counter[str] = Counter()
    by_scope: Counter[str] = Counter()
    by_trigger: Counter[str] = Counter()
    cross: Counter[tuple[str, str]] = Counter()
    dur_by_sub: Counter[str] = Counter()
    times: list[datetime] = []
    ts_dropped = 0
    for r in records:
        args = r.get("args") or []
        sub = args[0] if args else "(none)"
        scope = r.get("scope") or "unknown"
        by_sub[sub] += 1
        by_scope[scope] += 1
        by_trigger[r.get("tick_trigger") or "(none)"] += 1
        cross[(scope, sub)] += 1
        dur_by_sub[sub] += int(r.get("dur_ms") or 0)
        ts = r.get("ts")
        if not ts:
            ts_dropped += 1
            continue
        try:
            times.append(_parse_ts(ts))
        except ValueError:
            ts_dropped += 1
    total = sum(by_sub.values())
    window_s = 0.0
    if len(times) >= 2:
        window_s = (max(times) - min(times)).total_seconds()
    return {
        "total": total,
        "window_s": window_s,
        "ts_dropped": ts_dropped,
        "by_sub": by_sub,
        "by_scope": by_scope,
        "by_trigger": by_trigger,
        "cross": cross,
        "dur_by_sub": dur_by_sub,
    }


def _fmt_table(
    title: str,
    counter: Counter[Any],
    total: int,
    window_s: float,
    extra: Mapping[Any, str] | None = None,
) -> str:
    lines = [f"\n### {title}", f"{'key':<28}{'calls':>9}{'/sec':>10}{'%':>8}"]
    for key, n in counter.most_common():
        rate = n / window_s if window_s else 0.0
        pct = 100.0 * n / total if total else 0.0
        suffix = ""
        if extra and key in extra:
            suffix = extra[key]
        lines.append(f"{str(key):<28}{n:>9}{rate:>10.2f}{pct:>7.1f}%{suffix}")
    return "\n".join(lines)


def render(report: Mapping[str, Any]) -> str:
    total: int = report["total"]
    window_s: float = report["window_s"]
    ts_dropped: int = report.get("ts_dropped", 0)
    out = [
        "# bd-call-rate report",
        f"total calls: {total}   window: {window_s:.1f}s   "
        f"overall rate: {(total / window_s if window_s else 0.0):.2f} bd/sec",
    ]
    if ts_dropped:
        out.append(
            f"_note: {ts_dropped} record(s) had a missing/unparseable ts and "
            "could not contribute to the window; the rate above is computed over "
            "the remaining timestamps._"
        )
    dur = report["dur_by_sub"]
    out.append(
        _fmt_table(
            "by subcommand",
            report["by_sub"],
            total,
            window_s,
            extra={k: f"   {dur[k]}ms total" for k in dur},
        )
    )
    out.append(_fmt_table("by scope", report["by_scope"], total, window_s))
    out.append(_fmt_table("by tick_trigger", report["by_trigger"], total, window_s))
    out.append("\n### scope x subcommand (top 20)")
    for (scope, sub), n in report["cross"].most_common(20):
        out.append(f"  {scope:<24} {sub:<10} {n:>8}")
    return "\n".join(out)


def _self_test() -> None:
    recs = [
        {
            "ts": "2026-06-16T10:00:00.000000Z",
            "args": ["list"],
            "scope": "order-dispatch",
            "tick_trigger": "patrol",
            "dur_ms": 5,
        },
        {
            "ts": "2026-06-16T10:00:05.000000Z",
            "args": ["query"],
            "scope": "tick-body",
            "tick_trigger": "patrol",
            "dur_ms": 7,
        },
        {
            "ts": "2026-06-16T10:00:10.000000Z",
            "args": ["list"],
            "scope": "order-dispatch",
            "tick_trigger": "poke",
            "dur_ms": 3,
        },
    ]
    rep = aggregate(recs)
    assert rep["total"] == 3, rep["total"]
    assert abs(rep["window_s"] - 10.0) < 1e-6, rep["window_s"]
    assert rep["by_sub"]["list"] == 2 and rep["by_sub"]["query"] == 1, rep["by_sub"]
    assert rep["by_scope"]["order-dispatch"] == 2, rep["by_scope"]
    assert rep["by_trigger"]["patrol"] == 2 and rep["by_trigger"]["poke"] == 1
    assert rep["cross"][("order-dispatch", "list")] == 2
    assert rep["dur_by_sub"]["list"] == 8
    assert rep["ts_dropped"] == 0, rep["ts_dropped"]

    # RFC3339Nano (9 fractional digits + "Z") parses, trimmed to microseconds.
    parsed = _parse_ts("2026-06-16T10:00:00.123456789Z")
    assert parsed.year == 2026 and parsed.microsecond == 123456, parsed

    # Missing / unparseable timestamps are counted and surfaced, not dropped.
    bad = aggregate(
        recs
        + [
            {"args": ["list"], "scope": "x"},  # missing ts
            {"ts": "not-a-timestamp", "args": ["query"], "scope": "y"},  # unparseable
        ]
    )
    assert bad["total"] == 5, bad["total"]
    assert bad["ts_dropped"] == 2, bad["ts_dropped"]

    # render() produces the expected structure and flags dropped timestamps.
    text = render(rep)
    assert "# bd-call-rate report" in text, text
    assert "by subcommand" in text and "order-dispatch" in text, text
    assert "_note:" in render(bad)

    # empty input must not divide by zero
    empty = aggregate([])
    assert empty["total"] == 0 and empty["window_s"] == 0.0
    print("self-test OK")


def main(argv: Sequence[str]) -> int:
    parser = argparse.ArgumentParser(
        prog="aggregate.py",
        description="Aggregate a GC_BD_TRACE_JSON trace into a bd-call-rate report.",
    )
    parser.add_argument(
        "trace",
        nargs="?",
        type=argparse.FileType("r"),
        default=sys.stdin,
        help="JSONL trace file (default: stdin; '-' also reads stdin)",
    )
    parser.add_argument(
        "--self-test",
        action="store_true",
        help="run the built-in self-test and exit",
    )
    args = parser.parse_args(list(argv[1:]))

    if args.self_test:
        _self_test()
        return 0

    records: list[dict[str, Any]] = []
    bad_lines = 0
    with args.trace as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                records.append(json.loads(line))
            except json.JSONDecodeError:
                bad_lines += 1
    if bad_lines:
        _warn(f"skipped {bad_lines} malformed JSON line(s)")
    report = aggregate(records)
    if report["ts_dropped"]:
        _warn(
            f"{report['ts_dropped']} record(s) had a missing/unparseable ts; "
            "rate is computed over the remaining timestamps"
        )
    print(render(report))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
