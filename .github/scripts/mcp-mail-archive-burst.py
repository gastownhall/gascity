"""Measure real MCP mail delivery and archive latency in a fresh process."""

import asyncio
from contextlib import asynccontextmanager
import json
import os
from pathlib import Path
import shutil
import sys
import time


kind = sys.argv[1]
root = Path(f"/tmp/mcp-mail-archive-burst-{kind}")
shutil.rmtree(root, ignore_errors=True)
root.mkdir(parents=True)
os.environ.update(
    DATABASE_URL=f"sqlite+aiosqlite:///{root / 'mail.sqlite3'}",
    STORAGE_ROOT=str(root / "storage"),
    APP_ENVIRONMENT="test",
    GIT_AUTHOR_NAME="Test",
    GIT_AUTHOR_EMAIL="test@example.com",
    TOOLS_LOG_ENABLED="false",
)

from fastmcp import Client  # noqa: E402
from mcp_agent_mail.app import build_mcp_server  # noqa: E402
import mcp_agent_mail.app as app  # noqa: E402
import mcp_agent_mail.storage as storage  # noqa: E402
import git  # noqa: E402


def percentile(values, quantile):
    return sorted(values)[min(len(values) - 1, int(len(values) * quantile))] if values else None


archive_waits = []
archive_lock = app.archive_write_lock


@asynccontextmanager
async def timed_archive_lock(*args, **kwargs):
    started = time.perf_counter()
    acquired = False
    try:
        async with archive_lock(*args, **kwargs):
            archive_waits.append(time.perf_counter() - started)
            acquired = True
            yield
    except TimeoutError:
        if not acquired:
            archive_waits.append(time.perf_counter() - started)
        raise


app.archive_write_lock = timed_archive_lock
commit_times = []
original_commit = storage._commit_direct


async def timed_commit(*args, **kwargs):
    started = time.perf_counter()
    try:
        return await original_commit(*args, **kwargs)
    finally:
        commit_times.append(time.perf_counter() - started)


storage._commit_direct = timed_commit


async def run_burst():
    server = build_mcp_server()
    errors = []
    responses = []
    request_times = []
    service_times = []
    semaphore = asyncio.Semaphore(8)

    async with Client(server) as client:
        project = (await client.call_tool("ensure_project", {"human_key": "/tmp/mcp-mail-burst-project"})).data
        slug = project["slug"]
        await client.call_tool(
            "register_agent",
            {"project_key": slug, "program": "codex", "model": "test", "name": "BlueLake"},
        )

        async def send(index):
            queued = time.perf_counter()
            async with semaphore:
                started = time.perf_counter()
                try:
                    response = await client.call_tool(
                        "send_message",
                        {
                            "project_key": slug,
                            "sender_name": "BlueLake",
                            "to": ["BlueLake"],
                            "subject": f"burst-{index:04}",
                            "body_md": f"message {index}",
                        },
                    )
                    if getattr(response, "isError", False):
                        errors.append((index, "ToolError", str(response.data)))
                    else:
                        responses.append((index, response.data))
                except Exception as exc:
                    errors.append((index, type(exc).__name__, str(exc)))
                finally:
                    finished = time.perf_counter()
                    request_times.append(finished - queued)
                    service_times.append(finished - started)

        wall_started = time.perf_counter()
        await asyncio.gather(*(send(index) for index in range(400)))
        wall = time.perf_counter() - wall_started
        inbox = await client.call_tool(
            "fetch_inbox",
            {"project_key": slug, "agent_name": "BlueLake", "limit": 500},
        )
        inbox_rows = inbox.data or []

    ids = [index for index, _ in responses]
    summary = {
        "kind": kind,
        "gitpython": git.__version__,
        "wall": wall,
        "success": len(responses),
        "unique_requests": len(set(ids)),
        "errors": errors,
        "inbox_count": len(inbox_rows),
        "unique_inbox_ids": len({row.get("id") for row in inbox_rows}),
        "unique_subjects": len({row.get("subject") for row in inbox_rows}),
        "request_p95": percentile(request_times, 0.95),
        "request_max": max(request_times, default=None),
        "request_over_60s": sum(value > 60 for value in request_times),
        "service_p95": percentile(service_times, 0.95),
        "archive_wait_p95": percentile(archive_waits, 0.95),
        "archive_wait_max": max(archive_waits, default=None),
        "archive_wait_over_60s": sum(value > 60 for value in archive_waits),
        "commit_p95": percentile(commit_times, 0.95),
        "commit_max": max(commit_times, default=None),
        "commit_over_60s": sum(value > 60 for value in commit_times),
    }
    print("RESULT " + json.dumps(summary, sort_keys=True), flush=True)


asyncio.run(run_burst())
