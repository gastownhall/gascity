"""Adapter for dsh session transcripts.

Layout: ``~/.dsh/sessions/<cwd-key>/session-<id>/session.v3.jsonl.zstd``. The
file is zstd-compressed JSONL. Decompression prefers the ``zstandard`` Python
module and falls back to the ``zstd`` binary through a subprocess, so no new pip
dependency is required.

All records carry a monotonic ``seq`` and epoch-millisecond ``time``. Tool
requests/results pair by ``callId``; token usage is per ``assistant/message``.
The model is taken from the ``store/model`` string on the message source (for
example ``deepseek/deepseek-flash``), not the provider field.
"""

from __future__ import annotations

import contextlib
import io
import json
import shutil
import subprocess
import threading
from pathlib import Path
from typing import Any

from ..contract import SCHEMA_VERSION
from .base import (
    AdapterContext,
    AdapterError,
    AdapterResult,
    SourceAdapter,
    SourceSizeExceeded,
    TitleRevision,
    extract_command,
    fallback_event_id,
    is_number,
    iso_from_epoch,
    number_or_none,
    split_jsonl,
)
from .git_evidence import attach_git_evidence
from .redaction import elide_large_text, redact_and_bound, redact_text

_META_TYPES = frozenset(
    {
        "permission/preset",
        "sandbox/mode",
        "approval/policy",
        "turn/start",
        "turn/end",
        "step/start",
        "step/end",
        "request/context",
        "request/header",
        "assistant/attempt",
        "llm/retry",
        "llm/retry-started",
    }
)

# Record types only dsh emits. Used to content-check an uncompressed file that
# happens to live under a ``.dsh/sessions`` layout before calling it a
# transcript, so an unrelated JSONL file is not silently misclassified.
_DSH_SIGNATURE_TYPES = _META_TYPES | frozenset(
    {
        "session",
        "session/title",
        "assistant/message",
        "user/message",
        "system/message",
        "tool/call",
        "tool/result",
    }
)

# How many leading records to scan for a dsh signature. A byte prefix is the
# wrong bound: one oversized but valid first record consumes it before any
# complete line is seen. Streaming complete lines, bounded by record count,
# detects a transcript regardless of how large a single record is.
_DSH_SIGNATURE_RECORDS = 64


def _looks_like_dsh_transcript(path: Path) -> bool:
    """Return whether *path* begins with a recognizable dsh record.

    Detection cannot rely on the directory layout alone: ``.dsh/sessions`` can
    contain logs, notes and other JSONL that are not transcripts. Scanning a
    bounded number of complete lines for a dsh ``type`` keeps discovery honest
    without truncating a valid first record that exceeds a byte prefix.
    """

    try:
        with open(path, "rb") as handle:
            for index, raw_line in enumerate(handle):
                if index >= _DSH_SIGNATURE_RECORDS:
                    break
                line = raw_line.strip()
                if not line:
                    continue
                try:
                    decoded = json.loads(line.decode("utf-8", "replace"))
                except ValueError:
                    continue
                if isinstance(decoded, dict) and decoded.get("type") in _DSH_SIGNATURE_TYPES:
                    return True
    except OSError:
        return False
    return False


# Keep only a bounded prefix of a child's stderr for the failure message; the
# reader must still drain the whole pipe so a chatty child cannot block on it.
_STDERR_PREFIX_BYTES = 64 * 1024


def _decompress_zstd_binary(binary: str, raw: bytes, source_path: str, max_bytes: int | None) -> bytes:
    """Decompress *raw* with the ``zstd`` binary, bounded by *max_bytes*.

    The child's stdout is read in chunks and the process is killed the moment the
    logical output passes the cap, so a small compressed stream cannot expand
    into unbounded memory. Helper threads drain stdin and stderr: a child that
    fills the stderr pipe would otherwise block on write while stdout never
    EOFs, hanging the read loop forever (and, in the collector, holding the
    projection lock until the process is killed).
    """

    process = subprocess.Popen(
        [binary, "-dc"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    def feed() -> None:
        try:
            if process.stdin is not None:
                process.stdin.write(raw)
        except (BrokenPipeError, OSError):
            pass
        finally:
            if process.stdin is not None:
                with contextlib.suppress(OSError):
                    process.stdin.close()

    stderr_chunks: list[bytes] = []
    stderr_bytes = 0

    def drain_stderr() -> None:
        nonlocal stderr_bytes
        if process.stderr is None:
            return
        try:
            while True:
                chunk = process.stderr.read(65536)
                if not chunk:
                    break
                if stderr_bytes < _STDERR_PREFIX_BYTES:
                    keep = chunk[: _STDERR_PREFIX_BYTES - stderr_bytes]
                    stderr_chunks.append(keep)
                    stderr_bytes += len(keep)
        except (OSError, ValueError):
            pass

    writer = threading.Thread(target=feed, daemon=True)
    writer.start()
    stderr_reader = threading.Thread(target=drain_stderr, daemon=True)
    stderr_reader.start()
    limit = None if max_bytes is None else max_bytes + 1
    chunks: list[bytes] = []
    total = 0
    exceeded = False
    try:
        if process.stdout is not None:
            while True:
                chunk = process.stdout.read(65536)
                if not chunk:
                    break
                chunks.append(chunk)
                total += len(chunk)
                if limit is not None and total >= limit:
                    exceeded = total > max_bytes
                    break
        if exceeded:
            process.kill()
    finally:
        writer.join(timeout=5)
        process.wait()
        stderr_reader.join(timeout=5)
    stderr = b"".join(stderr_chunks)
    for stream in (process.stdout, process.stderr):
        if stream is not None:
            with contextlib.suppress(OSError):
                stream.close()
    if exceeded:
        raise SourceSizeExceeded(source_path, max_bytes)
    if process.returncode != 0:
        reason = stderr.decode("utf-8", "replace").strip().splitlines()
        raise AdapterError(f"zstd decompression failed: {reason[0] if reason else 'unknown error'}", source_path)
    return b"".join(chunks)


class DshAdapter(SourceAdapter):
    """Read-only adapter for compressed dsh session transcripts."""

    provider = "dsh"
    adapter_version = "1.0.0"

    def detect(self, source_path: str) -> bool:
        path = Path(source_path)
        name = path.name
        parts = path.parts
        if ".dsh" not in parts or "sessions" not in parts:
            return False
        if name.endswith(".zstd"):
            # Compressed content cannot be sniffed without decompressing, so the
            # filename plus the ``.dsh/sessions`` layout is the detection signal.
            # parse() then content-checks the decompressed records and emits a
            # ``no_dsh_signature`` note when none of them are dsh types.
            return "session.v3.jsonl" in name
        if not name.endswith(".jsonl"):
            return False
        return _looks_like_dsh_transcript(path)

    def decompress(self, raw: bytes, source_path: str, max_bytes: int | None = None) -> bytes:
        if not Path(source_path).name.endswith(".zstd"):
            # An uncompressed session file is already logical content; the base
            # contract is identity here. Only ``*.zstd`` names carry a stream.
            return super().decompress(raw, source_path, max_bytes)
        try:
            import zstandard  # type: ignore[import-not-found]
        except ImportError:
            zstandard = None  # type: ignore[assignment]
        if zstandard is not None:
            try:
                reader = zstandard.ZstdDecompressor().stream_reader(io.BytesIO(raw))
                if max_bytes is None:
                    return reader.read()
                data = reader.read(max_bytes + 1)
            except Exception as exc:  # pragma: no cover - depends on optional module
                raise AdapterError(f"zstd decompression failed: {exc}", source_path) from exc
            if len(data) > max_bytes:
                raise SourceSizeExceeded(source_path, max_bytes)
            return data

        binary = shutil.which("zstd")
        if not binary:
            raise AdapterError(
                "zstd decompression requires the python 'zstandard' module or the 'zstd' binary",
                source_path,
            )
        return _decompress_zstd_binary(binary, raw, source_path, max_bytes)

    def parse(
        self,
        data: bytes,
        *,
        context: AdapterContext,
        generation: int,
        source_path: str,
        source_sha256: str,
    ) -> AdapterResult:
        decoded, partial, errors = split_jsonl(data, source_path)
        result = AdapterResult(
            provider=self.provider,
            adapter_version=self.adapter_version,
            source_path=source_path,
            source_sha256=source_sha256,
            source_size_bytes=len(data),
            session_id=Path(source_path).parent.name or Path(source_path).stem or "unknown",
            parent_session_id=None,
            errors=list(errors),
            partial_trailing_line=partial,
            line_count=len(decoded),
        )
        model: str | None = None
        call_times: dict[str, float] = {}
        saw_dsh_signature = False

        for line_number, obj in decoded:
            if not isinstance(obj, dict):
                result.note_skip("non_object")
                continue
            record_type = obj.get("type")
            if isinstance(record_type, str) and record_type in _DSH_SIGNATURE_TYPES:
                saw_dsh_signature = True
            payload = obj.get("data") if isinstance(obj.get("data"), dict) else {}
            if record_type == "session":
                session_id = obj.get("id")
                if isinstance(session_id, str) and session_id:
                    result.session_id = session_id
                result.note_skip("session")
            elif record_type == "session/title":
                self._record_title(obj, payload, line_number, result)
            elif record_type == "assistant/message":
                candidate = _message_model(payload)
                if candidate:
                    model = candidate
                self._parse_assistant(obj, payload, line_number, context, generation, model, result)
            elif record_type == "user/message":
                self._parse_message(obj, payload, line_number, context, generation, "message", result)
            elif record_type == "system/message":
                self._parse_message(obj, payload, line_number, context, generation, "system", result)
            elif record_type == "tool/call":
                self._parse_tool_call(obj, payload, line_number, context, generation, model, call_times, result)
            elif record_type == "tool/result":
                self._parse_tool_result(obj, payload, line_number, context, generation, call_times, result)
            elif record_type == "request/header":
                candidate = _header_model(payload)
                if candidate:
                    model = candidate
                result.note_skip("request/header")
            elif record_type in _META_TYPES:
                result.note_skip(f"meta:{record_type}")
            else:
                result.note_skip(f"type:{record_type}")

        if not saw_dsh_signature:
            # A file that decompressed cleanly but carries no dsh record type is
            # not a transcript; flag it instead of reporting a silent empty
            # session.
            result.note_skip("no_dsh_signature")

        attach_git_evidence(result, decoded, context)
        return result

    # -- record plumbing ---------------------------------------------------

    def _record(
        self,
        context: AdapterContext,
        result: AdapterResult,
        *,
        event_id: str,
        timestamp: str,
        kind: str,
        model: str | None = None,
        usage: dict[str, Any] | None = None,
        **fields: Any,
    ) -> dict[str, Any]:
        record = {
            "schema_version": SCHEMA_VERSION,
            "city_id": context.city_id,
            "host_id": context.host_id,
            "provider": self.provider,
            "session_id": result.session_id,
            "event_id": event_id,
            "timestamp": timestamp,
            "kind": kind,
            "title": None,
            "text": None,
            "tool_name": None,
            "tool_call_id": None,
            "command": None,
            "exit_code": None,
            "duration_ms": None,
            "model": model,
            "repo": context.repo,
            "commit_sha": None,
            "parent_session_id": result.parent_session_id,
            "bead_id": None,
            "formula_id": None,
            "usage": usage,
        }
        record.update(fields)
        return record

    # -- handlers ----------------------------------------------------------

    def _record_title(
        self,
        obj: dict[str, Any],
        payload: dict[str, Any],
        line_number: int,
        result: AdapterResult,
    ) -> None:
        title = payload.get("title")
        if not isinstance(title, str) or not title.strip():
            result.note_skip("title_empty")
            return
        result.title_revisions.append(
            TitleRevision(
                title=redact_text(title),
                position=line_number,
                observed_timestamp=iso_from_epoch(obj.get("time")),
                source="session/title",
            )
        )

    def _parse_assistant(
        self,
        obj: dict[str, Any],
        payload: dict[str, Any],
        line_number: int,
        context: AdapterContext,
        generation: int,
        model: str | None,
        result: AdapterResult,
    ) -> None:
        timestamp = iso_from_epoch(obj.get("time"))
        if timestamp is None:
            result.note_skip("no_timestamp")
            return
        message = payload.get("message") if isinstance(payload.get("message"), dict) else {}
        message_id = message.get("id") if isinstance(message.get("id"), str) else None
        raw_usage = payload.get("usage")
        usage = _dsh_usage(raw_usage)
        if isinstance(raw_usage, dict) and usage is None:
            result.note_skip("usage_unmapped")
        seq = obj.get("seq")
        pending: list[dict[str, Any]] = []

        content = message.get("content")
        blocks = [block for block in content if isinstance(block, dict)] if isinstance(content, list) else []
        for index, block in enumerate(blocks):
            block_type = block.get("type")
            if block_type == "text":
                text = block.get("text")
                if isinstance(text, str) and text.strip():
                    pending.append(
                        self._record(
                            context,
                            result,
                            event_id=(
                                f"{message_id}:text:{index}"
                                if message_id
                                else fallback_event_id(self.provider, generation, line_number, "message", block)
                            ),
                            timestamp=timestamp,
                            kind="message",
                            model=model,
                            text=redact_and_bound(text),
                        )
                    )
            elif block_type == "reasoning":
                result.note_skip("reasoning")
            elif block_type == "tool-call":
                # The canonical tool/call record carries this; skip the copy.
                result.note_skip("assistant_tool_call_copy")
            else:
                result.note_skip(f"block:{block_type}")

        if not pending and usage is not None:
            if isinstance(seq, int) and not isinstance(seq, bool):
                pending.append(
                    self._record(
                        context,
                        result,
                        event_id=f"seq:{seq}",
                        timestamp=timestamp,
                        kind="assistant_message",
                        model=model,
                    )
                )
            else:
                # The record reports tokens but carries no content and no usable
                # seq to anchor an assistant_message, so the usage would vanish.
                result.note_skip("usage_dropped")
        if pending:
            if usage is not None:
                pending[0]["usage"] = usage
            result.records.extend(pending)

    def _parse_message(
        self,
        obj: dict[str, Any],
        payload: dict[str, Any],
        line_number: int,
        context: AdapterContext,
        generation: int,
        kind: str,
        result: AdapterResult,
    ) -> None:
        timestamp = iso_from_epoch(obj.get("time"))
        if timestamp is None:
            result.note_skip("no_timestamp")
            return
        text = _flatten_text(payload.get("content"))
        if not text:
            result.note_skip(f"{kind}_empty")
            return
        seq = obj.get("seq")
        event_id = f"seq:{seq}" if isinstance(seq, int) else fallback_event_id(self.provider, generation, line_number, kind, payload)
        result.records.append(
            self._record(
                context,
                result,
                event_id=event_id,
                timestamp=timestamp,
                kind=kind,
                text=redact_and_bound(text),
            )
        )

    def _parse_tool_call(
        self,
        obj: dict[str, Any],
        payload: dict[str, Any],
        line_number: int,
        context: AdapterContext,
        generation: int,
        model: str | None,
        call_times: dict[str, float],
        result: AdapterResult,
    ) -> None:
        timestamp = iso_from_epoch(obj.get("time"))
        if timestamp is None:
            result.note_skip("no_timestamp")
            return
        call_id = payload.get("callId") if isinstance(payload.get("callId"), str) else None
        call_time = obj.get("time")
        if call_id and is_number(call_time):
            call_times[call_id] = call_time
        seq = obj.get("seq")
        event_id = f"seq:{seq}" if isinstance(seq, int) else fallback_event_id(self.provider, generation, line_number, "tool_call", payload)
        tool_name = payload.get("name") if isinstance(payload.get("name"), str) else None
        command = extract_command(tool_name, payload.get("arguments"))
        arguments = payload.get("arguments")
        result.records.append(
            self._record(
                context,
                result,
                event_id=event_id,
                timestamp=timestamp,
                kind="tool_call",
                model=model,
                tool_name=tool_name,
                tool_call_id=call_id,
                command=elide_large_text(command) if command else None,
                text=elide_large_text(arguments) if isinstance(arguments, str) and arguments else None,
            )
        )

    def _parse_tool_result(
        self,
        obj: dict[str, Any],
        payload: dict[str, Any],
        line_number: int,
        context: AdapterContext,
        generation: int,
        call_times: dict[str, float],
        result: AdapterResult,
    ) -> None:
        timestamp = iso_from_epoch(obj.get("time"))
        if timestamp is None:
            result.note_skip("no_timestamp")
            return
        message = payload.get("message") if isinstance(payload.get("message"), dict) else {}
        call_id = _result_call_id(message)
        if call_id is None:
            source = message.get("source")
            if isinstance(source, dict) and isinstance(source.get("callId"), str):
                call_id = source["callId"]
        text = _tool_result_text(message)
        duration_ms = None
        result_time = obj.get("time")
        if call_id is None or not is_number(result_time) or call_id not in call_times:
            # A result without a matching prior call has no duration to report;
            # flag it explicitly instead of leaving None as the only signal.
            result.note_skip("tool_result_unpaired")
        else:
            delta = result_time - call_times[call_id]
            if delta >= 0:
                # ``duration_ms`` is an integer field in the normalized contract;
                # a fractional source clock is truncated, never emitted as float.
                duration_ms = int(delta)
            else:
                # A result that predates its call cannot have a duration; flag the
                # contradiction rather than leaving None as the only signal.
                result.note_skip("tool_result_negative_delta")
        seq = obj.get("seq")
        event_id = f"seq:{seq}" if isinstance(seq, int) else fallback_event_id(self.provider, generation, line_number, "tool_result", payload)
        result.records.append(
            self._record(
                context,
                result,
                event_id=event_id,
                timestamp=timestamp,
                kind="tool_result",
                tool_call_id=call_id,
                duration_ms=duration_ms,
                text=elide_large_text(text) if text else None,
            )
        )


def _message_model(payload: dict[str, Any]) -> str | None:
    message = payload.get("message")
    if not isinstance(message, dict):
        return None
    source = message.get("source")
    if isinstance(source, dict) and isinstance(source.get("model"), str):
        return source["model"]
    return None


def _header_model(payload: dict[str, Any]) -> str | None:
    header = payload.get("header")
    if not isinstance(header, dict):
        return None
    config = header.get("config")
    if isinstance(config, dict) and isinstance(config.get("model"), str):
        return config["model"]
    return None


def _dsh_usage(usage: Any) -> dict[str, int | float | None] | None:
    if not isinstance(usage, dict):
        return None
    mapped = {
        "input_tokens": number_or_none(usage.get("inputTokens")),
        "output_tokens": number_or_none(usage.get("outputTokens")),
        "cache_read_tokens": number_or_none(usage.get("cacheReadTokens")),
        "cache_write_tokens": number_or_none(usage.get("cacheWriteTokens")),
        "total_tokens": number_or_none(usage.get("totalTokens")),
    }
    if all(value is None for value in mapped.values()):
        return None
    return mapped


def _flatten_text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""
    parts = []
    for block in content:
        if isinstance(block, dict) and isinstance(block.get("text"), str):
            parts.append(block["text"])
    return "\n".join(parts)


def _result_call_id(message: dict[str, Any]) -> str | None:
    content = message.get("content")
    if isinstance(content, list):
        for block in content:
            if isinstance(block, dict) and isinstance(block.get("toolCallId"), str):
                return block["toolCallId"]
    return None


def _tool_result_text(message: dict[str, Any]) -> str:
    content = message.get("content")
    if not isinstance(content, list):
        return ""
    parts = []
    for block in content:
        if not isinstance(block, dict):
            continue
        inner = block.get("content")
        if isinstance(inner, str):
            parts.append(inner)
        elif isinstance(inner, list):
            text = _flatten_text(inner)
            if text:
                parts.append(text)
    return "\n".join(parts)
