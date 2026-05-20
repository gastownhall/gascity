#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.12"
# ///
"""Label stranded beads with a surface label and mail a periodic digest.

A "stranded" bead is open, work-type, has no `gc.routed_to` metadata, no
assignee, no exempt label, and has not been touched for at least
GC_STRANDED_AGE_HOURS hours (default 4). Such beads will not be picked up
by any auto-route order and have no agent working on them — they would
sit invisible in the queue until a human noticed.

Each cooldown run:
  1. Walks every initialized rig and adds the surface label (default
     `needs:human`) to stranded beads. UIs and queries that filter on
     this label (e.g. the gas-ui TODO tab) surface the beads to the
     operator.
  2. Once per N days (gated by a state file), counts the still-stranded
     labeled beads across all rigs and mails the digest recipient
     (default `mayor`). Silent on every other run.

Acts as a backstop for any city where the HQ rig has no auto-route order:
`pe-*` (or equivalent) beads that nobody slung get visibility this way
without requiring a per-city auto-route order.

Configuration (all optional, sensible defaults):

  GC_STRANDED_AGE_HOURS       Hours since last update before a bead is
                              eligible for the surface label. Default 4.
  GC_STRANDED_SURFACE_LABEL   Label to add to stranded beads. Default
                              `needs:human`. Idempotency: any bead that
                              already carries this label is exempt.
  GC_STRANDED_EXEMPT_LABELS   Comma-separated additional labels that
                              exempt a bead from being labeled. The
                              surface label is always exempt; this adds
                              extras. Default `skip:auto-route`.
  GC_STRANDED_DIGEST_DAYS     Days between digest mails. Default 7.
  GC_STRANDED_DIGEST_TO       Recipient (agent name or rig/agent) of
                              the digest mail. Default `mayor`.

Run with --dry-run to print what would happen without modifying beads
or sending mail.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
from datetime import UTC, datetime, timedelta
from pathlib import Path

WORK_TYPES = {"task", "bug", "feature", "chore", "epic", "decision"}

DEFAULT_SURFACE_LABEL = "needs:human"
DEFAULT_EXTRA_EXEMPT_LABELS = ("skip:auto-route",)
DEFAULT_AGE_HOURS = 4
DEFAULT_DIGEST_DAYS = 7
DEFAULT_DIGEST_TO = "mayor"

STATE_FILENAME = "stranded-bead-digest-last"
PACK_NAME = "maintenance"
DIGEST_PREVIEW = 5


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Print what would happen; skip label writes, mail send, "
            "and state-file update."
        ),
    )
    return parser.parse_args()


def positive_int_env(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if not raw:
        return default
    try:
        n = int(raw)
    except ValueError:
        return default
    return n if n > 0 else default


def surface_label() -> str:
    return (os.environ.get("GC_STRANDED_SURFACE_LABEL") or "").strip() \
        or DEFAULT_SURFACE_LABEL


def exempt_labels(surface: str) -> tuple[str, ...]:
    """Set of labels that disqualify a bead from being labeled.

    Always includes the configured surface label (idempotency: beads
    already labeled are skipped). Additional labels are configurable via
    GC_STRANDED_EXEMPT_LABELS as a comma-separated list.
    """
    raw = os.environ.get("GC_STRANDED_EXEMPT_LABELS")
    if raw is None:
        extras = DEFAULT_EXTRA_EXEMPT_LABELS
    else:
        extras = tuple(s.strip() for s in raw.split(",") if s.strip())
    out: list[str] = [surface]
    for lbl in extras:
        if lbl and lbl not in out:
            out.append(lbl)
    return tuple(out)


def digest_interval() -> timedelta:
    return timedelta(days=positive_int_env(
        "GC_STRANDED_DIGEST_DAYS", DEFAULT_DIGEST_DAYS,
    ))


def digest_to() -> str:
    return (os.environ.get("GC_STRANDED_DIGEST_TO") or "").strip() \
        or DEFAULT_DIGEST_TO


def age_hours() -> int:
    return positive_int_env("GC_STRANDED_AGE_HOURS", DEFAULT_AGE_HOURS)


def state_path() -> Path:
    """Resolve the digest state file. Same lookup as closed-beads-digest."""
    if pack_state := os.environ.get("GC_PACK_STATE_DIR"):
        base = Path(pack_state)
    else:
        city = (
            os.environ.get("GC_CITY")
            or os.environ.get("GC_CITY_PATH")
            or os.getcwd()
        )
        runtime = (
            os.environ.get("GC_CITY_RUNTIME_DIR") or f"{city}/.gc/runtime"
        )
        base = Path(runtime) / "packs" / PACK_NAME
    return base / STATE_FILENAME


def fmt_iso(dt: datetime) -> str:
    """Format a UTC datetime with the Z suffix bd expects."""
    return dt.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def read_last_digest(path: Path) -> datetime | None:
    """Return the last digest send time, or None if never sent."""
    try:
        text = path.read_text().strip()
    except OSError:
        return None
    try:
        dt = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=UTC)
    return dt


def write_last_digest(path: Path, when: datetime) -> None:
    """Atomically write the digest send timestamp (temp file + rename)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".stranded-bead-digest-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as f:
            f.write(fmt_iso(when))
        os.replace(tmp, path)
    except OSError:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def run_json(cmd: list[str]) -> object | None:
    """Run a JSON-emitting subprocess; return parsed value or None on error."""
    result = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if result.returncode != 0 or not result.stdout.strip():
        return None
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError:
        return None


def list_rigs() -> list[dict]:
    """Return rigs with initialized bead stores, skipping suspended ones."""
    data = run_json(["gc", "rig", "list", "--json"])
    if not isinstance(data, dict):
        return []
    rigs: list[dict] = []
    for r in data.get("rigs", []):
        if not r.get("name") or not r.get("prefix"):
            continue
        if r.get("beads") != "initialized":
            continue
        if r.get("suspended"):
            continue
        rigs.append(r)
    return rigs


def bd_cmd(rig: dict) -> list[str]:
    """Return the `gc bd` prefix appropriate for this rig.

    HQ rigs use `gc bd -C <path>` because `gc bd --rig <hq-name>` is not
    supported.
    """
    if rig.get("hq"):
        return ["gc", "bd", "-C", rig["path"]]
    return ["gc", "bd", "--rig", rig["name"]]


def query_stranded_candidates(
    rig: dict, cutoff: datetime, exempt: tuple[str, ...],
) -> list[dict]:
    """Return open unassigned work beads in `rig` older than `cutoff`.

    Server-side filters cut the scan; the `gc.routed_to` and issue-type
    checks happen in Python because bd lacks "missing-metadata-key" and
    its `--type` filter only takes a single type.
    """
    cmd = bd_cmd(rig) + [
        "list",
        "--status=open",
        "--unassigned",
        f"--updated-before={fmt_iso(cutoff)}",
        f"--exclude-label={','.join(exempt)}",
        "--limit=0",
        "--json",
    ]
    data = run_json(cmd)
    if not isinstance(data, list):
        return []

    prefix = f"{rig['prefix']}-"
    keep: list[dict] = []
    for b in data:
        bid = b.get("id") or ""
        if not bid.startswith(prefix):
            continue
        if b.get("issue_type") not in WORK_TYPES:
            continue
        if b.get("ephemeral"):
            continue
        meta = b.get("metadata") or {}
        if meta.get("gc.routed_to"):
            continue
        if b.get("assignee"):
            # Belt-and-suspenders: --unassigned should have filtered these.
            continue
        keep.append(b)
    return keep


def query_still_stranded(rig: dict, surface: str) -> list[dict]:
    """Return labeled-stranded beads currently in `rig` for the digest.

    These are the beads `surface`-labeled by this script (or by any
    other path) that still have no `gc.routed_to` — i.e. they remain
    operator-actionable.
    """
    cmd = bd_cmd(rig) + [
        "list",
        "--status=open",
        f"--label={surface}",
        "--limit=0",
        "--json",
    ]
    data = run_json(cmd)
    if not isinstance(data, list):
        return []

    prefix = f"{rig['prefix']}-"
    keep: list[dict] = []
    for b in data:
        bid = b.get("id") or ""
        if not bid.startswith(prefix):
            continue
        if b.get("issue_type") not in WORK_TYPES:
            continue
        if b.get("ephemeral"):
            continue
        meta = b.get("metadata") or {}
        if meta.get("gc.routed_to"):
            continue
        keep.append(b)
    return keep


def add_surface_label(rig: dict, bead_id: str, surface: str) -> bool:
    """Add the surface label to a bead. Returns True on success."""
    cmd = bd_cmd(rig) + ["update", bead_id, "--add-label", surface]
    result = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        print(
            f"stranded-bead-sweep: label {bead_id} failed: {result.stderr.strip()}",
            file=sys.stderr,
        )
        return False
    return True


def updated_at_dt(b: dict) -> datetime:
    raw = b.get("updated_at") or b.get("created_at") or ""
    try:
        return datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        return datetime.min.replace(tzinfo=UTC)


def format_digest_entry(b: dict) -> str:
    bid = b.get("id", "?")
    prio = b.get("priority")
    prio_s = f"P{prio}" if isinstance(prio, int) else "P?"
    title = (b.get("title") or "").strip()
    age = datetime.now(UTC) - updated_at_dt(b)
    days = age.days
    age_s = f"{days}d" if days >= 1 else f"{age.seconds // 3600}h"
    return f"- {bid}  {prio_s}  {title}  (idle {age_s})"


def build_digest_body(stranded: list[dict], surface: str) -> tuple[str, str]:
    total = len(stranded)
    # Show oldest first — those are most likely to have been forgotten.
    by_age = sorted(stranded, key=updated_at_dt)
    preview = by_age[:DIGEST_PREVIEW]
    remaining = total - len(preview)

    subject = f"Weekly stranded-bead digest: {total} unrouted"
    lines = [
        f"{total} stranded beads still unrouted; first {len(preview)} listed; "
        f"full list via filter `label={surface} AND no:routed_to`.",
        "",
    ]
    lines.extend(format_digest_entry(b) for b in preview)
    if remaining > 0:
        lines.append(f"- ... +{remaining} more")
    return subject, "\n".join(lines) + "\n"


def send_digest_mail(to: str, subject: str, body: str) -> bool:
    cmd = [
        "gc", "mail", "send", to,
        "--from", "stranded-bead-sweep",
        "-s", subject, "-m", body, "--notify",
    ]
    result = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        print(
            f"stranded-bead-sweep: mail send failed: {result.stderr.strip()}",
            file=sys.stderr,
        )
        return False
    return True


def sweep(
    rigs: list[dict],
    cutoff: datetime,
    surface: str,
    exempt: tuple[str, ...],
    dry_run: bool,
) -> tuple[int, int]:
    """Find and (in dry-run, just count) label stranded beads.

    Returns (scanned, labeled) totals.
    """
    scanned = 0
    labeled = 0
    for rig in rigs:
        candidates = query_stranded_candidates(rig, cutoff, exempt)
        scanned += len(candidates)
        for b in candidates:
            bid = b.get("id")
            if not bid:
                continue
            if dry_run:
                print(
                    f"stranded-bead-sweep[{rig['name']}]: would label "
                    f"{bid} {surface} (idle since {b.get('updated_at')})"
                )
                labeled += 1
                continue
            if add_surface_label(rig, bid, surface):
                labeled += 1
                print(
                    f"stranded-bead-sweep[{rig['name']}]: labeled {bid} "
                    f"{surface} (idle since {b.get('updated_at')})"
                )
    return scanned, labeled


def maybe_send_digest(
    rigs: list[dict],
    surface: str,
    to: str,
    interval: timedelta,
    path: Path,
    now: datetime,
    dry_run: bool,
) -> None:
    """Send the periodic digest if due; advance the state file regardless."""
    last = read_last_digest(path)
    if last is not None and now - last < interval:
        return

    stranded: list[dict] = []
    for rig in rigs:
        stranded.extend(query_still_stranded(rig, surface))

    if not stranded:
        if dry_run:
            print(
                "stranded-bead-sweep: dry-run — digest due but no stranded "
                "beads; would advance state"
            )
            return
        # Advance state so the next digest fires `interval` from now, not
        # next tick.
        write_last_digest(path, now)
        return

    subject, body = build_digest_body(stranded, surface)

    if dry_run:
        print(f"stranded-bead-sweep: dry-run — would send digest to {to}")
        print(f"Subject: {subject}\n")
        print(body)
        return

    if not send_digest_mail(to, subject, body):
        # Don't advance state; retry next tick.
        return
    write_last_digest(path, now)
    print(
        f"stranded-bead-sweep: mailed digest to {to} "
        f"({len(stranded)} bead(s) still unrouted)"
    )


def main() -> int:
    args = parse_args()
    now = datetime.now(UTC)
    cutoff = now - timedelta(hours=age_hours())

    surface = surface_label()
    exempt = exempt_labels(surface)
    interval = digest_interval()
    to = digest_to()

    rigs = list_rigs()
    if not rigs:
        print("stranded-bead-sweep: no initialized rigs; nothing to do")
        return 0

    scanned, labeled = sweep(rigs, cutoff, surface, exempt, args.dry_run)
    maybe_send_digest(rigs, surface, to, interval, state_path(), now, args.dry_run)

    if labeled:
        print(
            f"stranded-bead-sweep: scanned {scanned}, labeled {labeled} "
            f"bead(s) (age threshold {age_hours()}h, label {surface})"
        )
    return 0


if __name__ == "__main__":
    sys.exit(main())
