"""Immutable-safe enrichment from explicit Gas City session metadata.

The importer trusts only a GC session's explicit ``template`` and
``session_key``. It never infers a role from a provider session name, GC bead ID,
transcript content, or message role. Repository identity is derived from an
explicit ``repo`` field or a local worktree's origin remote; only the normalized
owner/repository value is persisted.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from .adapters.git_evidence import normalize_repo_identity
from .canonical import canonical_json, identity_key, sha256_text
from .errors import ObservatoryError

_SUPPORTED_PROVIDERS = frozenset({"claude", "codex", "dsh"})
_REMOTE_SCHEMES = frozenset({"http", "https", "ssh", "git"})
_GIT_CONFIG_TIMEOUT_SECONDS = 3.0


def _reject_json_constant(value: str) -> Any:
    raise ValueError(f"non-finite JSON constant {value!r} is not allowed")


@dataclass(frozen=True)
class _SessionBinding:
    provider: str
    session_id: str
    template: str
    repo: str | None
    repo_source: str | None
    repo_ambiguous: bool = False


@dataclass
class GCEnrichmentRun:
    """Counts-only result; identifiers, paths, and raw GC metadata are omitted."""

    rows_read: int = 0
    rows_usable: int = 0
    metadata_skipped: int = 0
    sessions_matched: int = 0
    sessions_unmatched: int = 0
    bindings_written: int = 0
    role_bindings_written: int = 0
    repo_bindings_written: int = 0
    repo_ambiguous: int = 0
    conflicts: int = 0

    def to_dict(self) -> dict[str, int]:
        return {
            "rows_read": self.rows_read,
            "rows_usable": self.rows_usable,
            "metadata_skipped": self.metadata_skipped,
            "sessions_matched": self.sessions_matched,
            "sessions_unmatched": self.sessions_unmatched,
            "bindings_written": self.bindings_written,
            "role_bindings_written": self.role_bindings_written,
            "repo_bindings_written": self.repo_bindings_written,
            "repo_ambiguous": self.repo_ambiguous,
            "conflicts": self.conflicts,
        }


def enrich_gc_sessions(
    store: Any,
    source_path: str | os.PathLike[str],
    *,
    city_id: str,
    host_id: str,
) -> GCEnrichmentRun:
    """Import exact GC session-key/template bindings from an explicit JSON export.

    The input may be the object emitted by ``gc session list --json`` or an
    array of its session rows. A row binds only when ``provider`` and
    ``session_key`` exactly identify a session already present in ``sessions``
    or ``session_fingerprints``. ``id`` and ``session_name`` are deliberately
    ignored because they are not provider-session keys.
    """

    city_id = city_id.strip() if isinstance(city_id, str) else ""
    host_id = host_id.strip() if isinstance(host_id, str) else ""
    if not city_id or not host_id:
        raise ObservatoryError("GC enrichment requires non-empty city_id and host_id")

    input_path = Path(source_path)
    try:
        data = input_path.read_bytes()
    except OSError as exc:
        raise ObservatoryError(f"cannot read GC session metadata: {exc}") from exc
    try:
        document = json.loads(data.decode("utf-8"), parse_constant=_reject_json_constant)
    except (UnicodeDecodeError, ValueError) as exc:
        raise ObservatoryError("GC session metadata is not valid UTF-8 JSON") from exc

    if isinstance(document, list):
        rows = document
    elif isinstance(document, dict) and isinstance(document.get("sessions"), list):
        rows = document["sessions"]
    else:
        raise ObservatoryError(
            "GC session metadata must be an array or an object with a sessions array"
        )

    result = GCEnrichmentRun(rows_read=len(rows))
    remote_cache: dict[str, str | None] = {}
    bindings: dict[tuple[str, str], _SessionBinding] = {}
    template_conflicts: set[tuple[str, str]] = set()
    repo_ambiguous_keys: set[tuple[str, str]] = set()

    for raw in rows:
        if not isinstance(raw, dict):
            result.metadata_skipped += 1
            continue
        provider = _nonempty_string(raw.get("provider"))
        session_id = _nonempty_string(raw.get("session_key")) or _nonempty_string(
            raw.get("provider_session_id")
        )
        template = _nonempty_string(raw.get("template"))
        if (
            provider is None
            or provider.lower() not in _SUPPORTED_PROVIDERS
            or session_id is None
            or template is None
        ):
            result.metadata_skipped += 1
            continue

        provider = provider.lower()
        repo, repo_source, ambiguous = _repository_for_row(raw, remote_cache)
        key = (provider, session_id)
        candidate = _SessionBinding(
            provider=provider,
            session_id=session_id,
            template=template,
            repo=repo,
            repo_source="ambiguous" if ambiguous else repo_source,
            repo_ambiguous=ambiguous,
        )
        result.rows_usable += 1
        if ambiguous:
            repo_ambiguous_keys.add(key)

        existing = bindings.get(key)
        if existing is None:
            bindings[key] = candidate
            continue
        if existing.template != candidate.template:
            template_conflicts.add(key)
            continue
        if existing.repo_ambiguous or candidate.repo_ambiguous:
            bindings[key] = _SessionBinding(
                provider=provider,
                session_id=session_id,
                template=template,
                repo=None,
                repo_source="ambiguous",
                repo_ambiguous=True,
            )
            repo_ambiguous_keys.add(key)
            continue
        if existing.repo and candidate.repo and existing.repo != candidate.repo:
            bindings[key] = _SessionBinding(
                provider=provider,
                session_id=session_id,
                template=template,
                repo=None,
                repo_source="ambiguous",
                repo_ambiguous=True,
            )
            repo_ambiguous_keys.add(key)
            continue
        if existing.repo is None and candidate.repo is not None:
            bindings[key] = candidate

    result.repo_ambiguous = len(repo_ambiguous_keys)
    result.conflicts = len(template_conflicts)

    store.conn.execute("BEGIN IMMEDIATE")
    try:
        for key, binding in sorted(bindings.items()):
            if key in template_conflicts:
                continue
            session_identity = (city_id, host_id, binding.provider, binding.session_id)
            if not _session_is_known(store, session_identity):
                result.sessions_unmatched += 1
                continue
            result.sessions_matched += 1

            current = store.conn.execute(
                "SELECT template, repo, repo_source, source_sha256 FROM session_enrichment "
                "WHERE city_id = ? AND host_id = ? AND provider = ? AND session_id = ?",
                session_identity,
            ).fetchone()
            if current is not None and current["template"] != binding.template:
                result.conflicts += 1
                continue
            repo_conflict = False
            conflicting_repo = binding.repo_ambiguous or (
                current is not None
                and current["repo"] is not None
                and binding.repo is not None
                and current["repo"] != binding.repo
            )
            if current is not None and current["repo"] is not None and conflicting_repo:
                # Never replace a durable repo binding with conflicting metadata.
                if not binding.repo_ambiguous:
                    result.conflicts += 1
                    result.repo_ambiguous += 1
                repo_conflict = True
                effective_repo = current["repo"]
                effective_repo_source = current["repo_source"]
            else:
                effective_repo = current["repo"] if current is not None and current["repo"] else binding.repo
                effective_repo_source = (
                    current["repo_source"]
                    if current is not None and current["repo"]
                    else binding.repo_source
                )
            source_sha256 = _binding_sha256(
                session_identity,
                binding.template,
                effective_repo,
                effective_repo_source,
            )

            if current is None:
                store.conn.execute(
                    "INSERT INTO session_enrichment(city_id, host_id, provider, session_id, "
                    "template, repo, repo_source, source_sha256) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                    (*session_identity, binding.template, effective_repo, effective_repo_source, source_sha256),
                )
                result.bindings_written += 1
                result.repo_bindings_written += int(effective_repo is not None)
            else:
                context_changed = (
                    current["template"] != binding.template
                    or current["repo"] != effective_repo
                    or current["repo_source"] != effective_repo_source
                )
                source_changed = current["source_sha256"] != source_sha256
                if not repo_conflict and (context_changed or source_changed):
                    store.conn.execute(
                        "UPDATE session_enrichment SET template = ?, repo = ?, repo_source = ?, "
                        "source_sha256 = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') "
                        "WHERE city_id = ? AND host_id = ? AND provider = ? AND session_id = ?",
                        (
                            binding.template,
                            effective_repo,
                            effective_repo_source,
                            source_sha256,
                            *session_identity,
                        ),
                    )
                    if context_changed:
                        result.bindings_written += 1
                    if current["repo"] is None and effective_repo is not None:
                        result.repo_bindings_written += 1

            session_row = store.conn.execute(
                "SELECT role FROM sessions WHERE city_id = ? AND host_id = ? AND provider = ? "
                "AND session_id = ?",
                session_identity,
            ).fetchone()
            if session_row is not None and session_row["role"] != binding.template:
                store.conn.execute(
                    "UPDATE sessions SET role = ? WHERE city_id = ? AND host_id = ? "
                    "AND provider = ? AND session_id = ?",
                    (binding.template, *session_identity),
                )
                result.role_bindings_written += 1

        store.conn.execute("COMMIT")
    except BaseException:
        if store.conn.in_transaction:
            store.conn.execute("ROLLBACK")
        raise
    return result


def _binding_sha256(
    identity: tuple[str, str, str, str],
    template: str,
    repo: str | None,
    repo_source: str | None,
) -> str:
    city_id, host_id, provider, session_id = identity
    return sha256_text(
        canonical_json(
            {
                "city_id": city_id,
                "host_id": host_id,
                "provider": provider,
                "session_id": session_id,
                "template": template,
                "repo": repo,
                "repo_source": repo_source,
            }
        )
    )


def _session_is_known(store: Any, identity: tuple[str, str, str, str]) -> bool:
    city_id, host_id, provider, session_id = identity
    fingerprint_key = identity_key(identity)
    return bool(
        store.conn.execute(
            "SELECT EXISTS(SELECT 1 FROM sessions WHERE city_id = ? AND host_id = ? "
            "AND provider = ? AND session_id = ?) OR EXISTS("
            "SELECT 1 FROM session_fingerprints WHERE session_json = ?)",
            (city_id, host_id, provider, session_id, fingerprint_key),
        ).fetchone()[0]
    )


def _normalize_explicit_repo(value: str) -> str | None:
    candidate = value.strip()
    if candidate.startswith(("/", "file:")) or re.match(
        r"^[A-Za-z]:[/\\]", candidate
    ):
        return None
    if "://" in candidate:
        try:
            scheme = urlsplit(candidate).scheme.lower()
        except ValueError:
            return None
        if scheme not in _REMOTE_SCHEMES:
            return None
    return normalize_repo_identity(candidate)


def _repository_for_row(
    row: dict[str, Any],
    remote_cache: dict[str, str | None],
) -> tuple[str | None, str | None, bool]:
    candidates: list[tuple[str, str]] = []
    explicit_repo = _nonempty_string(row.get("repo"))
    if explicit_repo is not None:
        normalized = _normalize_explicit_repo(explicit_repo)
        if normalized is not None:
            candidates.append((normalized, "explicit"))

    for field in ("worker_dir", "work_dir"):
        work_dir = _nonempty_string(row.get(field))
        if work_dir is None:
            continue
        normalized = _repository_from_work_dir(work_dir, remote_cache)
        if normalized is not None:
            candidates.append((normalized, field))

    repos = {repo for repo, _source in candidates}
    if len(repos) > 1:
        return None, "ambiguous", True
    if not repos:
        return None, None, False
    repo = next(iter(repos))
    for preferred in ("explicit", "worker_dir", "work_dir"):
        if any(
            candidate_repo == repo and source == preferred
            for candidate_repo, source in candidates
        ):
            return repo, preferred, False
    return repo, None, False


def _repository_from_work_dir(work_dir: str, remote_cache: dict[str, str | None]) -> str | None:
    expanded = os.path.expanduser(work_dir)
    if not os.path.isabs(expanded):
        return None
    cache_key = os.path.realpath(expanded)
    if cache_key in remote_cache:
        return remote_cache[cache_key]
    try:
        completed = subprocess.run(
            ["git", "-C", expanded, "config", "--local", "--get", "remote.origin.url"],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            timeout=_GIT_CONFIG_TIMEOUT_SECONDS,
        )
    except (OSError, subprocess.SubprocessError):
        remote_cache[cache_key] = None
        return None
    repo = _normalize_git_remote(completed.stdout.strip()) if completed.returncode == 0 else None
    remote_cache[cache_key] = repo
    return repo


def _normalize_git_remote(value: str) -> str | None:
    candidate = value.strip()
    if (
        not candidate
        or candidate.startswith(("/", "file:"))
        or re.match(r"^[A-Za-z]:[/\\]", candidate)
    ):
        return None
    if "://" in candidate:
        try:
            scheme = urlsplit(candidate).scheme.lower()
        except ValueError:
            return None
        if scheme not in _REMOTE_SCHEMES:
            return None
    elif ":" not in candidate:
        # Reject relative local paths and ambiguous owner/repo strings from Git
        # config; only explicit repo fields may use owner/repo without a URL.
        return None
    return normalize_repo_identity(candidate)


def _nonempty_string(value: Any) -> str | None:
    if not isinstance(value, str):
        return None
    value = value.strip()
    return value or None


__all__ = ["GCEnrichmentRun", "enrich_gc_sessions"]
