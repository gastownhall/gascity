"""Adapter for Codex rollout JSONL transcripts.

Layout: ``~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl``. Each line has a
top-level ``type`` and ``payload``. Tool requests and results are
``response_item`` records of type ``function_call``/``function_call_output``
(and the ``custom_tool_call`` variants), paired by ``call_id``. Per-turn token
usage is carried by ``event_msg``/``token_count`` records via
``last_token_usage``.

The session id is the rollout id in ``session_meta``; ``parent_thread_id`` (or
the nested subagent spawn parent) becomes ``parent_session_id``. Encrypted
reasoning payloads are never stored.
"""

from __future__ import annotations

import re
from datetime import datetime
from pathlib import Path
from typing import Any

from ..contract import SCHEMA_VERSION
from .base import (
    AdapterContext,
    AdapterResult,
    SourceAdapter,
    extract_command,
    fallback_event_id,
    number_or_none,
    split_jsonl,
)
from .git_evidence import attach_git_evidence
from .redaction import elide_large_text, redact_and_bound

_EXIT_CODE_RE = re.compile(r"(?:Process )?exited with code (-?\d+)")
_SESSION_UUID_RE = re.compile(
    r"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"
)
# response_item payload types that duplicate another record; skipped to keep one
# event per observed action.
_SKIP_PAYLOAD_TYPES = frozenset({"reasoning"})


class CodexAdapter(SourceAdapter):
    """Read-only adapter for Codex rollout transcripts."""

    provider = "codex"
    adapter_version = "1.0.0"

    def detect(self, source_path: str) -> bool:
        parts = Path(source_path).parts
        return source_path.endswith(".jsonl") and ".codex" in parts and "sessions" in parts

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
            session_id=_session_id_from_path(source_path),
            parent_session_id=None,
            errors=list(errors),
            partial_trailing_line=partial,
            line_count=len(decoded),
        )
        model: str | None = None
        call_times: dict[str, str] = {}

        for line_number, obj in decoded:
            if not isinstance(obj, dict):
                result.note_skip("non_object")
                continue
            record_type = obj.get("type")
            payload = obj.get("payload") if isinstance(obj.get("payload"), dict) else {}
            if record_type == "session_meta":
                self._apply_session_meta(payload, result)
                result.note_skip("session_meta")
            elif record_type == "turn_context":
                candidate = payload.get("model")
                if isinstance(candidate, str) and candidate:
                    model = candidate
                result.note_skip("turn_context")
            elif record_type == "world_state":
                candidate = _world_state_model(payload)
                if candidate:
                    model = model or candidate
                result.note_skip("world_state")
            elif record_type == "response_item":
                self._parse_response_item(
                    obj,
                    payload,
                    line_number,
                    context,
                    generation,
                    model,
                    call_times,
                    result,
                )
            elif record_type == "event_msg":
                self._parse_event_msg(obj, payload, line_number, context, generation, model, result)
            elif record_type in {"compacted"}:
                result.note_skip(f"meta:{record_type}")
            else:
                result.note_skip(f"type:{record_type}")

        attach_git_evidence(result, decoded, context)
        return result

    # -- metadata ----------------------------------------------------------

    def _apply_session_meta(self, payload: dict[str, Any], result: AdapterResult) -> None:
        session_id = payload.get("id") or payload.get("session_id")
        if isinstance(session_id, str) and session_id:
            result.session_id = session_id
        parent = payload.get("parent_thread_id")
        if not isinstance(parent, str) or not parent:
            source = payload.get("source")
            if isinstance(source, dict):
                subagent = source.get("subagent")
                if isinstance(subagent, dict):
                    thread_spawn = subagent.get("thread_spawn")
                    if isinstance(thread_spawn, dict) and isinstance(thread_spawn.get("parent_thread_id"), str):
                        parent = thread_spawn["parent_thread_id"]
        if isinstance(parent, str) and parent and parent != result.session_id:
            result.parent_session_id = parent

    # -- records -----------------------------------------------------------

    def _record(
        self,
        context: AdapterContext,
        result: AdapterResult,
        *,
        event_id: str,
        timestamp: str,
        kind: str,
        model: str | None,
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

    def _parse_response_item(
        self,
        obj: dict[str, Any],
        payload: dict[str, Any],
        line_number: int,
        context: AdapterContext,
        generation: int,
        model: str | None,
        call_times: dict[str, str],
        result: AdapterResult,
    ) -> None:
        timestamp = obj.get("timestamp")
        if not isinstance(timestamp, str) or not timestamp:
            result.note_skip("no_timestamp")
            return
        payload_type = payload.get("type")
        native_id = payload.get("id") if isinstance(payload.get("id"), str) else None
        event_id = native_id or fallback_event_id(self.provider, generation, line_number, payload_type or "record", payload)

        if payload_type == "message":
            text = _flatten_message(payload.get("content"))
            role = payload.get("role")
            if not text:
                result.note_skip("message_empty")
                return
            result.records.append(
                self._record(
                    context,
                    result,
                    event_id=event_id,
                    timestamp=timestamp,
                    kind="message",
                    model=model if role == "assistant" else None,
                    text=redact_and_bound(text),
                )
            )
        elif payload_type == "function_call":
            call_id = payload.get("call_id") if isinstance(payload.get("call_id"), str) else None
            if call_id:
                call_times[call_id] = timestamp
            command = extract_command(payload.get("name"), payload.get("arguments"))
            arguments = payload.get("arguments")
            result.records.append(
                self._record(
                    context,
                    result,
                    event_id=event_id,
                    timestamp=timestamp,
                    kind="tool_call",
                    model=model,
                    tool_name=payload.get("name") if isinstance(payload.get("name"), str) else None,
                    tool_call_id=call_id,
                    command=elide_large_text(command) if command else None,
                    text=elide_large_text(arguments) if isinstance(arguments, str) and arguments else None,
                )
            )
        elif payload_type == "function_call_output":
            call_id = payload.get("call_id") if isinstance(payload.get("call_id"), str) else None
            output = payload.get("output") if isinstance(payload.get("output"), str) else ""
            result.records.append(
                self._record(
                    context,
                    result,
                    event_id=event_id,
                    timestamp=timestamp,
                    kind="tool_result",
                    model=model,
                    tool_call_id=call_id,
                    exit_code=_parse_exit_code(output),
                    duration_ms=_duration_ms(call_times.get(call_id or ""), timestamp),
                    text=elide_large_text(output) if output else None,
                )
            )
        elif payload_type == "custom_tool_call":
            call_id = payload.get("call_id") if isinstance(payload.get("call_id"), str) else None
            if call_id:
                call_times[call_id] = timestamp
            value = payload.get("input")
            result.records.append(
                self._record(
                    context,
                    result,
                    event_id=event_id,
                    timestamp=timestamp,
                    kind="tool_call",
                    model=model,
                    tool_name=payload.get("name") if isinstance(payload.get("name"), str) else None,
                    tool_call_id=call_id,
                    text=elide_large_text(value) if isinstance(value, str) and value else None,
                )
            )
        elif payload_type == "custom_tool_call_output":
            call_id = payload.get("call_id") if isinstance(payload.get("call_id"), str) else None
            value = payload.get("output")
            result.records.append(
                self._record(
                    context,
                    result,
                    event_id=event_id,
                    timestamp=timestamp,
                    kind="tool_result",
                    model=model,
                    tool_call_id=call_id,
                    duration_ms=_duration_ms(call_times.get(call_id or ""), timestamp),
                    text=elide_large_text(value) if isinstance(value, str) and value else None,
                )
            )
        elif payload_type in _SKIP_PAYLOAD_TYPES:
            result.note_skip(f"payload:{payload_type}")
        else:
            result.note_skip(f"payload:{payload_type}")

    def _parse_event_msg(
        self,
        obj: dict[str, Any],
        payload: dict[str, Any],
        line_number: int,
        context: AdapterContext,
        generation: int,
        model: str | None,
        result: AdapterResult,
    ) -> None:
        payload_type = payload.get("type")
        if payload_type == "token_count":
            timestamp = obj.get("timestamp")
            if not isinstance(timestamp, str) or not timestamp:
                result.note_skip("no_timestamp")
                return
            usage = _codex_usage(payload.get("info"))
            if usage is None:
                result.note_skip("token_count_empty")
                return
            event_id = fallback_event_id(self.provider, generation, line_number, "usage", payload)
            result.records.append(
                self._record(
                    context,
                    result,
                    event_id=event_id,
                    timestamp=timestamp,
                    kind="usage",
                    model=model,
                    usage=usage,
                )
            )
        elif payload_type in {"item_completed", "item_started"}:
            # item_completed duplicates the corresponding response_item.
            result.note_skip(f"event_msg:{payload_type}")
        else:
            result.note_skip(f"event_msg:{payload_type}")


def _session_id_from_path(source_path: str) -> str:
    match = _SESSION_UUID_RE.search(Path(source_path).stem)
    if match:
        return match.group(1)
    return Path(source_path).stem or "unknown"


def _world_state_model(payload: dict[str, Any]) -> str | None:
    state = payload.get("state")
    if not isinstance(state, dict):
        return None
    collaboration = state.get("collaboration_mode")
    if isinstance(collaboration, dict) and isinstance(collaboration.get("model"), str):
        return collaboration["model"]
    return None


def _flatten_message(content: Any) -> str:
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""
    parts = []
    for block in content:
        if isinstance(block, dict):
            text = block.get("text")
            if isinstance(text, str) and text:
                parts.append(text)
    return "\n".join(parts)


def _parse_exit_code(output: str) -> int | None:
    match = _EXIT_CODE_RE.search(output)
    if not match:
        return None
    try:
        return int(match.group(1))
    except ValueError:  # pragma: no cover - regex only matches digits
        return None


def _duration_ms(start: str | None, end: str | None) -> int | None:
    if not start or not end:
        return None
    try:
        start_dt = datetime.fromisoformat(start.replace("Z", "+00:00"))
        end_dt = datetime.fromisoformat(end.replace("Z", "+00:00"))
    except ValueError:
        return None
    delta = (end_dt - start_dt).total_seconds() * 1000.0
    if delta < 0:
        return None
    return int(delta)


def _codex_usage(info: Any) -> dict[str, int | float | None] | None:
    if not isinstance(info, dict):
        return None
    selected = info.get("last_token_usage")
    if not isinstance(selected, dict):
        selected = info.get("total_token_usage")
    if not isinstance(selected, dict):
        return None
    usage = {
        "input_tokens": number_or_none(selected.get("input_tokens")),
        "output_tokens": number_or_none(selected.get("output_tokens")),
        "cache_read_tokens": number_or_none(selected.get("cached_input_tokens")),
        "cache_write_tokens": number_or_none(selected.get("cache_write_input_tokens")),
        "total_tokens": number_or_none(selected.get("total_tokens")),
    }
    if all(value is None for value in usage.values()):
        return None
    return usage
