"""SQLite-backed analytical projection.

The database is a *derived* artifact: beads and events remain the authoritative
record. Everything here is rebuildable from source JSONL, so the schema is
versioned and imports are transactional and idempotent. A partial or malformed
file never commits; a conflicting reuse of an event identity is skipped at the
record level with a note rather than silently overwritten or allowed to abort
the whole file.
"""

from __future__ import annotations

import json
import os
import sqlite3
from dataclasses import dataclass, field
from decimal import Decimal, InvalidOperation, localcontext
from pathlib import Path
from typing import Any, Iterable, Iterator, Mapping, Sequence

from .canonical import (
    canonical_hash,
    canonical_json,
    event_snapshot_hash,
    identity_key,
    session_snapshot_hash,
    session_text_snapshot_hash,
    sha256_bytes,
)
from .contract import (
    USAGE_INT_FIELDS,
    normalize_timestamp,
    payload_hash,
    record_identity,
    validate_record,
)
from .errors import (
    ContractError,
    LabelConflictError,
    ObservatoryError,
    PricingError,
    RegistryConflictError,
    RegistryError,
    SchemaVersionError,
)
from .migrations import (
    SCHEMA_VERSION_AFTER,
    V4_TO_V5_SCHEMA_STATEMENTS,
    V6_CORE_SCHEMA_STATEMENTS,
    V8_CORE_SCHEMA_STATEMENTS,
)

# Bump when the projection schema changes. Normal opens reject older schemas;
# use the explicit CLI migration for a supported in-place upgrade.
#
# Version 3 excludes canonical identity fields from the stored ``payload_hash``
# (contract.payload_hash), so a version-2 projection would carry stale hashes and
# could reject an identical re-import as a conflict. Rebuild instead.
#
# Version 4 adds the M5 optimization registry: ``changes``, ``change_activations``,
# ``commit_parents``, ``session_fingerprints`` and the derived ``exposures`` join.
#
# Version 5 adds the M7 shadow-policy ``recommendations`` projection. Rows are
# append-only and keyed by the recommendation content hash, so replaying an
# identical shadow run deduplicates while a changed policy version is retained.
# Version 6 adds classification-to-session bindings, session roles, and pricing.
# Version 7 adds trusted GC session enrichment as a derived side table and view.
DB_SCHEMA_VERSION = SCHEMA_VERSION_AFTER

# Normalized record fields, in table order. ``observed_timestamp`` is not here:
# it is derived provenance (the raw input string), not part of the payload hash.
_DATA_COLUMNS = (
    "city_id",
    "host_id",
    "provider",
    "session_id",
    "event_id",
    "timestamp",
    "kind",
    "title",
    "text",
    "tool_name",
    "tool_call_id",
    "command",
    "exit_code",
    "duration_ms",
    "model",
    "repo",
    "commit_sha",
    "parent_session_id",
    "bead_id",
    "formula_id",
)

# Derived provenance: payload hash plus the exact source of the observation.
_PROVENANCE_COLUMNS = (
    "payload_hash",
    "source_path",
    "source_sha256",
    "source_line",
    "observed_timestamp",
)

_EVENT_COLUMNS = _DATA_COLUMNS + _PROVENANCE_COLUMNS

_SCHEMA_STATEMENTS = (
    """
    CREATE TABLE IF NOT EXISTS schema_meta (
        key TEXT PRIMARY KEY,
        value TEXT NOT NULL
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS imported_files (
        source_path TEXT NOT NULL,
        source_sha256 TEXT NOT NULL,
        line_count INTEGER NOT NULL,
        imported_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
        PRIMARY KEY (source_path, source_sha256)
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS sessions (
        city_id TEXT NOT NULL,
        host_id TEXT NOT NULL,
        provider TEXT NOT NULL,
        session_id TEXT NOT NULL,
        parent_session_id TEXT,
        first_timestamp TEXT,
        role TEXT,
        PRIMARY KEY (city_id, host_id, provider, session_id)
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS events (
        city_id TEXT NOT NULL,
        host_id TEXT NOT NULL,
        provider TEXT NOT NULL,
        session_id TEXT NOT NULL,
        event_id TEXT NOT NULL,
        timestamp TEXT NOT NULL,
        kind TEXT NOT NULL,
        title TEXT,
        text TEXT,
        tool_name TEXT,
        tool_call_id TEXT,
        command TEXT,
        exit_code INTEGER,
        duration_ms INTEGER,
        model TEXT,
        repo TEXT,
        commit_sha TEXT,
        parent_session_id TEXT,
        bead_id TEXT,
        formula_id TEXT,
        payload_hash TEXT NOT NULL,
        source_path TEXT NOT NULL,
        source_sha256 TEXT NOT NULL,
        source_line INTEGER NOT NULL,
        observed_timestamp TEXT,
        PRIMARY KEY (city_id, host_id, provider, session_id, event_id)
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS event_usage (
        city_id TEXT NOT NULL,
        host_id TEXT NOT NULL,
        provider TEXT NOT NULL,
        session_id TEXT NOT NULL,
        event_id TEXT NOT NULL,
        input_tokens INTEGER,
        output_tokens INTEGER,
        cache_read_tokens INTEGER,
        cache_write_tokens INTEGER,
        total_tokens INTEGER,
        PRIMARY KEY (city_id, host_id, provider, session_id, event_id)
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS jev_requests (
        request_hash TEXT PRIMARY KEY,
        snapshot_hash TEXT NOT NULL,
        subject_kind TEXT NOT NULL,
        taxonomy_version TEXT NOT NULL,
        question_hash TEXT NOT NULL,
        model TEXT NOT NULL,
        request_json TEXT NOT NULL,
        request_bytes INTEGER NOT NULL,
        source_path TEXT,
        source_sha256 TEXT,
        city_id TEXT,
        host_id TEXT,
        provider TEXT,
        session_id TEXT
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS classifications (
        classification_id INTEGER PRIMARY KEY AUTOINCREMENT,
        subject_kind TEXT NOT NULL,
        snapshot_hash TEXT NOT NULL,
        taxonomy_version TEXT NOT NULL,
        question_hash TEXT NOT NULL,
        model_version TEXT NOT NULL,
        request_hash TEXT NOT NULL,
        response_hash TEXT NOT NULL,
        source_path TEXT,
        source_sha256 TEXT,
        UNIQUE (subject_kind, snapshot_hash, taxonomy_version, question_hash, model_version)
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS classification_answers (
        classification_id INTEGER NOT NULL,
        question_id TEXT NOT NULL,
        question_type TEXT NOT NULL,
        value_json TEXT NOT NULL,
        PRIMARY KEY (classification_id, question_id),
        FOREIGN KEY (classification_id) REFERENCES classifications(classification_id) ON DELETE CASCADE
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS gold_annotations (
        annotation_id INTEGER PRIMARY KEY AUTOINCREMENT,
        annotation_hash TEXT NOT NULL UNIQUE,
        episode_id TEXT NOT NULL,
        group_key TEXT NOT NULL,
        observed_at TEXT NOT NULL,
        taxonomy_version TEXT NOT NULL,
        facet_hash TEXT NOT NULL,
        gold_set_version TEXT NOT NULL,
        provider TEXT NOT NULL,
        annotator TEXT NOT NULL,
        adjudication TEXT NOT NULL,
        flags_json TEXT NOT NULL,
        labels_json TEXT NOT NULL,
        metadata_json TEXT NOT NULL,
        created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS changes (
        change_id TEXT PRIMARY KEY,
        change_hash TEXT NOT NULL,
        repo TEXT NOT NULL,
        kind TEXT NOT NULL,
        source_ref TEXT,
        pr INTEGER,
        title TEXT,
        body TEXT,
        labels_json TEXT NOT NULL,
        author TEXT,
        base_sha TEXT,
        head_sha TEXT,
        merge_sha TEXT,
        merged_at TEXT,
        changed_paths_json TEXT NOT NULL,
        hypothesis TEXT,
        rollback TEXT,
        artifact_digest TEXT,
        classification TEXT NOT NULL,
        categories_json TEXT NOT NULL,
        classification_evidence_json TEXT NOT NULL,
        baseline_json TEXT,
        prices_json TEXT,
        screening_version TEXT NOT NULL,
        taxonomy_version TEXT NOT NULL
    )
    """,
    """
    CREATE INDEX IF NOT EXISTS changes_repo_kind ON changes(repo, kind)
    """,
    """
    CREATE TABLE IF NOT EXISTS change_activations (
        activation_id TEXT PRIMARY KEY,
        change_id TEXT NOT NULL,
        activation_hash TEXT NOT NULL,
        mechanism TEXT NOT NULL,
        target TEXT,
        activated_at TEXT,
        deactivated_at TEXT,
        pending INTEGER NOT NULL,
        fingerprint_type TEXT,
        fingerprint_value TEXT,
        evidence TEXT,
        FOREIGN KEY (change_id) REFERENCES changes(change_id) ON DELETE CASCADE
    )
    """,
    """
    CREATE INDEX IF NOT EXISTS change_activations_change ON change_activations(change_id)
    """,
    """
    CREATE TABLE IF NOT EXISTS commit_parents (
        sha TEXT PRIMARY KEY,
        parents_json TEXT NOT NULL
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS session_fingerprints (
        session_json TEXT NOT NULL,
        type TEXT NOT NULL,
        value TEXT NOT NULL,
        observed_at TEXT NOT NULL DEFAULT '',
        evidence TEXT,
        PRIMARY KEY (session_json, type, value, observed_at)
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS exposures (
        change_id TEXT NOT NULL,
        session_json TEXT NOT NULL,
        status TEXT NOT NULL,
        evidence_json TEXT NOT NULL,
        PRIMARY KEY (change_id, session_json),
        FOREIGN KEY (change_id) REFERENCES changes(change_id) ON DELETE CASCADE
    )
    """,
    *V4_TO_V5_SCHEMA_STATEMENTS,
    *V6_CORE_SCHEMA_STATEMENTS,
    *V8_CORE_SCHEMA_STATEMENTS,
)

@dataclass
class ImportResult:
    """Outcome of one JSONL import."""

    source_path: str
    source_sha256: str
    lines_read: int = 0
    inserted: int = 0
    duplicates: int = 0
    skipped_conflicts: int = 0
    skipped_identical_file: bool = False
    notes: list[str] = field(default_factory=list)


@dataclass
class RegistryImportResult:
    """Outcome of one M5 change-registry bundle import."""

    changes_inserted: int = 0
    changes_deduplicated: int = 0
    activations_inserted: int = 0
    activations_deduplicated: int = 0
    commit_parents_inserted: int = 0
    session_fingerprints_inserted: int = 0


@dataclass
class RecommendationSaveResult:
    """Outcome of persisting one batch of M7 shadow recommendations."""

    inserted: int = 0
    deduplicated: int = 0


def _reject_json_constant(value: str) -> Any:
    raise ValueError(f"non-finite JSON constant {value!r} is not allowed")


def _decimal_event_cost_usd(
    input_tokens: Any,
    output_tokens: Any,
    cache_read_tokens: Any,
    cache_write_tokens: Any,
    input_usd_per_million: Any,
    output_usd_per_million: Any,
    cache_read_usd_per_million: Any,
    cache_write_usd_per_million: Any,
) -> str | None:
    """Calculate a cost as exact decimal USD text for SQLite's cost view."""
    token_values = (input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
    price_values = (
        input_usd_per_million,
        output_usd_per_million,
        cache_read_usd_per_million,
        cache_write_usd_per_million,
    )
    if all(value is None for value in token_values):
        return None

    terms: list[tuple[Decimal, Decimal]] = []
    for tokens, price in zip(token_values, price_values):
        if tokens is None:
            continue
        try:
            token_amount = Decimal(str(tokens))
        except InvalidOperation as exc:
            raise ValueError("event token count is not a decimal number") from exc
        if not token_amount.is_finite() or token_amount < 0:
            raise ValueError("event token count must be finite and nonnegative")
        if token_amount == 0:
            continue
        if price is None:
            return None
        try:
            unit_price = Decimal(str(price))
        except InvalidOperation as exc:
            raise ValueError("model price is not a decimal number") from exc
        if not unit_price.is_finite() or unit_price < 0:
            raise ValueError("model price must be finite and nonnegative")
        if unit_price != 0:
            terms.append((token_amount, unit_price))

    if not terms:
        return "0"

    min_exponent = min(
        tokens.as_tuple().exponent + price.as_tuple().exponent - 6
        for tokens, price in terms
    )
    max_adjusted = max(
        len(tokens.as_tuple().digits) + len(price.as_tuple().digits) - 1
        + tokens.as_tuple().exponent + price.as_tuple().exponent - 6
        for tokens, price in terms
    )
    product_precision = max(
        len(tokens.as_tuple().digits) + len(price.as_tuple().digits)
        for tokens, price in terms
    )
    with localcontext() as context:
        context.prec = max(28, product_precision, max_adjusted - min_exponent + 2)
        total = sum(
            (tokens * price / Decimal(1_000_000) for tokens, price in terms),
            Decimal(0),
        )

    text = format(total, "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text or "0"


def _split_jsonl_lines(text: str) -> list[str]:
    """Split JSONL on LF only, dropping one trailing CR per line.

    ``str.splitlines`` also splits on U+2028, U+2029 and U+0085, which are legal
    unescaped inside JSON strings. Those characters must stay inside their record,
    so splitting there would reject a valid file as malformed. Line numbers are
    still 1-based over the LF-delimited records.
    """
    lines = text.split("\n")
    return [line[:-1] if line.endswith("\r") else line for line in lines]


class ObservatoryStore:
    """A rebuildable analytical projection over normalized observatory JSONL."""

    SCHEMA_VERSION = DB_SCHEMA_VERSION

    def __init__(self, path: str | os.PathLike[str]):
        self.path = str(path)
        if self.path != ":memory:":
            parent = os.path.dirname(os.path.abspath(self.path))
            if parent:
                os.makedirs(parent, exist_ok=True)
        self._conn = sqlite3.connect(self.path, isolation_level=None)
        self._conn.row_factory = sqlite3.Row
        self._conn.create_function(
            "decimal_event_cost_usd", 8, _decimal_event_cost_usd, deterministic=True
        )
        from .pricing import normalize_provider_id

        self._conn.create_function(
            "normalize_provider_id", 1, normalize_provider_id, deterministic=True
        )
        self._conn.execute("PRAGMA foreign_keys = ON")
        self._session_snapshot_index: dict[str, set[tuple[str, str, str, str]]] | None = None
        try:
            self._ensure_schema()
        except BaseException:
            self._conn.close()
            raise

    # -- lifecycle ---------------------------------------------------------

    @property
    def conn(self) -> sqlite3.Connection:
        return self._conn

    def close(self) -> None:
        self._conn.close()

    def __enter__(self) -> "ObservatoryStore":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def _ensure_schema(self) -> None:
        version = self._conn.execute("PRAGMA user_version").fetchone()[0]
        if version not in (0, self.SCHEMA_VERSION):
            raise SchemaVersionError(
                f"database {self.path!r} has schema version {version}; "
                f"this build supports only {self.SCHEMA_VERSION}"
            )
        for statement in _SCHEMA_STATEMENTS:
            self._conn.execute(statement)
        if version == 0:
            self._conn.execute(f"PRAGMA user_version = {self.SCHEMA_VERSION}")
        row = self._conn.execute(
            "SELECT value FROM schema_meta WHERE key = 'schema_version'"
        ).fetchone()
        if row is None:
            self._conn.execute(
                "INSERT INTO schema_meta(key, value) VALUES ('schema_version', ?)",
                (str(self.SCHEMA_VERSION),),
            )
        elif row[0] != str(self.SCHEMA_VERSION):
            raise SchemaVersionError(
                f"database {self.path!r} metadata schema version is {row[0]!r}; "
                f"this build supports only {self.SCHEMA_VERSION}"
            )
        if version == 0:
            # Fresh projections start with the checked-in, source-cited price
            # schedule; existing stores can explicitly rerun the idempotent
            # seed command if they need to refresh it.
            from .pricing import seed_model_pricing

            seed_model_pricing(self)

    # -- import ------------------------------------------------------------

    def import_jsonl(self, source: str | os.PathLike[str]) -> ImportResult:
        """Import one JSONL file atomically.

        The whole file is parsed and type-checked before anything is written, and
        the writes happen in a single transaction. A malformed line aborts the
        file with its 1-based line number and commits nothing.

        A record that reuses an existing canonical identity with different
        content is skipped at the record level (counted in
        ``skipped_conflicts`` and explained in ``notes``); the rest of the file
        still imports. This keeps one bad native uuid from erasing a whole
        session's evidence.
        """
        source_path = str(source)
        try:
            data = Path(source_path).read_bytes()
        except OSError as exc:
            raise ContractError(f"cannot read import file: {exc}", source_path) from exc
        digest = sha256_bytes(data)

        already = self._conn.execute(
            "SELECT 1 FROM imported_files WHERE source_path = ? AND source_sha256 = ?",
            (source_path, digest),
        ).fetchone()
        if already is not None:
            result = ImportResult(source_path, digest, skipped_identical_file=True)
            result.lines_read = self._count_lines(data)
            return result

        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ContractError(f"import file is not valid UTF-8: {exc}", source_path) from exc

        parsed: list[tuple[int, dict[str, Any]]] = []
        for line_number, line in enumerate(_split_jsonl_lines(text), start=1):
            if not line.strip():
                continue
            try:
                raw = json.loads(line, parse_constant=_reject_json_constant)
            except (json.JSONDecodeError, ValueError) as exc:
                raise ContractError(f"invalid JSON: {exc}", source_path, line_number) from exc
            parsed.append((line_number, validate_record(raw, source_path, line_number)))

        result = ImportResult(source_path, digest, lines_read=len(parsed))
        self._conn.execute("BEGIN IMMEDIATE")
        try:
            for line_number, record in parsed:
                status, note = self._insert_record(record, source_path, digest, line_number)
                if status == "duplicate":
                    result.duplicates += 1
                elif status == "conflict":
                    result.skipped_conflicts += 1
                    if note:
                        result.notes.append(note)
                else:
                    result.inserted += 1
            self._conn.execute(
                "INSERT INTO imported_files(source_path, source_sha256, line_count) "
                "VALUES (?, ?, ?)",
                (source_path, digest, len(parsed)),
            )
            self._conn.execute("COMMIT")
        except BaseException:
            self._conn.execute("ROLLBACK")
            raise
        return result

    @staticmethod
    def _count_lines(data: bytes) -> int:
        text = data.decode("utf-8", errors="replace")
        return sum(1 for line in _split_jsonl_lines(text) if line.strip())

    def _insert_record(
        self,
        record: dict[str, Any],
        source_path: str,
        source_sha256: str,
        source_line: int,
    ) -> tuple[str, str | None]:
        """Insert one validated record; return ``(status, note)``.

        Status is ``"inserted"``, ``"duplicate"`` (identical payload already
        present) or ``"conflict"`` (same identity, different payload). A conflict
        is a record-level skip with an explanatory note rather than an abort, so
        a reused native id cannot erase the rest of the file.
        """

        identity = record_identity(record)
        digest = payload_hash(record)
        existing = self._conn.execute(
            "SELECT payload_hash FROM events WHERE city_id = ? AND host_id = ? AND "
            "provider = ? AND session_id = ? AND event_id = ?",
            identity,
        ).fetchone()
        if existing is not None:
            if existing["payload_hash"] != digest:
                note = (
                    "skipped event "
                    f"{identity_key(identity)} with reused identity and different "
                    f"payload; source {source_path}:{source_line}"
                )
                return "conflict", note
            return "duplicate", None

        trusted_binding = self._conn.execute(
            "SELECT template FROM session_enrichment WHERE city_id = ? AND host_id = ? "
            "AND provider = ? AND session_id = ?",
            identity[:4],
        ).fetchone()
        role = trusted_binding["template"] if trusted_binding is not None else None
        self._conn.execute(
            "INSERT OR IGNORE INTO sessions(city_id, host_id, provider, session_id, "
            "parent_session_id, first_timestamp, role) VALUES (?, ?, ?, ?, ?, ?, ?)",
            (
                identity[0],
                identity[1],
                identity[2],
                identity[3],
                record.get("parent_session_id"),
                record.get("timestamp"),
                role,
            ),
        )
        if role is not None:
            self._conn.execute(
                "UPDATE sessions SET role = ? WHERE city_id = ? AND host_id = ? "
                "AND provider = ? AND session_id = ? AND role IS NULL",
                (role, *identity[:4]),
            )
        # ``first_timestamp`` is the earliest observed event, not merely the first
        # one imported: a later import of an earlier event must lower it.
        self._conn.execute(
            "UPDATE sessions SET first_timestamp = ? WHERE city_id = ? AND host_id = ? "
            "AND provider = ? AND session_id = ? "
            "AND (first_timestamp IS NULL OR first_timestamp > ?)",
            (
                record.get("timestamp"),
                identity[0],
                identity[1],
                identity[2],
                identity[3],
                record.get("timestamp"),
            ),
        )
        if record.get("parent_session_id"):
            self._conn.execute(
                "UPDATE sessions SET parent_session_id = ? WHERE city_id = ? AND host_id = ? "
                "AND provider = ? AND session_id = ? AND parent_session_id IS NULL",
                (
                    record["parent_session_id"],
                    identity[0],
                    identity[1],
                    identity[2],
                    identity[3],
                ),
            )
        self._conn.execute(
            f"INSERT INTO events({', '.join(_EVENT_COLUMNS)}) "
            f"VALUES ({', '.join('?' for _ in _EVENT_COLUMNS)})",
            [
                *(record.get(column) for column in _DATA_COLUMNS),
                digest,
                source_path,
                source_sha256,
                source_line,
                record.get("observed_timestamp"),
            ],
        )
        usage = record.get("usage")
        if usage is not None:
            self._conn.execute(
                "INSERT INTO event_usage(city_id, host_id, provider, session_id, event_id, "
                "input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, total_tokens) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                identity + tuple(usage.get(field) for field in USAGE_INT_FIELDS),
            )
        self._session_snapshot_index = None
        return "inserted", None

    # -- read helpers ------------------------------------------------------

    def schema_version(self) -> int:
        return int(self._conn.execute("PRAGMA user_version").fetchone()[0])

    def event_count(self) -> int:
        return int(self._conn.execute("SELECT COUNT(*) FROM events").fetchone()[0])

    def session_count(self) -> int:
        return int(self._conn.execute("SELECT COUNT(*) FROM sessions").fetchone()[0])

    def iter_events(self) -> Iterator[dict[str, Any]]:
        rows = self._conn.execute(
            "SELECT * FROM events ORDER BY timestamp, city_id, host_id, provider, session_id, event_id"
        ).fetchall()
        for row in rows:
            yield dict(row)

    def get_event(self, identity: Sequence[str]) -> dict[str, Any] | None:
        row = self._conn.execute(
            "SELECT * FROM events WHERE city_id = ? AND host_id = ? AND provider = ? "
            "AND session_id = ? AND event_id = ?",
            tuple(identity),
        ).fetchone()
        return dict(row) if row is not None else None

    def get_usage(self, identity: Sequence[str]) -> dict[str, Any] | None:
        row = self._conn.execute(
            "SELECT * FROM event_usage WHERE city_id = ? AND host_id = ? AND provider = ? "
            "AND session_id = ? AND event_id = ?",
            tuple(identity),
        ).fetchone()
        return dict(row) if row is not None else None

    def session_keys(self) -> list[tuple[str, str, str, str]]:
        rows = self._conn.execute(
            "SELECT city_id, host_id, provider, session_id FROM sessions "
            "ORDER BY city_id, host_id, provider, session_id"
        ).fetchall()
        return [tuple(row) for row in rows]

    def session_events(self, session_key: Sequence[str]) -> list[dict[str, Any]]:
        rows = self._conn.execute(
            "SELECT * FROM events WHERE city_id = ? AND host_id = ? AND provider = ? "
            "AND session_id = ? ORDER BY timestamp, event_id",
            tuple(session_key),
        ).fetchall()
        return [dict(row) for row in rows]

    def event_snapshot(self, session_key: Sequence[str], event_id: str) -> str:
        row = self.get_event(tuple(session_key) + (event_id,))
        if row is None:
            raise ContractError(
                f"event {identity_key(tuple(session_key) + (event_id,))} not found in projection"
            )
        return event_snapshot_hash(tuple(session_key) + (event_id,), row["payload_hash"])

    def session_snapshot(self, session_key: Sequence[str]) -> str:
        events = self.session_events(session_key)
        if not events:
            raise ContractError(
                f"session {identity_key(session_key)} has no events in projection"
            )
        return session_snapshot_hash(
            session_key, [(event["event_id"], event["payload_hash"]) for event in events]
        )

    def _snapshot_bindings(
        self,
    ) -> dict[str, set[tuple[tuple[str, str, str, str], str]]]:
        """Return an in-memory index of current session/event snapshot identities."""
        if self._session_snapshot_index is not None:
            return self._session_snapshot_index
        index: dict[str, set[tuple[tuple[str, str, str, str], str]]] = {}
        for key in self.session_keys():
            events = self.session_events(key)
            if not events:
                continue
            raw_snapshot = session_snapshot_hash(
                key, [(event["event_id"], event["payload_hash"]) for event in events]
            )
            index.setdefault(raw_snapshot, set()).add((key, "snapshot"))
            index.setdefault(session_text_snapshot_hash(raw_snapshot), set()).add(
                (key, "text_snapshot")
            )
            for event in events:
                event_hash = event_snapshot_hash(
                    tuple(key) + (event["event_id"],), event["payload_hash"]
                )
                index.setdefault(event_hash, set()).add((key, "event_snapshot"))
        self._session_snapshot_index = index
        return index

    def resolve_session_binding(
        self, snapshot_hash: str, subject_kind: str
    ) -> tuple[tuple[str, str, str, str], str] | None:
        """Resolve a classification snapshot to one exact session, never by guess."""
        if subject_kind == "session":
            allowed = {"snapshot", "text_snapshot"}
        elif subject_kind == "event":
            allowed = {"event_snapshot"}
        else:
            return None
        candidates = {
            (key, method)
            for key, method in self._snapshot_bindings().get(snapshot_hash, set())
            if method in allowed
        }
        if len(candidates) != 1:
            return None
        return next(iter(candidates))

    # -- Jev request provenance -------------------------------------------

    def save_request(
        self,
        *,
        request_hash: str,
        snapshot_hash: str,
        subject_kind: str,
        taxonomy_version: str,
        question_hash: str,
        model: str,
        request_json: str,
        request_bytes: int,
        source_path: str | None = None,
        source_sha256: str | None = None,
        session_key: Sequence[str] | None = None,
    ) -> bool:
        """Persist a generated request and its exact session when resolvable.

        Collectors may build requests without passing an identity. In that case
        the current snapshot index records the unique owning session at request
        time; an unmatched or ambiguous hash remains unbound.
        """
        binding = self.resolve_session_binding(snapshot_hash, subject_kind)
        if session_key is not None:
            if isinstance(session_key, (str, bytes)):
                raise ContractError("session_key must be a four-item sequence, not text")
            parts = tuple(session_key)
            if len(parts) != 4 or not all(isinstance(part, str) and part for part in parts):
                raise ContractError("session_key must contain four non-empty strings")
            explicit_key = parts  # type: ignore[assignment]
            if binding is None or binding[0] != explicit_key:
                # An explicit identity is accepted only when its snapshot proves
                # that it names this exact subject.
                explicit_key = None
            if explicit_key is not None:
                binding = (explicit_key, binding[1])
        identity = binding[0] if binding is not None else (None, None, None, None)
        cursor = self._conn.execute(
            "INSERT OR IGNORE INTO jev_requests(request_hash, snapshot_hash, subject_kind, "
            "taxonomy_version, question_hash, model, request_json, request_bytes, source_path, "
            "source_sha256, city_id, host_id, provider, session_id) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                request_hash,
                snapshot_hash,
                subject_kind,
                taxonomy_version,
                question_hash,
                model,
                request_json,
                request_bytes,
                source_path,
                source_sha256,
                *identity,
            ),
        )
        if binding is not None:
            self._conn.execute(
                "UPDATE jev_requests SET city_id = ?, host_id = ?, provider = ?, session_id = ? "
                "WHERE request_hash = ? AND city_id IS NULL AND host_id IS NULL "
                "AND provider IS NULL AND session_id IS NULL",
                (*binding[0], request_hash),
            )
        return cursor.rowcount == 1

    def get_request(self, request_hash: str) -> dict[str, Any] | None:
        row = self._conn.execute(
            "SELECT * FROM jev_requests WHERE request_hash = ?", (request_hash,)
        ).fetchone()
        return dict(row) if row is not None else None

    # -- classifications ---------------------------------------------------

    def _find_classification(
        self,
        *,
        subject_kind: str,
        snapshot_hash: str,
        taxonomy_version: str,
        question_hash: str,
        model_version: str,
    ) -> sqlite3.Row | None:
        return self._conn.execute(
            "SELECT classification_id, response_hash FROM classifications WHERE "
            "subject_kind = ? AND snapshot_hash = ? AND taxonomy_version = ? AND "
            "question_hash = ? AND model_version = ?",
            (subject_kind, snapshot_hash, taxonomy_version, question_hash, model_version),
        ).fetchone()

    def _checked_session_binding(
        self, session_key: Sequence[str], binding_method: str
    ) -> tuple[tuple[str, str, str, str], str] | None:
        if isinstance(session_key, (str, bytes)):
            raise ContractError("session_key must be a four-item sequence, not text")
        parts = tuple(session_key)
        if len(parts) != 4 or not all(isinstance(part, str) and part for part in parts):
            raise ContractError("session_key must contain four non-empty strings")
        if binding_method not in {
            "request", "snapshot", "text_snapshot", "event_snapshot", "source_provenance", "explicit"
        }:
            raise ContractError(f"unsupported classification binding method {binding_method!r}")
        exists = self._conn.execute(
            "SELECT 1 FROM sessions WHERE city_id = ? AND host_id = ? AND provider = ? "
            "AND session_id = ?",
            parts,
        ).fetchone()
        if exists is None:
            return None
        return (parts, binding_method)  # type: ignore[return-value]

    def _record_classification_binding(
        self, classification_id: int, binding: tuple[tuple[str, str, str, str], str]
    ) -> None:
        key, method = binding
        existing = self._conn.execute(
            "SELECT city_id, host_id, provider, session_id FROM classification_sessions "
            "WHERE classification_id = ?",
            (classification_id,),
        ).fetchone()
        if existing is not None:
            if tuple(existing) != key:
                raise ObservatoryError(
                    f"classification {classification_id} is already bound to a different session"
                )
            return
        self._conn.execute(
            "INSERT INTO classification_sessions(classification_id, city_id, host_id, provider, "
            "session_id, binding_method) VALUES (?, ?, ?, ?, ?, ?)",
            (classification_id, *key, method),
        )

    def save_classification(
        self,
        *,
        subject_kind: str,
        snapshot_hash: str,
        taxonomy_version: str,
        question_hash: str,
        model_version: str,
        request_hash: str,
        response_hash: str,
        answers: Iterable[dict[str, Any]],
        source_path: str | None = None,
        source_sha256: str | None = None,
        session_key: Sequence[str] | None = None,
        binding_method: str = "explicit",
    ) -> tuple[int, bool]:
        """Store an immutable classification and its exact owning session.

        A persisted request identity wins even if the session acquired later
        events after the request was classified. Without one, only an exact
        current snapshot match creates a binding; unmatched hashes stay unbound.
        """
        if session_key is not None:
            binding = self._checked_session_binding(session_key, binding_method)
        else:
            resolved = self.resolve_session_binding(snapshot_hash, subject_kind)
            binding = resolved
        self._conn.execute("BEGIN IMMEDIATE")
        try:
            existing = self._find_classification(
                subject_kind=subject_kind,
                snapshot_hash=snapshot_hash,
                taxonomy_version=taxonomy_version,
                question_hash=question_hash,
                model_version=model_version,
            )
            if existing is not None:
                if existing["response_hash"] == response_hash:
                    if binding is not None:
                        self._record_classification_binding(
                            int(existing["classification_id"]), binding
                        )
                    self._conn.execute("COMMIT")
                    return int(existing["classification_id"]), True
                raise LabelConflictError(
                    "refusing to overwrite classification for subject "
                    f"{subject_kind}:{snapshot_hash} taxonomy {taxonomy_version} "
                    f"model {model_version}"
                )

            cursor = self._conn.execute(
                "INSERT INTO classifications(subject_kind, snapshot_hash, taxonomy_version, "
                "question_hash, model_version, request_hash, response_hash, source_path, "
                "source_sha256) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    subject_kind,
                    snapshot_hash,
                    taxonomy_version,
                    question_hash,
                    model_version,
                    request_hash,
                    response_hash,
                    source_path,
                    source_sha256,
                ),
            )
            classification_id = int(cursor.lastrowid)
            for answer in answers:
                self._conn.execute(
                    "INSERT INTO classification_answers(classification_id, question_id, "
                    "question_type, value_json) VALUES (?, ?, ?, ?)",
                    (
                        classification_id,
                        answer["question_id"],
                        answer["question_type"],
                        canonical_json(answer["answer"]),
                    ),
                )
            if binding is not None:
                self._record_classification_binding(classification_id, binding)
            self._conn.execute("COMMIT")
            return classification_id, False
        except sqlite3.IntegrityError:
            if self._conn.in_transaction:
                self._conn.execute("ROLLBACK")
            existing = self._find_classification(
                subject_kind=subject_kind,
                snapshot_hash=snapshot_hash,
                taxonomy_version=taxonomy_version,
                question_hash=question_hash,
                model_version=model_version,
            )
            if existing is not None:
                if existing["response_hash"] == response_hash:
                    if binding is not None:
                        self._record_classification_binding(
                            int(existing["classification_id"]), binding
                        )
                    return int(existing["classification_id"]), True
                raise LabelConflictError(
                    "refusing to overwrite classification for subject "
                    f"{subject_kind}:{snapshot_hash} taxonomy {taxonomy_version} "
                    f"model {model_version}"
                ) from None
            raise
        except BaseException:
            if self._conn.in_transaction:
                self._conn.execute("ROLLBACK")
            raise

    def get_classification(
        self, *, subject_kind: str, snapshot_hash: str, taxonomy_version: str,
        question_hash: str, model_version: str,
    ) -> dict[str, Any] | None:
        row = self._conn.execute(
            "SELECT * FROM classifications WHERE subject_kind = ? AND snapshot_hash = ? "
            "AND taxonomy_version = ? AND question_hash = ? AND model_version = ?",
            (subject_kind, snapshot_hash, taxonomy_version, question_hash, model_version),
        ).fetchone()
        if row is None:
            return None
        answers = self._conn.execute(
            "SELECT question_id, question_type, value_json FROM classification_answers "
            "WHERE classification_id = ? ORDER BY question_id",
            (row["classification_id"],),
        ).fetchall()
        record = dict(row)
        record["answers"] = [
            {
                "question_id": answer["question_id"],
                "question_type": answer["question_type"],
                "answer": json.loads(answer["value_json"]),
            }
            for answer in answers
        ]
        return record

    def classifications_for_session(
        self,
        session_key: Sequence[str],
        *,
        subject_kind: str | None = None,
    ) -> list[dict[str, Any]]:
        """Read classifications by durable session binding, newest first."""
        key = tuple(session_key)
        if len(key) != 4 or not all(isinstance(part, str) and part for part in key):
            raise ContractError("session_key must contain four non-empty strings")
        query = (
            "SELECT c.*, b.binding_method, b.bound_at, s.role AS session_role, "
            "a.question_id, a.question_type, a.value_json "
            "FROM classification_sessions AS b "
            "JOIN classifications AS c ON c.classification_id = b.classification_id "
            "JOIN sessions AS s ON s.city_id = b.city_id AND s.host_id = b.host_id "
            "AND s.provider = b.provider AND s.session_id = b.session_id "
            "LEFT JOIN classification_answers AS a ON a.classification_id = c.classification_id "
            "WHERE b.city_id = ? AND b.host_id = ? AND b.provider = ? AND b.session_id = ?"
        )
        params: tuple[Any, ...] = key
        if subject_kind is not None:
            query += " AND c.subject_kind = ?"
            params += (subject_kind,)
        query += " ORDER BY c.classification_id DESC, a.question_id"
        grouped: dict[int, dict[str, Any]] = {}
        for row in self._conn.execute(query, params):
            classification_id = int(row["classification_id"])
            record = grouped.setdefault(
                classification_id,
                {
                    key: row[key]
                    for key in row.keys()
                    if key not in {"question_id", "question_type", "value_json"}
                },
            )
            record.setdefault("session_key", key)
            record.setdefault("answers", [])
            if row["question_id"] is not None:
                record["answers"].append(
                    {
                        "question_id": row["question_id"],
                        "question_type": row["question_type"],
                        "answer": json.loads(row["value_json"]),
                    }
                )
        return list(grouped.values())

    def latest_classification_for_session(
        self,
        session_key: Sequence[str],
        *,
        subject_kind: str = "session",
    ) -> dict[str, Any] | None:
        """Return the newest bound classification for one session, if present."""
        rows = self.classifications_for_session(session_key, subject_kind=subject_kind)
        return rows[0] if rows else None


    def classification_count(self) -> int:
        return int(self._conn.execute("SELECT COUNT(*) FROM classifications").fetchone()[0])

    def save_model_pricing(self, rows: Iterable[Mapping[str, Any]]) -> int:
        """Append exact USD-per-million prices idempotently; conflicting keys are refused."""
        from .pricing import _decimal_text, _price, normalize_provider_id

        columns = (
            "model_id",
            "provider",
            "input_usd_per_million",
            "output_usd_per_million",
            "cache_read_usd_per_million",
            "cache_write_usd_per_million",
            "effective_from",
            "source",
        )
        normalized_rows: list[tuple[Any, ...]] = []
        for row in rows:
            model_id = row.get("model_id")
            provider = row.get("provider")
            effective_from = row.get("effective_from")
            source = row.get("source")
            if (
                not isinstance(model_id, str)
                or not model_id.strip()
                or "provider" not in row
                or (
                    provider is not None
                    and (not isinstance(provider, str) or not provider.strip())
                )
                or not isinstance(effective_from, str)
                or not effective_from
                or not isinstance(source, str)
                or not source
            ):
                raise ContractError(
                    "model pricing needs model_id, provider (or null), effective_from and source"
                )
            try:
                effective_from = normalize_timestamp(effective_from)
            except ContractError as exc:
                raise PricingError(
                    "model pricing.effective_from must be timezone-aware ISO-8601"
                ) from exc
            price_values = (
                _price(
                    row.get("input_usd_per_million"),
                    "input_usd_per_million",
                    "model pricing",
                    required=True,
                ),
                _price(
                    row.get("output_usd_per_million"),
                    "output_usd_per_million",
                    "model pricing",
                    required=True,
                ),
                _price(
                    row.get("cache_read_usd_per_million"),
                    "cache_read_usd_per_million",
                    "model pricing",
                    required=False,
                ),
                _price(
                    row.get("cache_write_usd_per_million"),
                    "cache_write_usd_per_million",
                    "model pricing",
                    required=False,
                ),
            )
            normalized_rows.append(
                (
                    model_id.strip(),
                    normalize_provider_id(provider) if isinstance(provider, str) else None,
                    *(_decimal_text(value) for value in price_values),
                    effective_from,
                    source,
                )
            )

        inserted = 0
        self._conn.execute("BEGIN IMMEDIATE")
        try:
            for values in normalized_rows:
                cursor = self._conn.execute(
                    "INSERT OR IGNORE INTO model_pricing(" + ", ".join(columns) + ") "
                    "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                    values,
                )
                if cursor.rowcount == 1:
                    inserted += 1
                    continue
                existing = self._conn.execute(
                    "SELECT " + ", ".join(columns) + " FROM model_pricing "
                    "WHERE model_id = ? AND provider IS ? AND effective_from = ?",
                    (values[0], values[1], values[6]),
                ).fetchone()
                if existing is None or tuple(existing) != values:
                    raise ObservatoryError(
                        f"conflicting model pricing for {values[0]!r} provider "
                        f"{values[1]!r} effective {values[6]!r}"
                    )
            self._conn.execute("COMMIT")
            return inserted
        except BaseException:
            if self._conn.in_transaction:
                self._conn.execute("ROLLBACK")
            raise


    # -- gold annotations --------------------------------------------------

    def save_gold_annotations(self, gold_set: Any) -> int:
        """Append gold annotations from *gold_set*; return newly stored rows.

        Rows are content-keyed and append-only: identical annotations dedupe, a
        correction for the same episode is a new row, and no prediction row is
        ever rewritten. This keeps human labels and model output separate.
        """
        inserted = 0
        self._conn.execute("BEGIN IMMEDIATE")
        try:
            for episode in gold_set.episodes:
                cursor = self._conn.execute(
                    "INSERT OR IGNORE INTO gold_annotations(annotation_hash, episode_id, "
                    "group_key, observed_at, taxonomy_version, facet_hash, gold_set_version, "
                    "provider, annotator, adjudication, flags_json, labels_json, metadata_json) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        episode.annotation_hash(),
                        episode.episode_id,
                        episode.group_key,
                        episode.observed_at,
                        gold_set.taxonomy_version,
                        gold_set.facet_hash,
                        gold_set.gold_set_version,
                        episode.provider,
                        episode.annotator,
                        episode.adjudication,
                        canonical_json(sorted(episode.flags)),
                        canonical_json({key: list(value) for key, value in sorted(episode.labels.items())}),
                        canonical_json(dict(episode.metadata)),
                    ),
                )
                inserted += cursor.rowcount
            self._conn.execute("COMMIT")
        except BaseException:
            self._conn.execute("ROLLBACK")
            raise
        return inserted

    def iter_gold_annotations(self) -> Iterator[dict[str, Any]]:
        rows = self._conn.execute(
            "SELECT * FROM gold_annotations ORDER BY annotation_id"
        ).fetchall()
        for row in rows:
            record = dict(row)
            record["flags"] = json.loads(record.pop("flags_json"))
            record["labels"] = json.loads(record.pop("labels_json"))
            record["metadata"] = json.loads(record.pop("metadata_json"))
            yield record

    def gold_annotation_count(self) -> int:
        return int(self._conn.execute("SELECT COUNT(*) FROM gold_annotations").fetchone()[0])


    # -- M5 optimization registry -----------------------------------------

    def import_registry(self, bundle: dict[str, Any]) -> RegistryImportResult:
        """Import a normalized change bundle atomically.

        Re-importing an identical change or activation deduplicates; re-importing
        the same identity with different content raises
        :class:`RegistryConflictError`. The bundle must already be normalized by
        :func:`agent_observatory.changes.normalize_change_bundle`.
        """
        changes = bundle.get("changes", [])
        activations = bundle.get("activations", [])
        graph = bundle.get("commit_graph") or {}
        fingerprints = bundle.get("session_fingerprints", [])
        result = RegistryImportResult()

        self._conn.execute("BEGIN IMMEDIATE")
        try:
            for change in changes:
                existing = self._conn.execute(
                    "SELECT change_hash FROM changes WHERE change_id = ?",
                    (change["change_id"],),
                ).fetchone()
                if existing is not None:
                    if existing["change_hash"] != change["change_hash"]:
                        raise RegistryConflictError(
                            "refusing to overwrite change "
                            f"{change['change_id']} (existing content differs)"
                        )
                    result.changes_deduplicated += 1
                    continue
                self._conn.execute(
                    "INSERT INTO changes(change_id, change_hash, repo, kind, source_ref, pr, "
                    "title, body, labels_json, author, base_sha, head_sha, merge_sha, merged_at, "
                    "changed_paths_json, hypothesis, rollback, artifact_digest, classification, "
                    "categories_json, classification_evidence_json, baseline_json, prices_json, "
                    "screening_version, taxonomy_version) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        change["change_id"],
                        change["change_hash"],
                        change["repo"],
                        change["kind"],
                        change.get("source_ref"),
                        change.get("pr"),
                        change.get("title"),
                        change.get("body"),
                        canonical_json(change.get("labels") or []),
                        change.get("author"),
                        change.get("base_sha"),
                        change.get("head_sha"),
                        change.get("merge_sha"),
                        change.get("merged_at"),
                        canonical_json(change.get("changed_paths") or []),
                        change.get("hypothesis"),
                        change.get("rollback"),
                        change.get("artifact_digest"),
                        change["classification"],
                        canonical_json(change.get("categories") or []),
                        canonical_json(change.get("classification_evidence") or []),
                        canonical_json(change["baseline"]) if change.get("baseline") is not None else None,
                        canonical_json(change["prices"]) if change.get("prices") is not None else None,
                        change["screening_version"],
                        change["taxonomy_version"],
                    ),
                )
                result.changes_inserted += 1

            for activation in activations:
                existing = self._conn.execute(
                    "SELECT activation_hash FROM change_activations WHERE activation_id = ?",
                    (activation["activation_id"],),
                ).fetchone()
                if existing is not None:
                    if existing["activation_hash"] != activation["activation_hash"]:
                        raise RegistryConflictError(
                            "refusing to overwrite activation "
                            f"{activation['activation_id']} (existing content differs)"
                        )
                    result.activations_deduplicated += 1
                    continue
                fingerprint = activation.get("fingerprint") or {}
                self._conn.execute(
                    "INSERT INTO change_activations(activation_id, change_id, activation_hash, "
                    "mechanism, target, activated_at, deactivated_at, pending, fingerprint_type, "
                    "fingerprint_value, evidence) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        activation["activation_id"],
                        activation["change_id"],
                        activation["activation_hash"],
                        activation["mechanism"],
                        activation.get("target"),
                        activation.get("activated_at"),
                        activation.get("deactivated_at"),
                        1 if activation.get("pending") else 0,
                        fingerprint.get("type"),
                        fingerprint.get("value"),
                        activation.get("evidence"),
                    ),
                )
                result.activations_inserted += 1

            for sha, parents in graph.items():
                parents_json = canonical_json(list(parents))
                existing = self._conn.execute(
                    "SELECT parents_json FROM commit_parents WHERE sha = ?",
                    (sha,),
                ).fetchone()
                if existing is not None:
                    # ``commit_parents`` is immutable evidence too: the same sha
                    # with different parents is a conflict, not an update, even
                    # though the original INSERT OR IGNORE silently kept the old
                    # row. Identical content stays a no-op.
                    if existing["parents_json"] != parents_json:
                        raise RegistryConflictError(
                            "refusing to overwrite commit parents for "
                            f"{sha} (existing content differs)"
                        )
                    continue
                self._conn.execute(
                    "INSERT INTO commit_parents(sha, parents_json) VALUES (?, ?)",
                    (sha, parents_json),
                )
                result.commit_parents_inserted += 1

            for fingerprint in fingerprints:
                observed_at = fingerprint.get("observed_at") or ""
                cursor = self._conn.execute(
                    "INSERT OR IGNORE INTO session_fingerprints(session_json, type, value, "
                    "observed_at, evidence) VALUES (?, ?, ?, ?, ?)",
                    (
                        identity_key(fingerprint["session"]),
                        fingerprint["type"],
                        fingerprint["value"],
                        observed_at,
                        fingerprint.get("evidence"),
                    ),
                )
                result.session_fingerprints_inserted += cursor.rowcount

            self._conn.execute("COMMIT")
        except sqlite3.IntegrityError as exc:
            self._conn.execute("ROLLBACK")
            raise RegistryError(f"registry import rejected: {exc}") from None
        except BaseException:
            self._conn.execute("ROLLBACK")
            raise
        return result

    def change_count(self) -> int:
        return int(self._conn.execute("SELECT COUNT(*) FROM changes").fetchone()[0])

    def activation_count(self) -> int:
        return int(self._conn.execute("SELECT COUNT(*) FROM change_activations").fetchone()[0])

    def exposure_count(self) -> int:
        return int(self._conn.execute("SELECT COUNT(*) FROM exposures").fetchone()[0])

    def get_change(self, change_id: str) -> dict[str, Any] | None:
        row = self._conn.execute(
            "SELECT * FROM changes WHERE change_id = ?", (change_id,)
        ).fetchone()
        return _change_from_row(row) if row is not None else None

    def iter_changes(self) -> Iterator[dict[str, Any]]:
        rows = self._conn.execute("SELECT * FROM changes ORDER BY change_id").fetchall()
        for row in rows:
            yield _change_from_row(row)

    def iter_activations(self, change_id: str | None = None) -> Iterator[dict[str, Any]]:
        if change_id is None:
            rows = self._conn.execute(
                "SELECT * FROM change_activations ORDER BY activation_id"
            ).fetchall()
        else:
            rows = self._conn.execute(
                "SELECT * FROM change_activations WHERE change_id = ? ORDER BY activation_id",
                (change_id,),
            ).fetchall()
        for row in rows:
            yield _activation_from_row(row)

    def load_commit_graph(self) -> dict[str, list[str]]:
        rows = self._conn.execute("SELECT sha, parents_json FROM commit_parents").fetchall()
        return {row["sha"]: json.loads(row["parents_json"]) for row in rows}

    def load_session_enrichments(self) -> dict[tuple[str, str, str, str], dict[str, Any]]:
        """Return trusted GC template/repository bindings keyed by full session identity."""
        rows = self._conn.execute(
            "SELECT city_id, host_id, provider, session_id, template, repo, repo_source "
            "FROM session_enrichment ORDER BY city_id, host_id, provider, session_id"
        ).fetchall()
        return {
            (row["city_id"], row["host_id"], row["provider"], row["session_id"]): {
                "template": row["template"],
                "repo": row["repo"],
                "repo_source": row["repo_source"],
            }
            for row in rows
        }

    def load_session_fingerprints(self) -> list[dict[str, Any]]:
        enrichments = self.load_session_enrichments()
        rows = self._conn.execute(
            "SELECT session_json, type, value, observed_at, evidence FROM session_fingerprints "
            "ORDER BY session_json, type, value, observed_at"
        ).fetchall()
        fingerprints = []
        for row in rows:
            session = json.loads(row["session_json"])
            enrichment = enrichments.get(tuple(session), {})
            fingerprints.append(
                {
                    "session": session,
                    "type": row["type"],
                    "value": row["value"],
                    "observed_at": row["observed_at"] or None,
                    "evidence": row["evidence"],
                    "repo": enrichment.get("repo"),
                }
            )
        return fingerprints

    def replace_exposures(self, rows: Iterable[dict[str, Any]]) -> int:
        """Replace the derived exposure projection in one transaction.

        Exposures are recomputable derived rows, so a fresh join replaces the
        previous one rather than appending a second copy.
        """
        self._conn.execute("BEGIN IMMEDIATE")
        try:
            self._conn.execute("DELETE FROM exposures")
            count = 0
            for row in rows:
                self._conn.execute(
                    "INSERT INTO exposures(change_id, session_json, status, evidence_json) "
                    "VALUES (?, ?, ?, ?)",
                    (
                        row["change_id"],
                        identity_key(row["session"]),
                        row["status"],
                        canonical_json(row.get("evidence") or []),
                    ),
                )
                count += 1
            self._conn.execute("COMMIT")
        except BaseException:
            self._conn.execute("ROLLBACK")
            raise
        return count

    def iter_exposures(self) -> Iterator[dict[str, Any]]:
        rows = self._conn.execute(
            "SELECT change_id, session_json, status, evidence_json FROM exposures "
            "ORDER BY change_id, session_json"
        ).fetchall()
        for row in rows:
            yield {
                "change_id": row["change_id"],
                "session": json.loads(row["session_json"]),
                "status": row["status"],
                "evidence": json.loads(row["evidence_json"]),
            }

    # -- M7 shadow recommendations ----------------------------------------

    def save_recommendations(self, rows: Iterable[dict[str, Any]]) -> RecommendationSaveResult:
        """Persist shadow recommendations append-only, keyed by content hash.

        The row id is the canonical hash of the stored ``payload``, so replaying
        the same shadow run deduplicates instead of appending a second copy, and
        a genuinely different recommendation (new catalog or classification) is
        retained as new evidence. Nothing here writes routing or dispatch.
        """
        result = RecommendationSaveResult()
        self._conn.execute("BEGIN IMMEDIATE")
        try:
            for row in rows:
                payload = row.get("payload")
                recommendation_id = canonical_hash(payload)
                existing = self._conn.execute(
                    "SELECT 1 FROM recommendations WHERE recommendation_id = ?",
                    (recommendation_id,),
                ).fetchone()
                if existing is not None:
                    result.deduplicated += 1
                    continue
                self._conn.execute(
                    "INSERT INTO recommendations(recommendation_id, episode_id, work_item_id, "
                    "kind, decision, eligibility, eligibility_reason, confidence, uncertainty, "
                    "recommended_candidate, current_candidate, fallback_candidate, fallback_path, "
                    "disagreement, temporal_leak_free, as_of, catalog_version, evaluator_version, "
                    "payload_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        recommendation_id,
                        row["episode_id"],
                        row.get("work_item_id"),
                        row["kind"],
                        row["decision"],
                        row["eligibility"],
                        row["eligibility_reason"],
                        row.get("confidence"),
                        row.get("uncertainty"),
                        row.get("recommended_candidate"),
                        row.get("current_candidate"),
                        row.get("fallback_candidate"),
                        row.get("fallback_path"),
                        1 if row.get("disagreement") else 0,
                        1 if row.get("temporal_leak_free") else 0,
                        row["as_of"],
                        row["catalog_version"],
                        row["evaluator_version"],
                        canonical_json(payload),
                    ),
                )
                result.inserted += 1
            self._conn.execute("COMMIT")
        except BaseException:
            self._conn.execute("ROLLBACK")
            raise
        return result

    def recommendation_count(self) -> int:
        return int(self._conn.execute("SELECT COUNT(*) FROM recommendations").fetchone()[0])

    def iter_recommendations(self) -> Iterator[dict[str, Any]]:
        rows = self._conn.execute(
            "SELECT recommendation_id, episode_id, work_item_id, kind, decision, eligibility, "
            "eligibility_reason, confidence, uncertainty, recommended_candidate, current_candidate, "
            "fallback_candidate, fallback_path, disagreement, temporal_leak_free, as_of, "
            "catalog_version, evaluator_version, payload_json "
            "FROM recommendations ORDER BY episode_id, kind, recommendation_id"
        ).fetchall()
        for row in rows:
            yield {
                "recommendation_id": row["recommendation_id"],
                "episode_id": row["episode_id"],
                "work_item_id": row["work_item_id"],
                "kind": row["kind"],
                "decision": row["decision"],
                "eligibility": row["eligibility"],
                "eligibility_reason": row["eligibility_reason"],
                "confidence": row["confidence"],
                "uncertainty": row["uncertainty"],
                "recommended_candidate": row["recommended_candidate"],
                "current_candidate": row["current_candidate"],
                "fallback_candidate": row["fallback_candidate"],
                "fallback_path": row["fallback_path"],
                "disagreement": bool(row["disagreement"]),
                "temporal_leak_free": bool(row["temporal_leak_free"]),
                "as_of": row["as_of"],
                "catalog_version": row["catalog_version"],
                "evaluator_version": row["evaluator_version"],
                "payload": json.loads(row["payload_json"]),
            }


def _change_from_row(row: sqlite3.Row) -> dict[str, Any]:
    baseline = row["baseline_json"]
    prices = row["prices_json"]
    return {
        "change_id": row["change_id"],
        "change_hash": row["change_hash"],
        "repo": row["repo"],
        "kind": row["kind"],
        "source_ref": row["source_ref"],
        "pr": row["pr"],
        "title": row["title"],
        "body": row["body"],
        "labels": json.loads(row["labels_json"]),
        "author": row["author"],
        "base_sha": row["base_sha"],
        "head_sha": row["head_sha"],
        "merge_sha": row["merge_sha"],
        "merged_at": row["merged_at"],
        "changed_paths": json.loads(row["changed_paths_json"]),
        "hypothesis": row["hypothesis"],
        "rollback": row["rollback"],
        "artifact_digest": row["artifact_digest"],
        "classification": row["classification"],
        "categories": json.loads(row["categories_json"]),
        "classification_evidence": json.loads(row["classification_evidence_json"]),
        "baseline": json.loads(baseline) if baseline is not None else None,
        "prices": json.loads(prices) if prices is not None else None,
        "screening_version": row["screening_version"],
        "taxonomy_version": row["taxonomy_version"],
    }


def _activation_from_row(row: sqlite3.Row) -> dict[str, Any]:
    fingerprint = None
    if row["fingerprint_type"] is not None:
        fingerprint = {"type": row["fingerprint_type"], "value": row["fingerprint_value"]}
    return {
        "activation_id": row["activation_id"],
        "change_id": row["change_id"],
        "activation_hash": row["activation_hash"],
        "mechanism": row["mechanism"],
        "target": row["target"],
        "activated_at": row["activated_at"],
        "deactivated_at": row["deactivated_at"],
        "pending": bool(row["pending"]),
        "fingerprint": fingerprint,
        "evidence": row["evidence"],
    }


def content_hash(value: Any) -> str:
    """Hash arbitrary JSON content canonically (used for responses)."""
    return canonical_hash(value)
