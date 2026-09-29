"""Explicit, transactional SQLite schema migrations.

Normal store opens never upgrade an existing database implicitly. Operators use
these migrations explicitly so the original file can be backed up before any
schema or version metadata is changed.
"""

from __future__ import annotations

import os
import sqlite3
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from .errors import ObservatoryError, SchemaVersionError

SCHEMA_VERSION_BEFORE = 4
SCHEMA_VERSION_AFTER = 5

# Schema 5 adds only this append-only recommendation projection to schema 4.
# The store uses these same statements when it creates a new schema-5 database.
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
_PRESERVED_TABLES = tuple(sorted(_V4_REQUIRED_TABLES))
_BACKUP_RETRIES = 100
_SQLITE_TIMEOUT_SECONDS = 30.0


@dataclass(frozen=True)
class MigrationResult:
    """Details of a v4-to-v5 upgrade or a no-op for an already-v5 database."""

    database_path: str
    backup_path: str | None
    from_version: int
    to_version: int
    preserved_rows: dict[str, int]
    already_at_version: int | None = None


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


def _validate_v5(connection: sqlite3.Connection, database_path: str) -> None:
    tables = {
        row[0]
        for row in connection.execute("SELECT name FROM sqlite_master WHERE type = 'table'")
    }
    missing_tables = sorted(_V4_REQUIRED_TABLES - tables)
    if missing_tables:
        raise ObservatoryError(
            f"database {database_path!r} is missing required schema tables: "
            + ", ".join(missing_tables)
        )

    version = int(connection.execute("PRAGMA user_version").fetchone()[0])
    metadata = connection.execute(
        "SELECT value FROM schema_meta WHERE key = 'schema_version'"
    ).fetchone()
    if (
        version != SCHEMA_VERSION_AFTER
        or metadata is None
        or metadata[0] != str(SCHEMA_VERSION_AFTER)
    ):
        raise ObservatoryError(
            f"database {database_path!r} does not have both schema version markers set to "
            f"{SCHEMA_VERSION_AFTER}"
        )
    table = connection.execute(
        "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'recommendations'"
    ).fetchone()
    index = connection.execute(
        "SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = 'recommendations_episode_kind'"
    ).fetchone()
    if table is None or index is None:
        raise ObservatoryError(
            f"database {database_path!r} is missing the "
            "schema-5 recommendations table or index"
        )


def _row_counts(connection: sqlite3.Connection) -> dict[str, int]:
    return {
        table: int(connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0])
        for table in _PRESERVED_TABLES
    }


def _reserve_backup_path(database_path: Path) -> Path:
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    prefix = f"{database_path.name}.v4-pre-schema5-{timestamp}"
    for attempt in range(_BACKUP_RETRIES):
        suffix = "" if attempt == 0 else f"-{attempt}"
        candidate = database_path.with_name(f"{prefix}{suffix}.bak")
        try:
            descriptor = os.open(candidate, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            continue
        os.close(descriptor)
        return candidate
    raise FileExistsError(f"could not reserve a unique schema-4 backup beside {database_path}")


def _create_backup(database_path: Path) -> tuple[Path, dict[str, int]]:
    """Back up the locked, unchanged database and verify its v4 markers."""
    backup_path = _reserve_backup_path(database_path)
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
            _validate_v4(backup, str(backup_path))
            counts = _row_counts(backup)
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


def migrate_database(db_path: str | os.PathLike[str]) -> MigrationResult:
    """Back up and transactionally upgrade one existing schema-4 database to v5.

    The backup is written beside the database with a UTC timestamp before the
    migration changes any schema or version metadata. Existing table contents
    are never rewritten; every required v4 table is counted in the backup and
    upgraded database as a preservation check.
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
    backup_path: Path | None = None
    try:
        version = int(connection.execute("PRAGMA user_version").fetchone()[0])
        if version == SCHEMA_VERSION_AFTER:
            _validate_v5(connection, str(database_path))
            return MigrationResult(
                database_path=str(database_path),
                backup_path=None,
                from_version=SCHEMA_VERSION_AFTER,
                to_version=SCHEMA_VERSION_AFTER,
                preserved_rows={},
                already_at_version=SCHEMA_VERSION_AFTER,
            )

        _validate_v4(connection, str(database_path))
        connection.execute("BEGIN IMMEDIATE")
        try:
            # Recheck after taking the writer lock, in case another process
            # changed the database between the initial validation and BEGIN.
            _validate_v4(connection, str(database_path))
            backup_path, backup_counts = _create_backup(database_path)
            before_counts = _row_counts(connection)
            if before_counts != backup_counts:
                raise ObservatoryError(
                    "schema-4 backup row counts do not match the locked database; "
                    "refusing to migrate"
                )

            for statement in V4_TO_V5_SCHEMA_STATEMENTS:
                connection.execute(statement)
            updated = connection.execute(
                "UPDATE schema_meta SET value = ? WHERE key = 'schema_version' AND value = ?",
                (str(SCHEMA_VERSION_AFTER), str(SCHEMA_VERSION_BEFORE)),
            )
            if updated.rowcount != 1:
                raise SchemaVersionError(
                    f"database {str(database_path)!r} schema_meta changed during migration"
                )
            connection.execute(f"PRAGMA user_version = {SCHEMA_VERSION_AFTER}")
            _validate_v5(connection, str(database_path))

            after_counts = _row_counts(connection)
            if after_counts != before_counts:
                raise ObservatoryError(
                    "schema migration changed rows in a required v4 table; "
                    "rolling back"
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
            to_version=SCHEMA_VERSION_AFTER,
            preserved_rows=after_counts,
        )
    finally:
        connection.close()
