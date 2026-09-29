"""Explicit, transactional SQLite schema migrations.

Normal store opens never upgrade an existing database implicitly. Operators use
these migrations explicitly so the original file can be backed up before any
schema or version metadata is changed.
"""

from __future__ import annotations

import os
import sqlite3
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .canonical import event_snapshot_hash, session_snapshot_hash, session_text_snapshot_hash
from .errors import ObservatoryError, SchemaVersionError
from .roles import role_from_session_name

SCHEMA_VERSION_BEFORE = 4
SCHEMA_VERSION_V5 = 5
SCHEMA_VERSION_AFTER = 6

# Schema 5 adds this append-only recommendation projection to schema 4.
# Fresh schema-6 stores also reuse these create statements.
V4_TO_V5_SCHEMA_STATEMENTS = (
    """
    CREATE TABLE IF NOT EXISTS recommendations (
        recommendation_id TEXT PRIMARY KEY,
        episode_id TEXT NOT NULL,
        work_item_id TEXT,
        kind TEXT NOT NULL,
        decision TEXT NOT NULL,
        eligibility TEXT NOT NULL,
        eligibility_reason TEXT NOT NULL,
        confidence REAL,
        uncertainty REAL,
        recommended_candidate TEXT,
        current_candidate TEXT,
        fallback_candidate TEXT,
        fallback_path TEXT,
        disagreement INTEGER NOT NULL,
        temporal_leak_free INTEGER NOT NULL,
        as_of TEXT NOT NULL,
        catalog_version TEXT NOT NULL,
        evaluator_version TEXT NOT NULL,
        payload_json TEXT NOT NULL,
        created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
    )
    """,
    """
    CREATE INDEX IF NOT EXISTS recommendations_episode_kind
        ON recommendations(episode_id, kind)
    """,
)


# Schema 6 adds durable session/role projections and token-price accounting.
# Keep the shared create statements usable by both fresh stores and the explicit
# v5->v6 migration; the ALTER statements are migration-only.
V6_CORE_SCHEMA_STATEMENTS = (
    """
    CREATE TABLE IF NOT EXISTS classification_sessions (
        classification_id INTEGER PRIMARY KEY,
        city_id TEXT NOT NULL,
        host_id TEXT NOT NULL,
        provider TEXT NOT NULL,
        session_id TEXT NOT NULL,
        binding_method TEXT NOT NULL CHECK (
            binding_method IN ('request', 'snapshot', 'text_snapshot', 'event_snapshot', 'source_provenance', 'explicit')
        ),
        bound_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
        FOREIGN KEY (classification_id) REFERENCES classifications(classification_id) ON DELETE CASCADE,
        FOREIGN KEY (city_id, host_id, provider, session_id)
            REFERENCES sessions(city_id, host_id, provider, session_id) ON DELETE RESTRICT
    )
    """,
    """
    CREATE INDEX IF NOT EXISTS classification_sessions_session
        ON classification_sessions(city_id, host_id, provider, session_id, classification_id)
    """,
    """
    CREATE TABLE IF NOT EXISTS model_pricing (
        model_id TEXT NOT NULL,
        provider TEXT CHECK (provider IS NULL OR length(provider) > 0),
        input_usd_per_million TEXT NOT NULL CHECK (length(input_usd_per_million) > 0),
        output_usd_per_million TEXT NOT NULL CHECK (length(output_usd_per_million) > 0),
        cache_read_usd_per_million TEXT CHECK (
            cache_read_usd_per_million IS NULL OR length(cache_read_usd_per_million) > 0
        ),
        cache_write_usd_per_million TEXT CHECK (
            cache_write_usd_per_million IS NULL OR length(cache_write_usd_per_million) > 0
        ),
        effective_from TEXT NOT NULL,
        source TEXT NOT NULL,
        PRIMARY KEY (model_id, provider, effective_from)
    )
    """,
    """
    CREATE UNIQUE INDEX IF NOT EXISTS model_pricing_provider_key
        ON model_pricing(model_id, COALESCE(provider, ''), effective_from)
    """,
    """
    CREATE VIEW IF NOT EXISTS event_usage_cost AS
    SELECT
        e.city_id,
        e.host_id,
        e.provider AS event_provider,
        e.session_id,
        e.event_id,
        e.timestamp AS event_timestamp,
        e.model AS model_id,
        p.provider AS model_provider,
        u.input_tokens,
        u.output_tokens,
        u.cache_read_tokens,
        u.cache_write_tokens,
        u.total_tokens,
        p.effective_from AS pricing_effective_from,
        p.source AS pricing_source,
        CASE
            WHEN p.model_id IS NULL
              OR (u.input_tokens IS NULL AND u.output_tokens IS NULL
                  AND u.cache_read_tokens IS NULL AND u.cache_write_tokens IS NULL)
              OR (u.cache_read_tokens > 0 AND p.cache_read_usd_per_million IS NULL)
              OR (u.cache_write_tokens > 0 AND p.cache_write_usd_per_million IS NULL)
            THEN 0 ELSE 1
        END AS cost_known,
        CASE
            WHEN p.model_id IS NULL
              OR (u.input_tokens IS NULL AND u.output_tokens IS NULL
                  AND u.cache_read_tokens IS NULL AND u.cache_write_tokens IS NULL)
              OR (u.cache_read_tokens > 0 AND p.cache_read_usd_per_million IS NULL)
              OR (u.cache_write_tokens > 0 AND p.cache_write_usd_per_million IS NULL)
            THEN NULL
            ELSE decimal_event_cost_usd(
                u.input_tokens,
                u.output_tokens,
                u.cache_read_tokens,
                u.cache_write_tokens,
                p.input_usd_per_million,
                p.output_usd_per_million,
                p.cache_read_usd_per_million,
                p.cache_write_usd_per_million
            )
        END AS cost_usd
    FROM event_usage AS u
    JOIN events AS e
      ON e.city_id = u.city_id AND e.host_id = u.host_id AND e.provider = u.provider
     AND e.session_id = u.session_id AND e.event_id = u.event_id
    LEFT JOIN model_pricing AS p
      ON p.model_id = e.model AND p.provider IS e.provider
     AND p.effective_from = (
         SELECT MAX(p2.effective_from)
           FROM model_pricing AS p2
          WHERE p2.model_id = e.model AND p2.provider IS e.provider
            AND p2.effective_from <= e.timestamp
     )
    """,
)

V5_TO_V6_SCHEMA_STATEMENTS = (
    "ALTER TABLE sessions ADD COLUMN role TEXT",
    "ALTER TABLE jev_requests ADD COLUMN city_id TEXT",
    "ALTER TABLE jev_requests ADD COLUMN host_id TEXT",
    "ALTER TABLE jev_requests ADD COLUMN provider TEXT",
    "ALTER TABLE jev_requests ADD COLUMN session_id TEXT",
    *V6_CORE_SCHEMA_STATEMENTS,
)


_V4_REQUIRED_TABLES = frozenset(
    {
        "schema_meta",
        "imported_files",
        "sessions",
        "events",
        "event_usage",
        "jev_requests",
        "classifications",
        "classification_answers",
        "gold_annotations",
        "changes",
        "change_activations",
        "commit_parents",
        "session_fingerprints",
        "exposures",
    }
)
_V5_REQUIRED_TABLES = frozenset((*_V4_REQUIRED_TABLES, "recommendations"))
_PRESERVED_TABLES = tuple(sorted(_V4_REQUIRED_TABLES))
_V5_PRESERVED_TABLES = tuple(sorted(_V5_REQUIRED_TABLES))
_BACKUP_RETRIES = 100
_SQLITE_TIMEOUT_SECONDS = 30.0


@dataclass(frozen=True)
class MigrationResult:
    """Details of an explicit migration or a no-op at the current schema."""

    database_path: str
    backup_path: str | None
    from_version: int
    to_version: int
    preserved_rows: dict[str, int]
    already_at_version: int | None = None
    backup_paths: tuple[str, ...] = ()
    backfill_summary: dict[str, Any] = field(default_factory=dict)


def _validate_v4(connection: sqlite3.Connection, database_path: str) -> None:
    version = int(connection.execute("PRAGMA user_version").fetchone()[0])
    if version != SCHEMA_VERSION_BEFORE:
        raise SchemaVersionError(
            f"database {database_path!r} has schema version {version}; "
            f"migrate supports only schema version {SCHEMA_VERSION_BEFORE}"
        )

    tables = {
        row[0]
        for row in connection.execute("SELECT name FROM sqlite_master WHERE type = 'table'")
    }
    missing_tables = sorted(_V4_REQUIRED_TABLES - tables)
    if missing_tables:
        raise SchemaVersionError(
            f"database {database_path!r} claims schema version 4 but is missing tables: "
            + ", ".join(missing_tables)
        )
    if "recommendations" in tables:
        raise SchemaVersionError(
            f"database {database_path!r} claims schema version 4 but already has the "
            "schema-5 recommendations table"
        )
    indexes = {
        row[0]
        for row in connection.execute("SELECT name FROM sqlite_master WHERE type = 'index'")
    }
    if "recommendations_episode_kind" in indexes:
        raise SchemaVersionError(
            f"database {database_path!r} claims schema version 4 but already has the "
            "schema-5 recommendations index"
        )

    metadata = connection.execute(
        "SELECT value FROM schema_meta WHERE key = 'schema_version'"
    ).fetchone()
    if metadata is None or metadata[0] != str(SCHEMA_VERSION_BEFORE):
        actual = None if metadata is None else metadata[0]
        raise SchemaVersionError(
            f"database {database_path!r} has schema_meta version {actual!r}; "
            f"expected {SCHEMA_VERSION_BEFORE} to match PRAGMA user_version"
        )


def _table_names(connection: sqlite3.Connection) -> set[str]:
    return {
        row[0]
        for row in connection.execute("SELECT name FROM sqlite_master WHERE type = 'table'")
    }


def _column_names(connection: sqlite3.Connection, table: str) -> set[str]:
    return {row[1] for row in connection.execute(f'PRAGMA table_info("{table}")')}


def _validate_v5(connection: sqlite3.Connection, database_path: str) -> None:
    tables = _table_names(connection)
    missing_tables = sorted(_V5_REQUIRED_TABLES - tables)
    if missing_tables:
        raise ObservatoryError(
            f"database {database_path!r} is missing required schema-5 tables: "
            + ", ".join(missing_tables)
        )

    version = int(connection.execute("PRAGMA user_version").fetchone()[0])
    metadata = connection.execute(
        "SELECT value FROM schema_meta WHERE key = 'schema_version'"
    ).fetchone()
    if version != SCHEMA_VERSION_V5 or metadata is None or metadata[0] != str(SCHEMA_VERSION_V5):
        raise ObservatoryError(
            f"database {database_path!r} does not have both schema version markers set to "
            f"{SCHEMA_VERSION_V5}"
        )
    index = connection.execute(
        "SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = 'recommendations_episode_kind'"
    ).fetchone()
    if index is None:
        raise ObservatoryError(
            f"database {database_path!r} is missing the schema-5 recommendations index"
        )
    views = {
        row[0] for row in connection.execute("SELECT name FROM sqlite_master WHERE type = 'view'")
    }
    if (
        {"classification_sessions", "model_pricing"} & tables
        or "event_usage_cost" in views
        or "role" in _column_names(connection, "sessions")
        or {"city_id", "host_id", "provider", "session_id"} & _column_names(connection, "jev_requests")
    ):
        raise ObservatoryError(
            f"database {database_path!r} claims schema 5 but already contains schema-6 objects"
        )


def _validate_v6(connection: sqlite3.Connection, database_path: str) -> None:
    tables = _table_names(connection)
    missing_tables = sorted((_V5_REQUIRED_TABLES | {"classification_sessions", "model_pricing"}) - tables)
    if missing_tables:
        raise ObservatoryError(
            f"database {database_path!r} is missing required schema-6 tables: "
            + ", ".join(missing_tables)
        )
    version = int(connection.execute("PRAGMA user_version").fetchone()[0])
    metadata = connection.execute(
        "SELECT value FROM schema_meta WHERE key = 'schema_version'"
    ).fetchone()
    if version != SCHEMA_VERSION_AFTER or metadata is None or metadata[0] != str(SCHEMA_VERSION_AFTER):
        raise ObservatoryError(
            f"database {database_path!r} does not have both schema version markers set to "
            f"{SCHEMA_VERSION_AFTER}"
        )
    indexes = {
        row[0] for row in connection.execute("SELECT name FROM sqlite_master WHERE type = 'index'")
    }
    views = {
        row[0] for row in connection.execute("SELECT name FROM sqlite_master WHERE type = 'view'")
    }
    if (
        "recommendations_episode_kind" not in indexes
        or "classification_sessions_session" not in indexes
        or "model_pricing_provider_key" not in indexes
        or "event_usage_cost" not in views
    ):
        raise ObservatoryError(
            f"database {database_path!r} is missing a schema-5/6 index or cost view"
        )
    if not {
        "classification_id", "city_id", "host_id", "provider", "session_id", "binding_method"
    } <= _column_names(connection, "classification_sessions"):
        raise ObservatoryError(f"database {database_path!r} has an incomplete classification binding table")
    if not {
        "model_id", "provider", "input_usd_per_million", "output_usd_per_million",
        "cache_read_usd_per_million", "cache_write_usd_per_million", "effective_from", "source",
    } <= _column_names(connection, "model_pricing"):
        raise ObservatoryError(f"database {database_path!r} has an incomplete model pricing table")
    pricing_info = {
        row[1]: row for row in connection.execute("PRAGMA table_info(model_pricing)")
    }
    if any(
        pricing_info[field][2].upper() != "TEXT"
        for field in (
            "input_usd_per_million",
            "output_usd_per_million",
            "cache_read_usd_per_million",
            "cache_write_usd_per_million",
        )
    ):
        raise ObservatoryError(
            f"database {database_path!r} does not store exact decimal model prices"
        )
    if pricing_info["provider"][3] != 0 or {
        field: pricing_info[field][5]
        for field in ("model_id", "provider", "effective_from")
    } != {"model_id": 1, "provider": 2, "effective_from": 3}:
        raise ObservatoryError(
            f"database {database_path!r} has an invalid provider-scoped pricing key"
        )
    if "role" not in _column_names(connection, "sessions"):
        raise ObservatoryError(f"database {database_path!r} is missing sessions.role")
    if not {"city_id", "host_id", "provider", "session_id"} <= _column_names(connection, "jev_requests"):
        raise ObservatoryError(f"database {database_path!r} is missing request session identity columns")


def _row_counts(
    connection: sqlite3.Connection, tables: tuple[str, ...] = _PRESERVED_TABLES
) -> dict[str, int]:
    return {
        table: int(connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0])
        for table in tables
    }


def _reserve_backup_path(database_path: Path, version: int) -> Path:
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    prefix = f"{database_path.name}.v{version}-pre-schema{version + 1}-{timestamp}"
    for attempt in range(_BACKUP_RETRIES):
        suffix = "" if attempt == 0 else f"-{attempt}"
        candidate = database_path.with_name(f"{prefix}{suffix}.bak")
        try:
            descriptor = os.open(candidate, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            continue
        os.close(descriptor)
        return candidate
    raise FileExistsError(
        f"could not reserve a unique schema-{version} backup beside {database_path}"
    )


def _validate_version(connection: sqlite3.Connection, database_path: str, version: int) -> None:
    if version == SCHEMA_VERSION_BEFORE:
        _validate_v4(connection, database_path)
    elif version == SCHEMA_VERSION_V5:
        _validate_v5(connection, database_path)
    else:
        raise SchemaVersionError(f"no backup validator for schema version {version}")


def _create_backup(database_path: Path, version: int) -> tuple[Path, dict[str, int]]:
    """Back up the locked, unchanged database and verify its schema markers."""
    backup_path = _reserve_backup_path(database_path, version)
    preserved_tables = _PRESERVED_TABLES if version == SCHEMA_VERSION_BEFORE else _V5_PRESERVED_TABLES
    source: sqlite3.Connection | None = None
    destination: sqlite3.Connection | None = None
    try:
        # The caller holds BEGIN IMMEDIATE on a separate connection. That
        # prevents another writer from changing the source while this read-only
        # connection takes a consistent SQLite online-backup snapshot.
        source = sqlite3.connect(
            str(database_path), timeout=_SQLITE_TIMEOUT_SECONDS, isolation_level=None
        )
        source.execute("PRAGMA query_only = ON")
        destination = sqlite3.connect(
            str(backup_path), timeout=_SQLITE_TIMEOUT_SECONDS, isolation_level=None
        )
        source.backup(destination)
        destination.commit()
        destination.close()
        destination = None
        source.close()
        source = None

        backup = sqlite3.connect(str(backup_path), timeout=_SQLITE_TIMEOUT_SECONDS)
        try:
            _validate_version(backup, str(backup_path), version)
            counts = _row_counts(backup, preserved_tables)
        finally:
            backup.close()
        return backup_path, counts
    except BaseException:
        if destination is not None:
            destination.close()
        if source is not None:
            source.close()
        try:
            backup_path.unlink()
        except FileNotFoundError:
            pass
        for suffix in ("-journal", "-wal", "-shm"):
            try:
                Path(f"{backup_path}{suffix}").unlink()
            except FileNotFoundError:
                pass
        raise


def _migrate_v4_to_v5(database_path: Path) -> MigrationResult:
    connection = sqlite3.connect(
        str(database_path), timeout=_SQLITE_TIMEOUT_SECONDS, isolation_level=None
    )
    backup_path: Path | None = None
    try:
        _validate_v4(connection, str(database_path))
        connection.execute("BEGIN IMMEDIATE")
        try:
            _validate_v4(connection, str(database_path))
            backup_path, backup_counts = _create_backup(database_path, SCHEMA_VERSION_BEFORE)
            before_counts = _row_counts(connection, _PRESERVED_TABLES)
            if before_counts != backup_counts:
                raise ObservatoryError(
                    "schema-4 backup row counts do not match the locked database; refusing to migrate"
                )
            for statement in V4_TO_V5_SCHEMA_STATEMENTS:
                connection.execute(statement)
            updated = connection.execute(
                "UPDATE schema_meta SET value = ? WHERE key = 'schema_version' AND value = ?",
                (str(SCHEMA_VERSION_V5), str(SCHEMA_VERSION_BEFORE)),
            )
            if updated.rowcount != 1:
                raise SchemaVersionError(
                    f"database {str(database_path)!r} schema_meta changed during migration"
                )
            connection.execute(f"PRAGMA user_version = {SCHEMA_VERSION_V5}")
            _validate_v5(connection, str(database_path))
            after_counts = _row_counts(connection, _PRESERVED_TABLES)
            if after_counts != before_counts:
                raise ObservatoryError(
                    "schema-4 migration changed a preserved table row count; rolling back"
                )
            connection.execute("COMMIT")
        except BaseException:
            if connection.in_transaction:
                connection.execute("ROLLBACK")
            raise
        return MigrationResult(
            database_path=str(database_path),
            backup_path=str(backup_path),
            from_version=SCHEMA_VERSION_BEFORE,
            to_version=SCHEMA_VERSION_V5,
            preserved_rows=after_counts,
            backup_paths=(str(backup_path),),
        )
    finally:
        connection.close()


def _session_indexes(
    connection: sqlite3.Connection,
) -> tuple[
    dict[str, set[tuple[str, str, str, str]]],
    dict[tuple[str, str], set[tuple[str, str, str, str]]],
    dict[str, str],
]:
    """Index existing snapshot hashes and exact event-source provenance."""
    snapshot_sessions: dict[str, set[tuple[str, str, str, str]]] = {}
    source_sessions: dict[tuple[str, str], set[tuple[str, str, str, str]]] = {}
    snapshot_methods: dict[str, str] = {}
    sessions = connection.execute(
        "SELECT city_id, host_id, provider, session_id FROM sessions "
        "ORDER BY city_id, host_id, provider, session_id"
    ).fetchall()
    for city_id, host_id, provider, session_id in sessions:
        key = (city_id, host_id, provider, session_id)
        events = connection.execute(
            "SELECT event_id, payload_hash, source_path, source_sha256 FROM events "
            "WHERE city_id = ? AND host_id = ? AND provider = ? AND session_id = ? "
            "ORDER BY timestamp, event_id",
            key,
        ).fetchall()
        if not events:
            continue
        raw_hash = session_snapshot_hash(key, [(row[0], row[1]) for row in events])
        text_hash = session_text_snapshot_hash(raw_hash)
        snapshot_sessions.setdefault(raw_hash, set()).add(key)
        snapshot_methods[raw_hash] = "snapshot"
        snapshot_sessions.setdefault(text_hash, set()).add(key)
        snapshot_methods[text_hash] = "text_snapshot"
        for event_id, payload_hash_value, source_path, source_sha256 in events:
            event_hash = event_snapshot_hash(key + (event_id,), payload_hash_value)
            snapshot_sessions.setdefault(event_hash, set()).add(key)
            snapshot_methods[event_hash] = "event_snapshot"
            if source_path and source_sha256:
                source_sessions.setdefault((source_path, source_sha256), set()).add(key)
    return snapshot_sessions, source_sessions, snapshot_methods


def _backfill_session_roles(connection: sqlite3.Connection) -> tuple[int, int]:
    rows = connection.execute(
        "SELECT city_id, host_id, provider, session_id FROM sessions"
    ).fetchall()
    with_role = 0
    for city_id, host_id, provider, session_id in rows:
        role = role_from_session_name(session_id)
        if role is None:
            continue
        connection.execute(
            "UPDATE sessions SET role = ? WHERE city_id = ? AND host_id = ? "
            "AND provider = ? AND session_id = ?",
            (role, city_id, host_id, provider, session_id),
        )
        with_role += 1
    return len(rows), with_role




def _backfill_request_sessions(
    connection: sqlite3.Connection,
    snapshot_sessions: dict[str, set[tuple[str, str, str, str]]],
    source_sessions: dict[tuple[str, str], set[tuple[str, str, str, str]]],
    snapshot_methods: dict[str, str],
) -> dict[str, Any]:
    """Carry exact subject-to-session provenance onto requests for late responses."""
    rows = connection.execute(
        "SELECT request_hash, subject_kind, snapshot_hash, source_path, source_sha256 "
        "FROM jev_requests ORDER BY request_hash"
    ).fetchall()
    methods = {"snapshot": 0, "text_snapshot": 0, "event_snapshot": 0, "source_provenance": 0}
    bound = 0
    for request_hash, subject_kind, snapshot_hash, source_path, source_sha256 in rows:
        match = snapshot_sessions.get(snapshot_hash, set())
        method = snapshot_methods.get(snapshot_hash)
        if len(match) == 1 and (
            (subject_kind == "session" and method in {"snapshot", "text_snapshot"})
            or (subject_kind == "event" and method == "event_snapshot")
        ):
            key = next(iter(match))
        else:
            candidates = (
                source_sessions.get((source_path, source_sha256), set())
                if source_path and source_sha256
                else set()
            )
            if len(candidates) != 1:
                continue
            key = next(iter(candidates))
            method = "source_provenance"
        connection.execute(
            "UPDATE jev_requests SET city_id = ?, host_id = ?, provider = ?, session_id = ? "
            "WHERE request_hash = ? AND city_id IS NULL AND host_id IS NULL "
            "AND provider IS NULL AND session_id IS NULL",
            (*key, request_hash),
        )
        bound += 1
        methods[method] += 1
    total = len(rows)
    return {
        "requests_total": total,
        "requests_bound": bound,
        "requests_unbound": total - bound,
        "request_match_rate": round(bound / total, 6) if total else None,
        "request_binding_methods": methods,
    }

def _backfill_classification_sessions(
    connection: sqlite3.Connection,
    snapshot_sessions: dict[str, set[tuple[str, str, str, str]]],
    source_sessions: dict[tuple[str, str], set[tuple[str, str, str, str]]],
    snapshot_methods: dict[str, str],
) -> dict[str, Any]:
    rows = connection.execute(
        "SELECT classification_id, subject_kind, snapshot_hash, request_hash, "
        "source_path, source_sha256 FROM classifications ORDER BY classification_id"
    ).fetchall()
    counts = {
        "snapshot": 0,
        "text_snapshot": 0,
        "event_snapshot": 0,
        "request": 0,
        "source_provenance": 0,
    }
    bound = 0
    for classification_id, subject_kind, snapshot_hash, request_hash, source_path, source_sha256 in rows:
        match = snapshot_sessions.get(snapshot_hash, set())
        key: tuple[str, str, str, str] | None = None
        method: str | None = None
        snapshot_method = snapshot_methods.get(snapshot_hash)
        if len(match) == 1 and (
            (subject_kind == "session" and snapshot_method in {"snapshot", "text_snapshot"})
            or (subject_kind == "event" and snapshot_method == "event_snapshot")
        ):
            key = next(iter(match))
            method = snapshot_method
        else:
            provenance_matches: set[tuple[str, str, str, str]] = set()
            source_pairs = []
            if source_path and source_sha256:
                source_pairs.append((source_path, source_sha256))
            request = connection.execute(
                "SELECT source_path, source_sha256, city_id, host_id, provider, session_id "
                "FROM jev_requests WHERE request_hash = ?",
                (request_hash,),
            ).fetchone()
            request_key = None
            if request is not None:
                if request[0] and request[1]:
                    source_pairs.append((request[0], request[1]))
                if all(request[index] for index in (2, 3, 4, 5)):
                    request_key = tuple(request[index] for index in (2, 3, 4, 5))
                    provenance_matches.add(request_key)
            for pair in set(source_pairs):
                provenance_matches.update(source_sessions.get(pair, set()))
            if len(provenance_matches) == 1:
                key = next(iter(provenance_matches))
                method = "request" if request_key == key else "source_provenance"

        if key is None or method is None:
            continue
        connection.execute(
            "INSERT INTO classification_sessions(classification_id, city_id, host_id, provider, "
            "session_id, binding_method) VALUES (?, ?, ?, ?, ?, ?)",
            (classification_id, *key, method),
        )
        bound += 1
        counts[method] += 1

    total = len(rows)
    return {
        "classifications_total": total,
        "classifications_bound": bound,
        "classifications_unbound": total - bound,
        "classification_match_rate": round(bound / total, 6) if total else None,
        "classification_binding_methods": counts,
    }


def _migrate_v5_to_v6(database_path: Path) -> MigrationResult:
    connection = sqlite3.connect(
        str(database_path), timeout=_SQLITE_TIMEOUT_SECONDS, isolation_level=None
    )
    backup_path: Path | None = None
    try:
        _validate_v5(connection, str(database_path))
        connection.execute("BEGIN IMMEDIATE")
        try:
            _validate_v5(connection, str(database_path))
            backup_path, backup_counts = _create_backup(database_path, SCHEMA_VERSION_V5)
            before_counts = _row_counts(connection, _V5_PRESERVED_TABLES)
            if before_counts != backup_counts:
                raise ObservatoryError(
                    "schema-5 backup row counts do not match the locked database; refusing to migrate"
                )
            for statement in V5_TO_V6_SCHEMA_STATEMENTS:
                connection.execute(statement)

            # The versioned seed is validated before rows are written, so schema
            # upgrades produce a usable price projection without touching a live
            # caller's event database outside this explicit migration.
            from .pricing import _decimal_text, load_pricing_seed

            pricing_rows = load_pricing_seed()
            connection.executemany(
                "INSERT INTO model_pricing(model_id, provider, input_usd_per_million, "
                "output_usd_per_million, cache_read_usd_per_million, "
                "cache_write_usd_per_million, effective_from, source) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                [
                    (
                        row["model_id"],
                        row["provider"],
                        _decimal_text(row["input_usd_per_million"]),
                        _decimal_text(row["output_usd_per_million"]),
                        _decimal_text(row["cache_read_usd_per_million"]),
                        _decimal_text(row["cache_write_usd_per_million"]),
                        row["effective_from"],
                        row["source"],
                    )
                    for row in pricing_rows
                ],
            )

            sessions_total, sessions_with_role = _backfill_session_roles(connection)
            snapshot_sessions, source_sessions, snapshot_methods = _session_indexes(connection)
            request_summary = _backfill_request_sessions(
                connection, snapshot_sessions, source_sessions, snapshot_methods
            )
            binding_summary = _backfill_classification_sessions(
                connection, snapshot_sessions, source_sessions, snapshot_methods
            )
            binding_summary.update(request_summary)
            binding_summary.update(
                {
                    "sessions_total": sessions_total,
                    "sessions_with_role": sessions_with_role,
                    "session_role_coverage": (
                        round(sessions_with_role / sessions_total, 6) if sessions_total else None
                    ),
                }
            )
            binding_summary["pricing_seed_rows"] = len(pricing_rows)

            updated = connection.execute(
                "UPDATE schema_meta SET value = ? WHERE key = 'schema_version' AND value = ?",
                (str(SCHEMA_VERSION_AFTER), str(SCHEMA_VERSION_V5)),
            )
            if updated.rowcount != 1:
                raise SchemaVersionError(
                    f"database {str(database_path)!r} schema_meta changed during migration"
                )
            connection.execute(f"PRAGMA user_version = {SCHEMA_VERSION_AFTER}")
            _validate_v6(connection, str(database_path))

            after_counts = _row_counts(connection, _V5_PRESERVED_TABLES)
            if after_counts != before_counts:
                raise ObservatoryError(
                    "schema-5 migration changed a preserved table row count; rolling back"
                )
            connection.execute("COMMIT")
        except BaseException:
            if connection.in_transaction:
                connection.execute("ROLLBACK")
            raise
        return MigrationResult(
            database_path=str(database_path),
            backup_path=str(backup_path),
            from_version=SCHEMA_VERSION_V5,
            to_version=SCHEMA_VERSION_AFTER,
            preserved_rows=after_counts,
            backup_paths=(str(backup_path),),
            backfill_summary=binding_summary,
        )
    finally:
        connection.close()


def migrate_database(db_path: str | os.PathLike[str]) -> MigrationResult:
    """Back up and upgrade an existing schema-4 or schema-5 projection to v6.

    Each step writes and validates a timestamped online backup before modifying
    the schema. Schema 4 is first advanced through the existing recommendation
    migration, then through the additive schema-6 migration. A schema-5 failure
    rolls back atomically while retaining its verified backup.
    """
    raw_path = os.fspath(db_path)
    if raw_path == ":memory:":
        raise ObservatoryError("migrate requires an existing on-disk SQLite database")
    database_path = Path(raw_path).expanduser().resolve(strict=True)
    if not database_path.is_file():
        raise ObservatoryError(f"database {str(database_path)!r} is not a regular file")

    connection = sqlite3.connect(
        str(database_path), timeout=_SQLITE_TIMEOUT_SECONDS, isolation_level=None
    )
    try:
        version = int(connection.execute("PRAGMA user_version").fetchone()[0])
        if version == SCHEMA_VERSION_AFTER:
            _validate_v6(connection, str(database_path))
            return MigrationResult(
                database_path=str(database_path),
                backup_path=None,
                from_version=SCHEMA_VERSION_AFTER,
                to_version=SCHEMA_VERSION_AFTER,
                preserved_rows={},
                already_at_version=SCHEMA_VERSION_AFTER,
            )
    finally:
        connection.close()

    if version == SCHEMA_VERSION_BEFORE:
        v4_result = _migrate_v4_to_v5(database_path)
        v5_result = _migrate_v5_to_v6(database_path)
        return MigrationResult(
            database_path=str(database_path),
            backup_path=v5_result.backup_path,
            from_version=SCHEMA_VERSION_BEFORE,
            to_version=SCHEMA_VERSION_AFTER,
            preserved_rows=v5_result.preserved_rows,
            backup_paths=v4_result.backup_paths + v5_result.backup_paths,
            backfill_summary=v5_result.backfill_summary,
        )
    if version == SCHEMA_VERSION_V5:
        return _migrate_v5_to_v6(database_path)
    raise SchemaVersionError(
        f"database {str(database_path)!r} has schema version {version}; "
        "migrate supports only valid schema versions 4, 5, or 6"
    )
