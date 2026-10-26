#!/usr/bin/env python3
"""Regression check: the mail SQLite DB survives filelock's exit-time release.

mcp-agent-mail wraps filelock.SoftFileLock in AsyncFileLock and drives acquire and
release through asyncio.to_thread, so the two can land on different pool workers.
With filelock's default thread_local=True the release from the other worker is a
no-op: the acquiring worker's context keeps the lock fd, and AsyncFileLock then
unlinks the lock file under it. When fd headroom runs low,
mcp_agent_mail.storage.cleanup_leaked_lockfile_fds() closes every fd that points
at a deleted ".lock" file without clearing filelock's context, so the context keeps
a stale fd number that the next open() (a SQLite connection) reuses.

filelock 4.x releases every thread's abandoned hold when the lock object is
finalized, and when another holder's marker sits at the lock path it overwrites
the stored fd with b"filelock/released\\n" and truncates it to 18 bytes. That fd
is now the mail database. filelock 3.29.0 only releases the finalizing thread's
own hold, and its release only closes the fd and unlinks the path. Review of #8307:
https://github.com/gastownhall/gascity/pull/8307#pullrequestreview-5645840708

The check drives mcp-agent-mail's own AsyncFileLock and
cleanup_leaked_lockfile_fds() in a child interpreter, lets that interpreter exit
normally, and then opens the database from this process. It runs in the
gc-mcp-mail image in .github/workflows/container-scan.yml, and locally in any
Python 3.12 environment holding the hashed lock:

    python3 -m venv /tmp/mail-venv
    /tmp/mail-venv/bin/pip install --no-deps --require-hashes \\
        -r .github/requirements/mcp-agent-mail.txt
    /tmp/mail-venv/bin/python .github/scripts/mcp-mail-filelock-exit-regression.py

Exit status: 0 when the database survives in every journal mode, 1 when any run
corrupts it, and 2 when the scenario could not be set up (the stale fd was not
leaked, closed and reused), because then the run proves nothing.
"""

from __future__ import annotations

import argparse
import asyncio
import concurrent.futures
import json
import os
import sqlite3
import subprocess
import sys
import tempfile
from pathlib import Path

LOCK_NAME = ".archive.lock"
DB_NAME = "mail.sqlite3"
ROWS = 200
SQLITE_HEADER = b"SQLite format 3\x00"
JOURNAL_MODES = ("DELETE", "WAL")
FD_DIR = Path("/proc/self/fd")

EXIT_OK = 0
EXIT_CORRUPTED = 1
EXIT_SETUP_FAILED = 2

# Held at module scope so the connection, and with it the reused fd, stays open
# until interpreter finalization, as the server's connection pool does.
_DB: sqlite3.Connection | None = None


class _SplitAcquireExecutor(concurrent.futures.ThreadPoolExecutor):
    """Runs one lock's acquire on worker A and every other call on worker B.

    asyncio.to_thread hands each call to whichever pool worker is free, so in the
    server the acquire and the release land on different workers only some of the
    time. Pinning that interleaving makes the check deterministic.
    """

    def __init__(self, acquire: object) -> None:
        super().__init__(max_workers=1)
        self._acquire = acquire
        self._acquire_worker = concurrent.futures.ThreadPoolExecutor(1, thread_name_prefix="acquire")
        self._other_worker = concurrent.futures.ThreadPoolExecutor(1, thread_name_prefix="other")

    def submit(self, fn, /, *args, **kwargs):  # type: ignore[override]
        # asyncio.to_thread submits functools.partial(contextvars.Context.run, func, *args).
        wrapped = getattr(fn, "args", ())
        if wrapped and wrapped[0] == self._acquire:
            return self._acquire_worker.submit(fn, *args, **kwargs)
        return self._other_worker.submit(fn, *args, **kwargs)

    def shutdown(self, wait: bool = True, *, cancel_futures: bool = False) -> None:
        self._acquire_worker.shutdown(wait, cancel_futures=cancel_futures)
        self._other_worker.shutdown(wait, cancel_futures=cancel_futures)
        super().shutdown(wait, cancel_futures=cancel_futures)


def _fds_pointing_at(predicate) -> list[int]:
    found = []
    for entry in FD_DIR.iterdir():
        try:
            target = os.readlink(entry)
        except OSError:
            continue  # the fd listdir itself opened, already closed
        if predicate(target):
            found.append(int(entry.name))
    return sorted(found)


async def _leak_and_reuse(root: Path, journal_mode: str) -> dict[str, object]:
    global _DB
    from mcp_agent_mail.storage import AsyncFileLock, cleanup_leaked_lockfile_fds

    lock_path = root / LOCK_NAME
    lock = AsyncFileLock(lock_path)
    asyncio.get_running_loop().set_default_executor(_SplitAcquireExecutor(lock._lock.acquire))
    async with lock:
        pass

    leaked = _fds_pointing_at(lambda t: t.endswith("(deleted)") and LOCK_NAME in t)
    closed = cleanup_leaked_lockfile_fds()

    _DB = sqlite3.connect(root / DB_NAME, isolation_level=None)
    _DB.execute(f"PRAGMA journal_mode={journal_mode}")
    _DB.execute("CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT NOT NULL)")
    _DB.execute("BEGIN")
    _DB.executemany("INSERT INTO messages (body) VALUES (?)", [("m" * 100,)] * ROWS)
    _DB.execute("COMMIT")
    db_fds = _fds_pointing_at(lambda t: t == str(root / DB_NAME))

    # Another holder's marker is at the path when this process exits: a
    # concurrent writer mid-commit, or a stale marker from a killed one.
    lock_path.write_text("held by another process\n", encoding="utf-8")
    return {"leaked_lock_fds": leaked, "cleanup_closed": closed, "db_fds": db_fds}


def _child(root: Path, journal_mode: str) -> None:
    print(json.dumps(asyncio.run(_leak_and_reuse(root, journal_mode))))


def _setup_problem(report: dict[str, object]) -> str | None:
    leaked = report["leaked_lock_fds"]
    if not leaked:
        return "no lock fd leaked: the release on the other worker freed it, so the hazard cannot arise"
    if report["cleanup_closed"] < 1:
        return "cleanup_leaked_lockfile_fds() closed nothing"
    if not set(leaked) & set(report["db_fds"]):
        return f"the database did not reuse a closed lock fd (leaked {leaked}, database {report['db_fds']})"
    return None


def _database_problem(db_path: Path) -> str | None:
    header = db_path.read_bytes()[: len(SQLITE_HEADER)]
    if header != SQLITE_HEADER:
        size = db_path.stat().st_size
        return f"main database file is {size} bytes starting {header!r}, not a SQLite header"
    conn = sqlite3.connect(db_path)
    try:
        integrity = conn.execute("PRAGMA integrity_check").fetchone()[0]
        rows = conn.execute("SELECT count(*) FROM messages").fetchone()[0]
    except sqlite3.DatabaseError as exc:
        return f"database unreadable: {exc}"
    finally:
        conn.close()
    if integrity != "ok" or rows != ROWS:
        return f"integrity_check={integrity!r}, rows={rows}, want ok and {ROWS}"
    return None


def _run(journal_mode: str) -> int:
    with tempfile.TemporaryDirectory(prefix="mcp-mail-filelock-") as tmp:
        root = Path(tmp)
        child = subprocess.run(
            [sys.executable, __file__, "--child", str(root), journal_mode],
            capture_output=True,
            text=True,
            check=False,
        )
        if child.returncode != 0:
            print(f"{journal_mode}: SETUP FAILED: child exited {child.returncode}\n{child.stderr}")
            return EXIT_SETUP_FAILED
        report = json.loads(child.stdout.strip().splitlines()[-1])
        if problem := _setup_problem(report):
            print(f"{journal_mode}: SETUP FAILED: {problem} ({report})")
            return EXIT_SETUP_FAILED
        if problem := _database_problem(root / DB_NAME):
            print(f"{journal_mode}: CORRUPTED: {problem} ({report})")
            return EXIT_CORRUPTED
        print(f"{journal_mode}: OK: database intact after exit ({report})")
        return EXIT_OK


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--child", nargs=2, metavar=("ROOT", "JOURNAL_MODE"), help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.child:
        _child(Path(args.child[0]), args.child[1])
        return EXIT_OK
    if not FD_DIR.is_dir():
        print(f"SETUP FAILED: {FD_DIR} is missing; cleanup_leaked_lockfile_fds() only acts on Linux")
        return EXIT_SETUP_FAILED

    import filelock

    print(f"filelock {filelock.__version__}")
    return max(_run(mode) for mode in JOURNAL_MODES)


if __name__ == "__main__":
    sys.exit(main())
