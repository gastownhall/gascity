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
            if current is not None and current["template"] is not None and current["template"] != binding.template:
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
                # A persisted-binding conflict is counted once as a conflict;
                # repo_ambiguous counts ambiguity within the incoming evidence.
                if not binding.repo_ambiguous:
                    result.conflicts += 1
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

        _enrich_historical_sessions(store, city_id, host_id, remote_cache, result)
        store.conn.execute("COMMIT")
    except BaseException:
        if store.conn.in_transaction:
            store.conn.execute("ROLLBACK")
        raise
    return result


def _removed_worktree_repository(cwd: str) -> str | None:
    """Only known, component-boundary prefixes; never guess generic fleet roots."""
    path = Path(os.path.expanduser(cwd))
    if not path.is_absolute() or path.exists() or ".." in path.parts:
        return None
    parts = path.parts
    mappings = (
        (("projects", "Gateway-LLM"), "uniblock-dev/gateway-llm"),
        (("src", "gascity"), "hoomji/gascity"),
        (("src", "gascity-worktrees"), "hoomji/gascity"),
        (("src", "city-worktrees"), "city"),
    )
    # Only the repository immediately below /home/<user> is evidence.
    # Nested src/projects components inside a deleted worktree are not roots.
    # This also covers the /home/coolhenry alias without resolving symlinks.
    if len(parts) < 4 or parts[1] != "home":
        return None
    mappings += ((("city",), "city"),)
    candidates = {
        repo for prefix, repo in mappings
        if tuple(parts[3:3 + len(prefix)]) == prefix
    }
    # Fail closed if overlapping anchored mappings ever disagree.
    return next(iter(candidates)) if len(candidates) == 1 else None


def _transcript_repositories(path: str, provider: str, session_id: str,
                             remote_cache: dict[str, str | None]) -> set[tuple[str, str]]:
    candidates: set[tuple[str, str]] = set()
    # Bound reads, ignore malformed/foreign rows, never execute transcript content.
    try:
        with open(path, "r", encoding="utf-8") as source:
            matching_codex_session = False
            while True:
                line = source.readline(4 * 1024 * 1024 + 1)
                if not line:
                    break
                if len(line) > 4 * 1024 * 1024:
                    while line and not line.endswith("\n"):
                        line = source.readline(4 * 1024 * 1024 + 1)
                    continue
                try:
                    row = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(row, dict):
                    continue
                if provider == "codex":
                    if row.get("type") not in ("session_meta", "turn_context"):
                        continue
                    payload = row.get("payload")
                    if not isinstance(payload, dict):
                        continue
                    if row.get("type") == "session_meta":
                        matching_codex_session = payload.get("id") == session_id
                    if not matching_codex_session:
                        continue
                    cwd = _nonempty_string(payload.get("cwd"))
                elif provider == "claude":
                    if row.get("sessionId") != session_id:
                        continue
                    cwd = _nonempty_string(row.get("cwd"))
                else:
                    continue
                if cwd is None:
                    continue
                repo = _repository_from_work_dir(cwd, remote_cache)
                provenance = "transcript_cwd"
                if repo is None:
                    repo = _removed_worktree_repository(cwd)
                    provenance = "transcript_cwd_prefix"
                if repo:
                    candidates.add((repo, provenance))
    except (OSError, UnicodeError):
        return set()
    return candidates


def _enrich_historical_sessions(store: Any, city_id: str, host_id: str,
                                remote_cache: dict[str, str | None], result: GCEnrichmentRun) -> None:
    rows = store.conn.execute(
        "SELECT DISTINCT provider, session_id, source_path FROM events "
        "WHERE city_id = ? AND host_id = ? AND source_path IS NOT NULL",
        (city_id, host_id),
    ).fetchall()
    checkpoints: dict[tuple[str, str], str] = {}
    if store.conn.execute("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'collector_sources'").fetchone():
        checkpoints = {(row["source_id"], row["provider"]): row["path"] for row in
                       store.conn.execute("SELECT source_id, provider, path FROM collector_sources")}
    candidates: dict[tuple[str, str], set[tuple[str, str]]] = {}
    for row in rows:
        key = (row["provider"], row["session_id"])
        # Collector events reference deleted normalized spools. Resolve their
        # stable source-id stems through the collector's native-source checkpoint.
        path = checkpoints.get((Path(row["source_path"]).stem, row["provider"]), row["source_path"])
        candidates.setdefault(key, set()).update(_transcript_repositories(
            path, *key, remote_cache))
    for key, evidence in sorted(candidates.items()):
        repos = {repo for repo, _source in evidence}
        if len(repos) > 1:
            result.repo_ambiguous += 1
            continue
        if not repos:
            continue
        identity = (city_id, host_id, *key)
        current = store.conn.execute(
            "SELECT template, repo FROM session_enrichment WHERE city_id = ? AND host_id = ? "
            "AND provider = ? AND session_id = ?", identity).fetchone()
        if current is not None and current["repo"] is not None:
            continue  # Durable explicit or inferred bindings always win.
        repo = next(iter(repos))
        provenance = "transcript_cwd" if (repo, "transcript_cwd") in evidence else "transcript_cwd_prefix"
        template = current["template"] if current else None
        digest = _binding_sha256(identity, template, repo, provenance)
        store.conn.execute(
            "INSERT INTO session_enrichment(city_id, host_id, provider, session_id, template, "
            "repo, repo_source, source_sha256) VALUES (?, ?, ?, ?, ?, ?, ?, ?) "
            "ON CONFLICT(city_id, host_id, provider, session_id) DO UPDATE SET "
            "repo = excluded.repo, repo_source = excluded.repo_source, source_sha256 = excluded.source_sha256, "
            "updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')",
            (*identity, template, repo, provenance, digest))
        result.bindings_written += 1
        result.repo_bindings_written += 1


def _binding_sha256(
    identity: tuple[str, str, str, str],
    template: str | None,
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
