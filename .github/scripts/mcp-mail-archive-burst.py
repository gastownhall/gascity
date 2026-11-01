"""Measure real MCP mail delivery and archive latency in a fresh process."""

import asyncio
import atexit
from contextlib import asynccontextmanager
from contextvars import ContextVar
from functools import wraps
import hashlib
import inspect
import json
import os
from pathlib import Path
import re
import shutil
import sys
import threading
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
current_phase = ContextVar("current_phase", default="setup")
current_request = ContextVar("current_request", default=None)
raw_samples = []
raw_samples_lock = threading.Lock()


def record_sample(operation, started_ns, started_unix_ns, **extra):
    sample = {
        "operation": operation,
        "phase": current_phase.get(),
        "request_index": current_request.get(),
        "started_unix_ns": started_unix_ns,
        "duration_ns": time.perf_counter_ns() - started_ns,
    }
    sample.update(extra)
    with raw_samples_lock:
        raw_samples.append(sample)


archive_lock = app.archive_write_lock


@asynccontextmanager
async def timed_archive_lock(*args, **kwargs):
    started_ns = time.perf_counter_ns()
    started_unix_ns = time.time_ns()
    started = time.perf_counter()
    acquired = False
    try:
        async with archive_lock(*args, **kwargs):
            archive_waits.append(time.perf_counter() - started)
            acquired = True
            record_sample("archive_lock_wait", started_ns, started_unix_ns)
            yield
    except TimeoutError:
        if not acquired:
            archive_waits.append(time.perf_counter() - started)
            record_sample("archive_lock_wait", started_ns, started_unix_ns, timed_out=True)
        raise


app.archive_write_lock = timed_archive_lock
commit_times = []
original_commit = storage._commit_direct


async def timed_commit(*args, **kwargs):
    started_ns = time.perf_counter_ns()
    started_unix_ns = time.time_ns()
    started = time.perf_counter()
    failure_type = None
    try:
        return await original_commit(*args, **kwargs)
    except BaseException as exc:
        failure_type = type(exc).__name__
        raise
    finally:
        elapsed = time.perf_counter() - started
        commit_times.append(elapsed)
        record_sample(
            "commit_direct",
            started_ns,
            started_unix_ns,
            duration_seconds=elapsed,
            status="error" if failure_type else "ok",
            failure_type=failure_type,
        )


storage._commit_direct = timed_commit

# The MCP storage layer sends normal commits through a long-lived background
# queue. ContextVars set in the send tool's task do not propagate back to that
# already-running worker. Carry the caller's labels on each queue request, then
# restore them while its batch is executed. A batch can contain several sends,
# so request_index is a list in that case.
original_request_init = storage._CommitRequest.__init__


def tagged_request_init(self, *args, **kwargs):
    original_request_init(self, *args, **kwargs)
    self._diagnostic_phase = current_phase.get()
    self._diagnostic_request_index = current_request.get()


storage._CommitRequest.__init__ = tagged_request_init
original_process_batch = storage._CommitQueue._process_batch


async def tagged_process_batch(self, batch):
    phases = {getattr(request, "_diagnostic_phase", "setup") for request in batch}
    phase = next(iter(phases)) if len(phases) == 1 else "mixed"
    request_indexes = [
        getattr(request, "_diagnostic_request_index", None)
        for request in batch
        if getattr(request, "_diagnostic_request_index", None) is not None
    ]
    request_value = request_indexes or None
    phase_token = current_phase.set(phase)
    request_token = current_request.set(request_value)
    try:
        return await original_process_batch(self, batch)
    finally:
        current_request.reset(request_token)
        current_phase.reset(phase_token)


storage._CommitQueue._process_batch = tagged_process_batch

# Mark the actual FastMCP tool execution, rather than relying on the in-process
# client task's ContextVar context to cross the server dispatch boundary.
original_instrument_tool = app._instrument_tool


def tagged_instrument_tool(tool_name, *args, **kwargs):
    decorate = original_instrument_tool(tool_name, *args, **kwargs)

    def apply_instrumentation(func):
        instrumented = decorate(func)
        if tool_name not in {"send_message", "fetch_inbox"}:
            return instrumented

        signature = inspect.signature(func)

        @wraps(instrumented)
        async def tagged_tool(*call_args, **call_kwargs):
            bound = signature.bind_partial(*call_args, **call_kwargs)
            phase = "send" if tool_name == "send_message" else "inbox_fetch"
            request_index = None
            if tool_name == "send_message":
                match = re.fullmatch(r"burst-(\d{4})", str(bound.arguments.get("subject", "")))
                if match:
                    request_index = int(match.group(1))
            phase_token = current_phase.set(phase)
            request_token = current_request.set(request_index)
            try:
                return await instrumented(*call_args, **call_kwargs)
            finally:
                current_request.reset(request_token)
                current_phase.reset(phase_token)

        return tagged_tool

    return apply_instrumentation


app._instrument_tool = tagged_instrument_tool

# Split the end-to-end commit timer into the GitPython operations that make up
# _commit_direct. These wrappers preserve arguments, return values, and
# exceptions; samples contain no repository paths, commit messages, or bodies.
from git.index.base import IndexFile  # noqa: E402


def instrument_method(owner, method_name, operation):
    original = getattr(owner, method_name)

    def timed(*args, **kwargs):
        started_ns = time.perf_counter_ns()
        started_unix_ns = time.time_ns()
        failure_type = None
        try:
            return original(*args, **kwargs)
        except BaseException as exc:
            failure_type = type(exc).__name__
            raise
        finally:
            record_sample(
                operation,
                started_ns,
                started_unix_ns,
                status="error" if failure_type else "ok",
                failure_type=failure_type,
            )

    setattr(owner, method_name, timed)


instrument_method(git.Repo, "__init__", "git_repo_open")
instrument_method(git.Repo, "is_dirty", "git_is_dirty")
instrument_method(IndexFile, "add", "git_index_add")
instrument_method(IndexFile, "commit", "git_index_commit")

original_commit_lock = storage.AsyncFileLock


@asynccontextmanager
async def timed_commit_lock(*args, **kwargs):
    lock = original_commit_lock(*args, **kwargs)
    started_ns = time.perf_counter_ns()
    started_unix_ns = time.time_ns()
    async with lock:
        record_sample("commit_lock_wait", started_ns, started_unix_ns)
        yield


storage.AsyncFileLock = timed_commit_lock


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
            phase_token = current_phase.set("send")
            request_token = current_request.set(index)
            queued_ns = time.perf_counter_ns()
            queued_unix_ns = time.time_ns()
            try:
                async with semaphore:
                    started = time.perf_counter()
                    started_ns = time.perf_counter_ns()
                    started_unix_ns = time.time_ns()
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
                        record_sample("request_total", queued_ns, queued_unix_ns)
                        record_sample("request_service", started_ns, started_unix_ns)
            finally:
                current_request.reset(request_token)
                current_phase.reset(phase_token)

        wall_started = time.perf_counter()
        await asyncio.gather(*(send(index) for index in range(400)))
        wall = time.perf_counter() - wall_started
        phase_token = current_phase.set("inbox_fetch")
        inbox_started_ns = time.perf_counter_ns()
        inbox_started_unix_ns = time.time_ns()
        try:
            inbox = await client.call_tool(
                "fetch_inbox",
                {"project_key": slug, "agent_name": "BlueLake", "limit": 500},
            )
        finally:
            record_sample("inbox_fetch", inbox_started_ns, inbox_started_unix_ns)
            current_phase.reset(phase_token)
        inbox_rows = inbox.data or []

    ids = [index for index, _ in responses]
    send_commit_samples = [
        sample for sample in raw_samples
        if sample.get("operation") == "commit_direct" and sample.get("phase") == "send"
    ]
    send_archive_lock_samples = [
        sample for sample in raw_samples
        if sample.get("operation") == "archive_lock_wait" and sample.get("phase") == "send"
    ]
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
        "send_commit_count": len(send_commit_samples),
        "send_commit_p95": percentile(
            [sample["duration_ns"] / 1_000_000_000 for sample in send_commit_samples],
            0.95,
        ),
        "send_archive_lock_wait_count": len(send_archive_lock_samples),
        "send_archive_lock_wait_p95": percentile(
            [sample["duration_ns"] / 1_000_000_000 for sample in send_archive_lock_samples],
            0.95,
        ),
        "sample_counts_by_phase": {
            phase: sum(sample.get("phase") == phase for sample in raw_samples)
            for phase in ("setup", "send", "inbox_fetch")
        },
        "sample_counts_by_operation_phase": {
            f"{operation}:{phase}": sum(
                sample.get("operation") == operation and sample.get("phase") == phase
                for sample in raw_samples
            )
            for operation in sorted({sample.get("operation") for sample in raw_samples})
            for phase in ("setup", "send", "inbox_fetch", "mixed")
        },
        "gitpython_path_cache": _path_cache_info(),
    }
    _write_raw_samples(summary)
    print("RESULT " + json.dumps(summary, sort_keys=True), flush=True)


def _path_cache_info():
    module = sys.modules.get("sitecustomize")
    info = getattr(module, "cache_info", None)
    if info is None:
        return None
    value = info()
    return value._asdict() if hasattr(value, "_asdict") else str(value)


def _write_raw_samples(summary=None):
    destination = os.environ.get("MAIL_SAMPLE_PATH")
    if not destination:
        return
    path = Path(destination)
    path.parent.mkdir(parents=True, exist_ok=True)
    summary_json = json.dumps(summary, sort_keys=True) if summary is not None else None
    safe_summary = None
    if summary is not None:
        safe_summary = {key: value for key, value in summary.items() if key != "errors"}
        safe_summary["error_count"] = len(summary.get("errors", []))
        safe_summary["error_types"] = sorted(
            {str(error[1]) for error in summary.get("errors", []) if len(error) > 1}
        )
    payload = {
        "schema_version": 1,
        "kind": kind,
        "python_version": sys.version,
        "gitpython_version": git.__version__,
        "image_id": os.environ.get("MAIL_IMAGE_ID"),
        "source_sha": os.environ.get("MAIL_SOURCE_SHA"),
        "pr_head_sha": os.environ.get("MAIL_PR_HEAD_SHA"),
        "workflow_run_id": os.environ.get("MAIL_RUN_ID"),
        "workflow_run_attempt": os.environ.get("MAIL_RUN_ATTEMPT"),
        "variant": os.environ.get("MAIL_VARIANT"),
        "trial": os.environ.get("MAIL_TRIAL"),
        "arm_order": os.environ.get("MAIL_ARM_ORDER"),
        "host_platform": os.environ.get("MAIL_HOST_PLATFORM"),
        "host_cpu_count": os.environ.get("MAIL_HOST_CPU_COUNT"),
        "baseline_wheel_sha256": os.environ.get("MAIL_BASELINE_WHEEL_SHA256"),
        "runner_cpu_limit": os.environ.get("MAIL_RUNNER_CPU_LIMIT"),
        "runner_memory_limit": os.environ.get("MAIL_RUNNER_MEMORY_LIMIT"),
        "summary": safe_summary,
        "summary_sha256": (
            hashlib.sha256(summary_json.encode("utf-8")).hexdigest()
            if summary_json is not None
            else None
        ),
        "samples": raw_samples,
    }
    path.write_text(json.dumps(payload, sort_keys=True) + "\n", encoding="utf-8")


def _write_partial_samples():
    destination = os.environ.get("MAIL_SAMPLE_PATH")
    if destination and not Path(destination).exists():
        _write_raw_samples()


atexit.register(_write_partial_samples)


asyncio.run(run_burst())
