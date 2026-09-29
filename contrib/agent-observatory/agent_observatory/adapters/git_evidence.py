"""Conservatively extract repository and commit evidence from tool transcripts.

The extractor never executes transcript content and never retains raw commands,
outputs, or remote URLs. It recognizes only explicit Git invocations paired with
their tool results, plus the session's observed cwd and Git toplevel paths.
"""

from __future__ import annotations

import json
import re
import shlex
from dataclasses import dataclass, field
from typing import Any, Callable, Iterable
from urllib.parse import urlsplit

from ..contract import normalize_timestamp
from .base import AdapterContext, AdapterResult, iso_from_epoch

_SHA_RE = re.compile(r"^[0-9a-fA-F]{7,64}$")
# Gas City's dispatch ids are exactly 20 hexadecimal characters. Refuse to
# retain arbitrary path components as fleet evidence.
_FLEET_PATH_RE = re.compile(r"(?:^|[/\\])fleet-([0-9a-fA-F]{20})(?=[/\\\s]|$)")
_URL_RE = re.compile(
    r"(?:https?|ssh|git)://[^\s\"'<>]+|(?:[A-Za-z0-9._+-]+@)?[A-Za-z0-9.-]+\.[A-Za-z]{2,}:[^\s\"'<>]+",
    re.IGNORECASE,
)
_COMMIT_LINE_RE = re.compile(r"^\s*\[.*?\s+([0-9a-fA-F]{7,64})\]\s+", re.MULTILINE)
_PUSH_RANGE_RE = re.compile(r"(?<![0-9a-fA-F])([0-9a-fA-F]{7,64})(\.\.\.?)([0-9a-fA-F]{7,64})(?![0-9a-fA-F])")
_LOG_SHA_RE = re.compile(r"^\s*(?:(?:\*\s*)|(?:commit\s+))?([0-9a-fA-F]{7,64})(?:\s|$)")
_HEAD_LINE_RE = re.compile(r"^\s*HEAD\s+([0-9a-fA-F]{7,64})\s*$", re.IGNORECASE)
# `git worktree list` uses one space for the longest path (and when there is
# only one worktree); other rows are padded to align their SHA columns.
_PLAIN_WORKTREE_LINE_RE = re.compile(
    r"^(?P<path>.+?)\s+(?P<sha>[0-9a-fA-F]{7,64})(?=\s+\[|\s+\(|$)"
)


@dataclass(frozen=True)
class _GitIntent:
    kind: str
    args: tuple[str, ...] = ()
    cwd_paths: tuple[str, ...] = ()
    remote_urls: tuple[str, ...] = ()
    head_query: bool = False


@dataclass(frozen=True)
class _ToolCall:
    call_id: str | None
    command: str
    timestamp: str | None
    cwd_paths: tuple[str, ...]
    intents: tuple[_GitIntent, ...]


@dataclass
class _SessionEvidence:
    paths_seen: bool = False
    fleet_ids: set[str] = field(default_factory=set)
    remote_repos: set[str] = field(default_factory=set)
    fingerprints: dict[tuple[str, str | None], set[str]] = field(default_factory=dict)
    head_shas_by_call: dict[str, set[str]] = field(default_factory=dict)
    repos_by_call: dict[str, set[str]] = field(default_factory=dict)
    pushed_fleet_ids: set[str] = field(default_factory=set)
    multi_ref_push_calls: set[str] = field(default_factory=set)


def normalize_repo_identity(value: str | None) -> str | None:
    """Normalize a remote URL or explicit ``owner/name`` to ``owner/name``.

    URLs are reduced to the final two path components (namespace/repository),
    and credentials, hosts, query strings, and ``.git`` are discarded. Local
    filesystem paths are not guessed to be repository identities.
    """

    if not isinstance(value, str):
        return None
    candidate = value.strip().strip("\"'<>(),;")
    if not candidate:
        return None

    path = ""
    if "://" in candidate:
        try:
            path = urlsplit(candidate).path
        except ValueError:
            return None
    else:
        if candidate.startswith("/") or re.match(r"^[A-Za-z]:[/\\\\]", candidate):
            return None
        scp = re.match(r"^(?:[^@/]+@)?([^/:]+):(.+)$", candidate)
        if scp:
            # SCP remotes have no URL parser to discard their suffixes. Strip
            # them explicitly, and never let userinfo/host data reach the path.
            path = scp.group(2).split("?", 1)[0].split("#", 1)[0]
        else:
            path = candidate.split("?", 1)[0].split("#", 1)[0]
            path = path.removeprefix("github.com/").removeprefix("gitlab.com/")

    parts = [part for part in path.strip("/").split("/") if part and part != "."]
    if parts and parts[-1].lower().endswith(".git"):
        parts[-1] = parts[-1][:-4]
    if len(parts) < 2:
        return None
    owner, name = parts[-2:]
    if not owner or not name or owner in {"..", "."} or name in {"..", "."}:
        return None
    return f"{owner}/{name}".lower()


def attach_git_evidence(
    result: AdapterResult,
    decoded: Iterable[tuple[int, Any]],
    context: AdapterContext,
    *,
    session_id_for_obj: Callable[[dict[str, Any]], str | None] | None = None,
) -> None:
    """Attach explicit Git fingerprints and repository bindings to *result*."""

    default_session_id = result.session_id
    sessions: dict[str, _SessionEvidence] = {}
    pending: dict[tuple[str, str], _ToolCall] = {}
    session_ids: set[str] = {default_session_id} if default_session_id else set()
    fallback_repo = normalize_repo_identity(context.repo)

    def state_for(session_id: str) -> _SessionEvidence:
        session_ids.add(session_id)
        return sessions.setdefault(session_id, _SessionEvidence())

    for _line_number, obj in decoded:
        if not isinstance(obj, dict):
            continue
        session_id = (
            session_id_for_obj(obj)
            if session_id_for_obj is not None
            else default_session_id
        )
        if not isinstance(session_id, str) or not session_id:
            continue
        state = state_for(session_id)
        for path in _object_paths(result.provider, obj):
            _add_path(state, path)

        for call_id, command, cwd_paths in _tool_calls(result.provider, obj):
            for path in cwd_paths:
                _add_path(state, path)
            intents = _git_intents(command)
            for intent in intents:
                for path in intent.cwd_paths:
                    _add_path(state, path)
                for remote_url in intent.remote_urls:
                    repo = normalize_repo_identity(remote_url)
                    if repo:
                        state.remote_repos.add(repo)
                        if call_id:
                            state.repos_by_call.setdefault(call_id, set()).add(repo)
            if call_id:
                pending[(session_id, call_id)] = _ToolCall(
                    call_id=call_id,
                    command=command,
                    timestamp=_record_timestamp(result.provider, obj),
                    cwd_paths=cwd_paths,
                    intents=intents,
                )

        for call_id, output in _tool_results(result.provider, obj):
            call = pending.pop((session_id, call_id), None)
            if call is None:
                # An orphan result is not sufficient to identify a Git command.
                continue
            observed_at = _record_timestamp(result.provider, obj) or call.timestamp
            _observe_result(state, call, output, observed_at)

    # Include every emitted session even if the source had only metadata records.
    for record in result.records:
        session_id = record.get("session_id")
        if isinstance(session_id, str) and session_id:
            state_for(session_id)

    session_repos: dict[str, str | None] = {}
    for session_id in sorted(session_ids):
        state = state_for(session_id)
        if len(state.remote_repos) == 1:
            repo = next(iter(state.remote_repos))
        elif fallback_repo and fallback_repo in state.remote_repos:
            # An explicit caller scope may disambiguate several observed remotes.
            repo = fallback_repo
        elif not state.remote_repos:
            repo = fallback_repo
        else:
            repo = None
        session_repos[session_id] = repo

        notes: list[str] = []
        if not state.paths_seen:
            notes.append("repo_path:none (no session cwd or git toplevel path observed)")
        if len(state.remote_repos) > 1 and repo is None:
            notes.append("repo:none (multiple remotes observed without an explicit matching scope)")
        elif not state.remote_repos and repo is None:
            notes.append("repo:none (no remote URL or valid configured owner/name observed)")
        elif not state.remote_repos and repo is not None:
            notes.append("repo: no transcript remote URL; used explicit repo context")
        if not state.fingerprints:
            notes.append("commit_sha:none (no SHA in a paired git commit, push, or rev-parse result)")
        elif any(observed_at is None for _sha, observed_at in state.fingerprints):
            notes.append("commit_sha_timestamp:none (SHA observed without a usable transcript timestamp)")
        if state.multi_ref_push_calls:
            notes.append("commit_sha:none (multi-ref push had multiple new heads; no unique commit SHA)")
        if state.fleet_ids and not state.pushed_fleet_ids:
            notes.append("fleet_push_head:none (fleet worktree observed without a pushed head SHA)")
        if notes:
            result.session_evidence_notes[session_id] = notes

        for (sha, observed_at), evidence_kinds in sorted(
            state.fingerprints.items(), key=lambda item: (item[0][0], item[0][1] or "")
        ):
            result.session_fingerprints.append(
                {
                    "session": [context.city_id, context.host_id, result.provider, session_id],
                    "type": "commit_sha",
                    "value": sha,
                    "observed_at": observed_at,
                    "evidence": ";".join(sorted(evidence_kinds)),
                }
            )

    for record in result.records:
        session_id = record.get("session_id")
        if not isinstance(session_id, str):
            continue
        state = sessions.get(session_id)
        repo = session_repos.get(session_id)
        call_id = record.get("tool_call_id")
        if record.get("kind") == "tool_result" and isinstance(call_id, str) and state:
            call_repos = state.repos_by_call.get(call_id, set())
            if len(call_repos) == 1:
                repo = next(iter(call_repos))
            head_shas = state.head_shas_by_call.get(call_id, set())
            if len(head_shas) == 1:
                record["commit_sha"] = next(iter(head_shas))
        record["repo"] = repo


def _observe_result(
    state: _SessionEvidence,
    call: _ToolCall,
    output: str,
    observed_at: str | None,
) -> None:
    for intent in call.intents:
        for remote_url in intent.remote_urls:
            _add_call_repo(state, call.call_id, remote_url)
        if intent.kind == "remote":
            for remote_url in _remote_urls(output):
                _add_call_repo(state, call.call_id, remote_url)
        elif intent.kind == "push":
            for remote_url in _push_urls(output):
                _add_call_repo(state, call.call_id, remote_url)
        elif intent.kind == "clone":
            for remote_url in _remote_urls(output):
                _add_call_repo(state, call.call_id, remote_url)

        if intent.kind == "toplevel":
            for line in output.splitlines():
                path = line.strip()
                if path.startswith(("/", "\\")):
                    _add_path(state, path)
        elif intent.kind == "pwd":
            for line in output.splitlines():
                path = line.strip()
                if path.startswith(("/", "\\")):
                    _add_path(state, path)
        elif intent.kind == "worktree":
            _observe_worktree_list(state, call, output, observed_at)
        elif intent.kind == "commit":
            for match in _COMMIT_LINE_RE.finditer(output):
                sha = match.group(1).lower()
                _add_fingerprint(state, sha, observed_at, "git_commit_output")
                _add_head(state, call.call_id, sha)
        elif intent.kind == "push":
            fleet_id = _call_fleet_id(call)
            push_heads: set[str] = set()
            for line in output.splitlines():
                for match in _PUSH_RANGE_RE.finditer(line):
                    old_sha, _separator, new_sha = match.groups()
                    _add_fingerprint(state, old_sha, observed_at, _fleet_evidence("git_push_previous", fleet_id))
                    _add_fingerprint(state, new_sha, observed_at, _fleet_evidence("git_push_head", fleet_id))
                    _add_head(state, call.call_id, new_sha)
                    push_heads.add(new_sha.lower())
                    if fleet_id:
                        state.pushed_fleet_ids.add(fleet_id)
            if call.call_id and len(push_heads) > 1:
                state.multi_ref_push_calls.add(call.call_id)
        elif intent.kind == "rev_parse":
            for line in output.splitlines():
                sha = line.strip()
                if _SHA_RE.fullmatch(sha):
                    evidence = "git_rev_parse_head" if intent.head_query else "git_rev_parse"
                    _add_fingerprint(state, sha, observed_at, _fleet_evidence(evidence, _call_fleet_id(call)))
                    if intent.head_query:
                        _add_head(state, call.call_id, sha)
        elif intent.kind in {"log", "show", "rev_list", "show_ref", "branch"}:
            _observe_sha_listing(state, call, intent.kind, output, observed_at)


def _observe_sha_listing(
    state: _SessionEvidence,
    call: _ToolCall,
    kind: str,
    output: str,
    observed_at: str | None,
) -> None:
    evidence = {
        "log": "git_log",
        "show": "git_show",
        "rev_list": "git_rev_list",
        "show_ref": "git_show_ref",
        "branch": "git_branch_verbose",
    }[kind]
    fleet_id = _call_fleet_id(call)
    for line in output.splitlines():
        sha: str | None = None
        if kind in {"log", "rev_list"}:
            match = _LOG_SHA_RE.match(line)
            if match:
                sha = match.group(1)
        elif kind == "show":
            match = re.match(r"^\s*commit\s+([0-9a-fA-F]{7,64})(?:\s|$)", line)
            if match:
                sha = match.group(1)
        elif kind == "show_ref":
            match = re.match(r"^\s*([0-9a-fA-F]{7,64})\s+\S+", line)
            if match:
                sha = match.group(1)
        elif kind == "branch":
            match = re.match(r"^\s*[* ]?\S+\s+([0-9a-fA-F]{7,64})(?:\s|$)", line)
            if match:
                sha = match.group(1)
        if sha:
            _add_fingerprint(state, sha, observed_at, _fleet_evidence(evidence, fleet_id))


def _observe_worktree_list(
    state: _SessionEvidence,
    call: _ToolCall,
    output: str,
    observed_at: str | None,
) -> None:
    current_fleet_id: str | None = None
    for line in output.splitlines():
        if line.startswith("worktree "):
            path = line[len("worktree ") :].strip()
            _add_path(state, path)
            current_fleet_id = _fleet_id(path)
        elif line.startswith("HEAD "):
            match = _HEAD_LINE_RE.match(line)
            if match:
                sha = match.group(1)
                _add_fingerprint(
                    state,
                    sha,
                    observed_at,
                    _fleet_evidence("git_worktree_head", current_fleet_id),
                )
                _add_head(state, call.call_id, sha)
        elif not line.strip():
            current_fleet_id = None
        else:
            # Non-porcelain `git worktree list` prints one row per worktree:
            # path, abbreviated HEAD, then a branch/detached label.
            match = _PLAIN_WORKTREE_LINE_RE.match(line)
            if match:
                path = match.group("path").rstrip()
                sha = match.group("sha")
                if path:
                    _add_path(state, path)
                    fleet_id = _fleet_id(path)
                    _add_fingerprint(
                        state,
                        sha,
                        observed_at,
                        _fleet_evidence("git_worktree_head", fleet_id),
                    )
                    _add_head(state, call.call_id, sha)
                current_fleet_id = None


def _add_call_repo(state: _SessionEvidence, call_id: str | None, remote_url: str) -> None:
    repo = normalize_repo_identity(remote_url)
    if not repo:
        return
    state.remote_repos.add(repo)
    if call_id:
        state.repos_by_call.setdefault(call_id, set()).add(repo)


def _add_fingerprint(
    state: _SessionEvidence,
    value: str,
    observed_at: str | None,
    evidence: str,
) -> None:
    if not _SHA_RE.fullmatch(value):
        return
    try:
        timestamp = normalize_timestamp(observed_at) if observed_at else None
    except Exception:
        timestamp = None
    key = (value.lower(), timestamp)
    state.fingerprints.setdefault(key, set()).add(evidence)


def _add_head(state: _SessionEvidence, call_id: str | None, sha: str) -> None:
    if call_id and _SHA_RE.fullmatch(sha):
        state.head_shas_by_call.setdefault(call_id, set()).add(sha.lower())


def _fleet_evidence(kind: str, fleet_id: str | None) -> str:
    return f"{kind}:fleet_worktree={fleet_id}" if fleet_id else kind


def _call_fleet_id(call: _ToolCall) -> str | None:
    command_paths = list(call.cwd_paths)
    for intent in call.intents:
        command_paths.extend(intent.cwd_paths)
    call_ids = {fleet_id for path in command_paths if (fleet_id := _fleet_id(path))}
    if len(call_ids) == 1:
        return next(iter(call_ids))
    return None


def _add_path(state: _SessionEvidence, path: str) -> None:
    candidate = path.strip().strip("\"'")
    if not candidate:
        return
    state.paths_seen = True
    fleet_id = _fleet_id(candidate)
    if fleet_id:
        state.fleet_ids.add(fleet_id)


def _fleet_id(path: str) -> str | None:
    match = _FLEET_PATH_RE.search(path)
    return match.group(1).lower() if match else None


def _record_timestamp(provider: str, obj: dict[str, Any]) -> str | None:
    value = obj.get("time") if provider == "dsh" else obj.get("timestamp")
    if isinstance(value, str):
        stripped = value.strip()
        if stripped.isascii() and stripped.isdigit():
            try:
                value = int(stripped)
            except ValueError:
                return None
        else:
            try:
                return normalize_timestamp(stripped)
            except Exception:
                return None
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        timestamp = iso_from_epoch(value)
        if timestamp:
            try:
                return normalize_timestamp(timestamp)
            except Exception:
                return None
    return None


def _object_paths(provider: str, obj: dict[str, Any]) -> list[str]:
    values: list[Any] = []
    if isinstance(obj.get("cwd"), str):
        values.append(obj["cwd"])
    if provider == "codex":
        payload = obj.get("payload") if isinstance(obj.get("payload"), dict) else {}
        if isinstance(payload.get("cwd"), str):
            values.append(payload["cwd"])
        roots = payload.get("workspace_roots")
        if isinstance(roots, list):
            values.extend(value for value in roots if isinstance(value, str))
    elif provider == "dsh":
        data = obj.get("data") if isinstance(obj.get("data"), dict) else {}
        if isinstance(data.get("cwd"), str):
            values.append(data["cwd"])
    return [value for value in values if isinstance(value, str)]


def _tool_calls(provider: str, obj: dict[str, Any]) -> list[tuple[str | None, str, tuple[str, ...]]]:
    calls: list[tuple[str | None, str, tuple[str, ...]]] = []
    if provider == "claude" and obj.get("type") == "assistant":
        message = obj.get("message") if isinstance(obj.get("message"), dict) else {}
        content = message.get("content")
        blocks = content if isinstance(content, list) else []
        for block in blocks:
            if not isinstance(block, dict) or block.get("type") != "tool_use":
                continue
            command = _command_text(block.get("input"))
            if command:
                calls.append((
                    _string_or_none(block.get("id")),
                    command,
                    _argument_cwds(block.get("input")),
                ))
    elif provider == "codex" and obj.get("type") == "response_item":
        payload = obj.get("payload") if isinstance(obj.get("payload"), dict) else {}
        if payload.get("type") in {"function_call", "custom_tool_call"}:
            arguments = payload.get("arguments", payload.get("input"))
            command = _command_text(arguments)
            if command:
                calls.append((
                    _string_or_none(payload.get("call_id")),
                    command,
                    _argument_cwds(arguments),
                ))
    elif provider == "dsh" and obj.get("type") == "tool/call":
        payload = obj.get("data") if isinstance(obj.get("data"), dict) else {}
        arguments = payload.get("arguments", payload.get("input"))
        command = _command_text(arguments)
        if command:
            calls.append((
                _string_or_none(payload.get("callId")),
                command,
                _argument_cwds(arguments),
            ))
    return calls


def _tool_results(provider: str, obj: dict[str, Any]) -> list[tuple[str, str]]:
    results: list[tuple[str, str]] = []
    if provider == "claude" and obj.get("type") == "user":
        message = obj.get("message") if isinstance(obj.get("message"), dict) else {}
        content = message.get("content")
        blocks = content if isinstance(content, list) else []
        top_result = obj.get("toolUseResult") if isinstance(obj.get("toolUseResult"), dict) else {}
        top_text = "\n".join(
            value for value in (top_result.get("stdout"), top_result.get("stderr"))
            if isinstance(value, str) and value
        )
        for block in blocks:
            if not isinstance(block, dict) or block.get("type") != "tool_result":
                continue
            call_id = _string_or_none(block.get("tool_use_id"))
            output = _text_value(block.get("content")) or top_text
            if call_id and output:
                results.append((call_id, output))
    elif provider == "codex" and obj.get("type") == "response_item":
        payload = obj.get("payload") if isinstance(obj.get("payload"), dict) else {}
        if payload.get("type") in {"function_call_output", "custom_tool_call_output"}:
            call_id = _string_or_none(payload.get("call_id"))
            output = _text_value(payload.get("output"))
            if call_id and output:
                results.append((call_id, output))
    elif provider == "dsh" and obj.get("type") == "tool/result":
        payload = obj.get("data") if isinstance(obj.get("data"), dict) else {}
        message = payload.get("message") if isinstance(payload.get("message"), dict) else {}
        source = message.get("source") if isinstance(message.get("source"), dict) else {}
        default_call_id = _string_or_none(source.get("callId"))
        content = message.get("content")
        blocks = content if isinstance(content, list) else []
        for block in blocks:
            if not isinstance(block, dict) or block.get("type") != "tool-result":
                continue
            call_id = _string_or_none(block.get("toolCallId")) or default_call_id
            output = _text_value(block.get("content"))
            if call_id and output:
                results.append((call_id, output))
        if not results and default_call_id:
            output = _text_value(content)
            if output:
                results.append((default_call_id, output))
    return results


def _argument_cwds(value: Any) -> tuple[str, ...]:
    decoded = _decode_argument(value)
    found: list[str] = []
    if isinstance(decoded, dict):
        cwd = decoded.get("cwd")
        if isinstance(cwd, str) and cwd:
            found.append(cwd)
        for key in ("cmd", "command", "script", "input"):
            nested = decoded.get(key)
            if isinstance(nested, dict):
                found.extend(_argument_cwds(nested))
    return tuple(dict.fromkeys(found))


def _command_text(value: Any) -> str | None:
    decoded = _decode_argument(value)
    if isinstance(decoded, str):
        return decoded if decoded.strip() else None
    if isinstance(decoded, dict):
        for key in ("cmd", "command", "script", "command_line", "shell_command"):
            nested = decoded.get(key)
            if isinstance(nested, (str, dict, list)):
                command = _command_text(nested)
                if command:
                    return command
        return None
    if isinstance(decoded, list) and all(isinstance(item, str) for item in decoded):
        try:
            return shlex.join(decoded)
        except AttributeError:  # pragma: no cover - Python 3.11 has shlex.join
            return " ".join(shlex.quote(item) for item in decoded)
    return None


def _decode_argument(value: Any) -> Any:
    if not isinstance(value, str):
        return value
    stripped = value.strip()
    if stripped.startswith(("{", "[")):
        try:
            return json.loads(stripped)
        except ValueError:
            pass
    return value


def _text_value(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "\n".join(part for item in value if (part := _text_value(item)))
    if isinstance(value, dict):
        for key in ("text", "output", "content"):
            if key in value:
                text = _text_value(value[key])
                if text:
                    return text
    return ""


def _string_or_none(value: Any) -> str | None:
    return value if isinstance(value, str) and value else None


def _git_intents(command: str) -> tuple[_GitIntent, ...]:
    intents: list[_GitIntent] = []
    for segment in _shell_segments(command):
        try:
            tokens = shlex.split(segment, posix=True)
        except ValueError:
            continue
        if not tokens:
            continue
        intents.extend(_intents_from_tokens(tokens))
        # Shell wrappers keep the actual command in their -c argument. Recurse
        # only through that explicit shell payload, not arbitrary quoted text.
        for index, token in enumerate(tokens[:-1]):
            if token in {"-c", "-lc", "-cl", "-ec", "-elc", "-lec"}:
                intents.extend(_git_intents_nested(tokens[index + 1]))
    return tuple(intents)


def _git_intents_nested(command: str) -> tuple[_GitIntent, ...]:
    return _git_intents(command)


def _shell_segments(command: str) -> list[str]:
    """Split shell control operators without splitting quoted command text."""

    segments: list[str] = []
    start = 0
    quote: str | None = None
    escaped = False
    index = 0
    while index < len(command):
        char = command[index]
        if escaped:
            escaped = False
            index += 1
            continue
        if char == "\\" and quote != "'":
            escaped = True
            index += 1
            continue
        if quote:
            if char == quote:
                quote = None
            index += 1
            continue
        if char in {"'", '"'}:
            quote = char
            index += 1
            continue
        if char in {";", "|", "&", "\n", "\r"}:
            segment = command[start:index].strip()
            if segment:
                segments.append(segment)
            if char in {"|", "&"} and index + 1 < len(command) and command[index + 1] == char:
                index += 1
            start = index + 1
        index += 1
    tail = command[start:].strip()
    if tail:
        segments.append(tail)
    return segments


def _intents_from_tokens(tokens: list[str]) -> list[_GitIntent]:
    intents: list[_GitIntent] = []
    index = 0
    while index < len(tokens):
        token = tokens[index]
        if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*=.*", token):
            index += 1
            continue
        if token in {"command", "builtin", "exec", "nohup", "time"}:
            index += 1
            if token == "command" and index < len(tokens) and tokens[index] in {"-p", "--"}:
                index += 1
            continue
        if token == "nice":
            index += 1
            if index < len(tokens) and tokens[index] in {"-n", "--adjustment"}:
                index += 2
            continue
        if token in {"env", "sudo"}:
            wrapper = token
            index += 1
            value_options = (
                {"-u", "--unset", "-C", "--chdir", "-S", "--split-string"}
                if wrapper == "env"
                else {"-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-C", "--chdir"}
            )
            while index < len(tokens) and tokens[index].startswith("-"):
                option = tokens[index]
                index += 1
                if option in value_options:
                    index += 1
            continue
        break

    if index >= len(tokens):
        return intents
    token = tokens[index]
    if token == "pwd":
        return [_GitIntent("pwd")]
    if token == "cd":
        path_index = index + 2 if index + 1 < len(tokens) and tokens[index + 1] == "--" else index + 1
        if path_index < len(tokens):
            return [_GitIntent("path", cwd_paths=(tokens[path_index],))]
        return intents
    if not _is_git(token):
        return intents

    args = tokens[index + 1 :]
    subcommand, rest, cwd_paths = _git_subcommand(args)
    if not subcommand:
        return intents
    rest_lower = [arg.lower() for arg in rest]
    remote_urls: tuple[str, ...] = ()
    if subcommand in {"clone", "push"} or (
        subcommand == "remote" and rest_lower[:1] in (["add"], ["set-url"])
    ):
        remote_urls = tuple(_remote_urls(" ".join(rest)))
    if subcommand == "rev-parse":
        if "--show-toplevel" in rest_lower:
            kind = "toplevel"
        elif any(
            arg in rest_lower
            for arg in (
                "--git-dir",
                "--show-prefix",
                "--show-cdup",
                "--is-inside-work-tree",
                "--is-bare-repository",
                "--show-object-format",
            )
        ):
            kind = "other"
        else:
            kind = "rev_parse"
        head_query = any(arg in {"head", "@"} for arg in rest_lower)
        intents.append(_GitIntent(kind, tuple(rest), tuple(cwd_paths), remote_urls, head_query))
    elif subcommand == "remote":
        if any(arg in {"-v", "--verbose", "get-url", "add", "set-url"} for arg in rest_lower):
            intents.append(_GitIntent("remote", tuple(rest), tuple(cwd_paths), remote_urls))
    elif subcommand == "config":
        joined = " ".join(rest_lower)
        if "remote." in joined and ".url" in joined:
            intents.append(_GitIntent("remote", tuple(rest), tuple(cwd_paths)))
    elif subcommand in {"clone", "commit", "push", "log", "show", "rev-list", "show-ref", "branch", "worktree"}:
        if subcommand == "branch" and not any(arg in {"-v", "-vv", "--verbose"} for arg in rest_lower):
            return intents
        if subcommand == "worktree" and not any(arg == "list" for arg in rest_lower):
            return intents
        if subcommand == "worktree":
            kind = "worktree"
        elif subcommand == "rev-list":
            kind = "rev_list"
        elif subcommand == "show-ref":
            kind = "show_ref"
        else:
            kind = subcommand
        intents.append(_GitIntent(kind, tuple(rest), tuple(cwd_paths), remote_urls))
    return intents


def _git_subcommand(args: list[str]) -> tuple[str | None, list[str], list[str]]:
    index = 0
    cwd_paths: list[str] = []
    value_options = {"-C", "-c", "--git-dir", "--work-tree", "--namespace"}
    flag_options = {
        "--no-pager",
        "--paginate",
        "--no-optional-locks",
        "--literal-pathspecs",
        "--glob-pathspecs",
        "--noglob-pathspecs",
        "--icase-pathspecs",
    }
    while index < len(args):
        arg = args[index]
        if arg in value_options:
            if index + 1 >= len(args):
                return None, [], cwd_paths
            if arg == "-C":
                cwd_paths.append(args[index + 1])
            index += 2
        elif arg.startswith("-C") and len(arg) > 2:
            cwd_paths.append(arg[2:])
            index += 1
        elif arg.startswith("--git-dir=") or arg.startswith("--work-tree=") or arg.startswith("--namespace="):
            index += 1
        elif arg in flag_options:
            index += 1
        elif arg.startswith("-"):
            # A Git global option not recognized here is not safe to interpret as
            # a subcommand; stop rather than guessing at its arity.
            return None, [], cwd_paths
        else:
            return arg.lower(), args[index + 1 :], cwd_paths
    return None, [], cwd_paths


def _is_git(token: str) -> bool:
    basename = token.replace("\\", "/").rsplit("/", 1)[-1].lower()
    return basename in {"git", "git.exe"}


def _remote_urls(text: str) -> list[str]:
    found: list[str] = []
    for match in _URL_RE.finditer(text):
        value = match.group(0).rstrip(".,;:!?)]}")
        if normalize_repo_identity(value) and value not in found:
            found.append(value)
    return found


def _push_urls(output: str) -> list[str]:
    found: list[str] = []
    for line in output.splitlines():
        stripped = line.strip()
        if stripped.lower().startswith("to "):
            found.extend(_remote_urls(stripped[3:]))
    return list(dict.fromkeys(found))
