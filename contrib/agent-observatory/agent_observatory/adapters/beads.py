"""Read-only adapter for the Gas City bead store held in Dolt.

Unlike the transcript adapters this source is not a file: one *ref* of a Dolt
database holds many beads, and each bead is one classification episode. The
adapter shells out to ``gc dolt sql`` in the city directory and reads the CSV
result stream, so it never opens the Dolt files itself and never issues a
writing statement.

Mapping
-------

* one bead -> one session (``session_id = "<bead-id>@<ref>"``) so the store
  segments a single episode per bead;
* the episode body is the bead ``title``, ``description`` and ``notes`` plus
  every ``comment`` in time order, each emitted as a ``note`` event (``note``
  is the only non-tool kind the text state carries);
* ``status``/``assignee``/``priority``/``issue_type``/labels ride on the first
  event as a machine-readable header line, so Jev sees the bead's state as well
  as its prose;
* every text value is run through :mod:`agent_observatory.adapters.redaction`
  before it can leave the host.

Two scopes
----------

``--ref main`` reads the live branch; ``--ref remotes/origin/main`` reads the
pre-compaction remote-tracking ref (head ``ammnfp9q`` at the time of writing)
without fetching. The ref is interpolated into a quoted SQL string literal, so
it is validated against a conservative character set first.

Read-only guarantee
-------------------

:func:`read_beads` refuses any SQL statement that is not ``USE``/``SELECT`` and
the default runner never passes a write flag. No Dolt state is created, so the
adapter is safe to point at a live city.
"""

from __future__ import annotations

import contextlib
import csv
import io
import re
import subprocess
import threading
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Callable, Iterator, Sequence

from ..canonical import sha256_bytes, sha256_text
from ..contract import SCHEMA_VERSION
from .base import (
    AdapterContext,
    AdapterError,
    AdapterResult,
    SourceAdapter,
    SourceSizeExceeded,
    TitleRevision,
)
from .git_evidence import normalize_repo_identity
from .redaction import redact_and_bound, redact_text

PROVIDER = "beads"
ADAPTER_VERSION = "1.0.0"

# The adapter always runs with the city directory as its working directory;
# ``gc dolt sql`` reaches the managed Dolt server through that city.
DEFAULT_CITY = "/home/coolhenrylinux/city"
DEFAULT_DATABASE = "gl"
DEFAULT_EXECUTABLE = "gc"
DEFAULT_TIMEOUT_SECONDS = 600.0

STREAM_CHUNK_BYTES = 64 * 1024
# Keep only a bounded prefix of a child's stderr for the failure message while
# still draining the pipe, so a chatty child cannot block on stderr backpressure.
_STDERR_PREFIX_BYTES = 64 * 1024

# Fields compared by :func:`diff_bead_snapshots`.
DIFF_FIELDS: tuple[str, ...] = (
    "title",
    "description",
    "notes",
    "status",
    "assignee",
    "priority",
    "issue_type",
)

# A ref is interpolated into a quoted SQL literal. Only names the Dolt/beads
# toolchain actually issues are accepted (branches, remote-tracking refs and
# commit hashes), which also makes injection impossible.
_REF_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]*$")

# SQL passed to the runner must be a read-only script. ``gc dolt sql`` itself
# would accept writes; the guard makes the read-only contract explicit and
# testable instead of relying on operator discipline.
_READ_ONLY_STATEMENT_RE = re.compile(r"^\s*(?:USE\s+[A-Za-z0-9_]+|SELECT\b)", re.IGNORECASE)

# A ``SELECT`` head is not enough to be read-only: Dolt also lets a SELECT write
# a server-side file or bind variables (``INTO OUTFILE``/``INTO DUMPFILE``/
# ``INTO @var``) and take write locks (``FOR UPDATE``). Those escape hatches
# ride inside an otherwise read-only-looking statement, so they are rejected
# anywhere in it rather than only at its head.
_READ_ONLY_ESCAPES: tuple[tuple[re.Pattern[str], str], ...] = (
    (re.compile(r"\bINTO\s+OUTFILE\b", re.IGNORECASE), "INTO OUTFILE"),
    (re.compile(r"\bINTO\s+DUMPFILE\b", re.IGNORECASE), "INTO DUMPFILE"),
    # ``@`` is a symbol, not a word, so MySQL accepts ``INTO@var`` with no
    # whitespace between the keyword and the variable.
    (re.compile(r"\bINTO\s*@", re.IGNORECASE), "INTO @variable"),
    (re.compile(r"\bFOR\s+UPDATE\b", re.IGNORECASE), "FOR UPDATE"),
)

SqlRunner = Callable[[str, "DoltCommand"], Iterator[bytes]]


class BeadsError(AdapterError):
    """A bead-store read failed (bad ref, guard rejection or Dolt error)."""


@dataclass(frozen=True)
class DoltCommand:
    """Where and how to reach the read-only Dolt SQL shell."""

    city: str = DEFAULT_CITY
    database: str = DEFAULT_DATABASE
    executable: str = DEFAULT_EXECUTABLE
    timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS


def normalize_ref(ref: str) -> str:
    """Validate and return a Dolt ref suitable for interpolation.

    Rejects empty strings, whitespace, quotes and anything outside the Dolt
    ref character set. The returned value is unchanged; the point is to refuse
    a value that could break out of the SQL string literal.
    """

    if not isinstance(ref, str) or not ref:
        raise BeadsError("bead ref must be a non-empty string")
    if not _REF_RE.match(ref):
        raise BeadsError(
            f"invalid bead ref {ref!r}; expected a Dolt branch, remote-tracking ref or commit hash"
        )
    return ref


def _identifier(name: str, *, what: str) -> str:
    """Validate a bare SQL identifier (database/table-ish name)."""

    if not isinstance(name, str) or not re.match(r"^[A-Za-z0-9_]+$", name):
        raise BeadsError(f"invalid {what} {name!r}; expected a bare identifier")
    return name


def _find_sql_comment(sql: str) -> str | None:
    """Return the first SQL comment marker outside a string literal, or ``None``.

    Comments are refused rather than stripped. MySQL's ``/*! ... */`` is an
    *executable* comment, so removing comment text before the escape scan would
    let a write escape hide inside it; refusing comments sidesteps that parser
    differential entirely. ``--`` opens a comment only when followed by
    whitespace or a control character (MySQL's rule), while ``#`` and ``/*``
    always do. Markers inside quoted strings or backtick identifiers are literal
    text, so a validated ref such as ``a--b`` interpolated into ``AS OF 'a--b'``
    is not mistaken for one.
    """

    index = 0
    length = len(sql)
    while index < length:
        char = sql[index]
        if char in ("'", '"', "`"):
            quote = char
            index += 1
            while index < length:
                inner = sql[index]
                # Backslash escapes and doubled quotes keep a quote from ending
                # the literal early; a backtick identifier only doubles.
                if inner == "\\" and quote != "`":
                    index += 2
                    continue
                if inner == quote:
                    if index + 1 < length and sql[index + 1] == quote:
                        index += 2
                        continue
                    index += 1
                    break
                index += 1
            continue
        if char == "-" and sql.startswith("--", index):
            following = sql[index + 2] if index + 2 < length else ""
            is_control = following == "" or (ord(following) < 0x20 or ord(following) == 0x7F)
            if following.isspace() or is_control:
                return "--"
        elif char == "#":
            return "#"
        elif char == "/" and sql.startswith("/*", index):
            return "/*"
        index += 1
    return None


def assert_read_only(sql: str) -> str:
    """Return *sql* when every statement is ``USE`` or ``SELECT``, else refuse.

    ``gc dolt sql`` will happily run DDL/DML, so the read-only guarantee is
    enforced here rather than assumed. Splitting on ``;`` is adequate because
    the generated SQL contains no semicolons inside string literals. A
    ``SELECT`` that still writes or locks (``INTO OUTFILE|DUMPFILE|@var`` or
    ``FOR UPDATE``) is refused wherever the escape appears in the statement, and
    any SQL comment is refused because a comment can stand in for the
    whitespace the escape patterns require (``INTO/**/OUTFILE``) or hide an
    executable ``/*! ... */`` payload.
    """

    comment = _find_sql_comment(sql)
    if comment is not None:
        raise BeadsError(f"refusing read-only statement containing a SQL comment ({comment!r})")
    statements = [statement for statement in sql.split(";") if statement.strip()]
    if not statements:
        raise BeadsError("refusing to run an empty SQL script")
    for statement in statements:
        if not _READ_ONLY_STATEMENT_RE.match(statement):
            first = statement.strip().split(None, 1)[0] if statement.strip() else ""
            raise BeadsError(f"refusing non-read-only statement {first!r}")
        for escape, label in _READ_ONLY_ESCAPES:
            if escape.search(statement):
                raise BeadsError(f"refusing read-only statement with write escape {label!r}")
    return sql


def _default_sql_runner(sql: str, command: DoltCommand) -> Iterator[bytes]:
    """Stream ``gc dolt sql -r csv`` output for *sql*.

    The query is passed as ``-q`` and the child's stdout is read in chunks so a
    caller can enforce ``--max-source-bytes`` without materializing an
    unbounded result. stderr is drained on a helper thread to avoid a
    pipe-buffer deadlock. ``DoltCommand.timeout_seconds`` is a wall-clock
    deadline: a watchdog kills the child when it expires, so a hung Dolt call
    fails instead of blocking the read forever.
    """

    argv = [
        command.executable,
        "dolt",
        "sql",
        "-r",
        "csv",
        "-q",
        sql,
    ]
    proc = subprocess.Popen(
        argv,
        cwd=command.city,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    stderr_chunks: list[bytes] = []
    stderr_bytes = 0

    def drain_stderr() -> None:
        nonlocal stderr_bytes
        if proc.stderr is None:
            return
        try:
            while True:
                chunk = proc.stderr.read(STREAM_CHUNK_BYTES)
                if not chunk:
                    break
                if stderr_bytes < _STDERR_PREFIX_BYTES:
                    keep = chunk[: _STDERR_PREFIX_BYTES - stderr_bytes]
                    stderr_chunks.append(keep)
                    stderr_bytes += len(keep)
        except (OSError, ValueError):
            pass

    # The deadline is enforced out of band: the read below may block on a child
    # that never writes or closes its stdout, so a timer thread must be what
    # ends the wait. ``timed_out`` distinguishes an expiry from an ordinary
    # non-zero exit after the read drains.
    timed_out = threading.Event()

    def expire() -> None:
        timed_out.set()
        with contextlib.suppress(Exception):
            proc.kill()

    timeout = command.timeout_seconds
    timer: threading.Timer | None = None
    if timeout and timeout > 0:
        timer = threading.Timer(float(timeout), expire)
        timer.daemon = True
        timer.start()

    stderr_reader = threading.Thread(target=drain_stderr, daemon=True)
    stderr_reader.start()
    try:
        if proc.stdout is not None:
            while True:
                chunk = proc.stdout.read(STREAM_CHUNK_BYTES)
                if not chunk:
                    break
                yield chunk
    finally:
        if timer is not None:
            timer.cancel()
        with contextlib.suppress(Exception):
            if proc.poll() is None:
                proc.kill()
        with contextlib.suppress(Exception):
            proc.wait()
        stderr_reader.join(timeout=5)
        for stream in (proc.stdout, proc.stderr):
            if stream is not None:
                with contextlib.suppress(OSError):
                    stream.close()

    if timed_out.is_set():
        raise BeadsError(f"gc dolt sql exceeded its {timeout:g}s timeout", "dolt")
    if proc.returncode != 0:
        reason = b"".join(stderr_chunks).decode("utf-8", "replace").strip().splitlines()
        raise BeadsError(f"gc dolt sql failed: {reason[0] if reason else 'unknown error'}", "dolt")


def _bounded_join(chunks: Iterator[bytes], max_bytes: int | None, source_path: str) -> bytes:
    """Join *chunks*, refusing to exceed *max_bytes*.

    The iterator is closed explicitly before an over-cap error propagates, so
    the subprocess behind it is killed immediately instead of waiting for
    garbage collection.
    """

    if max_bytes is not None and max_bytes < 1:
        raise BeadsError(f"max source bytes must be >= 1, got {max_bytes}", source_path)
    buffer = bytearray()
    try:
        for chunk in chunks:
            buffer.extend(chunk)
            if max_bytes is not None and len(buffer) > max_bytes:
                raise SourceSizeExceeded(source_path, max_bytes)
    finally:
        close = getattr(chunks, "close", None)
        if close is not None:
            with contextlib.suppress(Exception):
                close()
    return bytes(buffer)


def _iso_utc(value: str) -> str | None:
    """Convert a Dolt ``DATETIME`` string to canonical UTC ISO-8601.

    Dolt returns naive datetimes; the bead store writes UTC, so the value is
    interpreted as UTC and given an explicit ``Z``. Returns ``None`` when the
    value is missing or unparseable so the caller can flag the row instead of
    inventing a time.
    """

    if not value:
        return None
    candidate = value.strip()
    if not candidate:
        return None
    try:
        parsed = datetime.fromisoformat(candidate)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def _events_sql(ref: str, database: str) -> str:
    """Return the read-only SQL selecting one row per bead body field."""

    database = _identifier(database, what="database")
    ref = normalize_ref(ref)
    # Dolt's AS OF accepts a branch, remote-tracking ref or commit hash as a
    # quoted string. Each base table carries it because a derived table cannot.
    return f"""USE {database};
SELECT e.bead_id AS bead_id,
       e.event_kind AS event_kind,
       e.ordinal AS ordinal,
       e.source_id AS source_id,
       e.occurred_at AS occurred_at,
       e.text AS text,
       i.status AS status,
       COALESCE(i.assignee, '') AS assignee,
       i.priority AS priority,
       i.issue_type AS issue_type,
       COALESCE(l.labels, '') AS labels
FROM (
    SELECT id AS bead_id, 'title' AS event_kind, 0 AS ordinal, 'title' AS source_id,
           created_at AS occurred_at, title AS text FROM issues AS OF '{ref}'
    UNION ALL
    SELECT id, 'description', 1, 'description', created_at, description FROM issues AS OF '{ref}'
    UNION ALL
    SELECT id, 'notes', 2, 'notes', created_at, notes FROM issues AS OF '{ref}'
    UNION ALL
    SELECT c.issue_id, 'comment', 3, c.id, c.created_at, c.text
      FROM comments AS OF '{ref}' c
) e
JOIN issues AS OF '{ref}' i ON i.id = e.bead_id
LEFT JOIN (
    SELECT issue_id, GROUP_CONCAT(label ORDER BY label SEPARATOR ',') AS labels
      FROM labels AS OF '{ref}' GROUP BY issue_id
) l ON l.issue_id = i.id
ORDER BY e.bead_id, e.ordinal, e.occurred_at, e.source_id"""


def _fields_sql(ref: str, database: str) -> str:
    """Return the read-only SQL selecting one row per bead (for diffing)."""

    database = _identifier(database, what="database")
    ref = normalize_ref(ref)
    return f"""USE {database};
SELECT id AS bead_id,
       title AS title,
       description AS description,
       notes AS notes,
       status AS status,
       COALESCE(assignee, '') AS assignee,
       priority AS priority,
       issue_type AS issue_type
FROM issues AS OF '{ref}'
ORDER BY id"""


def _metadata_header(row: dict[str, str]) -> str:
    """Return the machine-readable bead-state header for the first event."""

    labels = row.get("labels") or ""
    return (
        f"[beads] status={row.get('status') or ''} assignee={row.get('assignee') or ''} "
        f"priority={row.get('priority') or ''} type={row.get('issue_type') or ''} "
        f"labels={labels}"
    )


# ``event_id`` is an identity field, so it is excluded from the body redactor.
# The ``source_id``/``event_kind`` columns folded into it are unconstrained
# bead-store text (a comment id is normally a UUID, but the column is free
# text), so each value is redacted and then required to be a short, conforming
# token before it can become part of the id. Anything else is replaced by a
# deterministic digest marker, which drops the original bytes while keeping
# distinct rows on distinct event ids.
_IDENTITY_TOKEN_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


def _identity_token(value: str, *, fallback: str) -> str:
    """Return a safe, redacted ``event_id`` component for an untrusted value.

    Redaction runs first so a credential that happens to use only id-safe
    characters (``ghp_...`` is all letters, digits and underscore) cannot pass a
    bare charset check, then a non-conforming or over-long result is replaced by
    a digest marker rather than being truncated into a possibly shorter secret.
    """

    candidate = value.strip()
    if not candidate:
        candidate = fallback
    redacted = redact_text(candidate)
    if _IDENTITY_TOKEN_RE.match(redacted):
        return redacted
    return f"redacted-{sha256_text(candidate)[:12]}"


class BeadsAdapter(SourceAdapter):
    """Read-only adapter over one Dolt ref of the bead store."""

    provider = PROVIDER
    adapter_version = ADAPTER_VERSION

    def detect(self, source_path: str) -> bool:
        """Recognize only the synthetic ``dolt://`` source ids.

        The bead source is not discovered from the filesystem, so this never
        matches a transcript path and cannot shadow the file adapters.
        """

        return source_path.startswith(("dolt://", "beads://"))

    def parse(
        self,
        data: bytes,
        *,
        context: AdapterContext,
        generation: int,
        source_path: str,
        source_sha256: str,
    ) -> AdapterResult:
        """Parse the CSV emitted by :func:`_default_sql_runner`.

        ``session_id`` names the *ref* because one CSV holds every bead; the
        per-bead session ids live on the records. The result is a plain
        :class:`AdapterResult` so the caller can validate and import it through
        the same path as a file adapter.
        """

        ref = _source_ref(source_path)
        result = AdapterResult(
            provider=self.provider,
            adapter_version=self.adapter_version,
            source_path=source_path,
            source_sha256=source_sha256,
            source_size_bytes=len(data),
            session_id=f"{source_path}",
            parent_session_id=None,
        )
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise AdapterError(f"bead source is not valid UTF-8: {exc}", source_path) from exc

        reader = csv.DictReader(io.StringIO(text))
        if reader.fieldnames is None:
            result.note_skip("empty_source")
            result.line_count = 0
            return result

        rows = 0
        position_by_bead: dict[str, int] = {}
        current_bead: str | None = None
        title_recorded = False
        for row in reader:
            rows += 1
            bead_id = (row.get("bead_id") or "").strip()
            if not bead_id:
                result.note_skip("missing_bead_id")
                continue
            if bead_id != current_bead:
                current_bead = bead_id
                title_recorded = False
            timestamp = _iso_utc(row.get("occurred_at") or "")
            if timestamp is None:
                result.note_skip("no_timestamp")
                continue

            # Both components feed ``event_id``, which the body redactor never
            # sees, so each is constrained to a safe token before it is used.
            event_kind = _identity_token(row.get("event_kind") or "note", fallback="note")
            source_id = _identity_token(row.get("source_id") or event_kind, fallback=event_kind)
            raw_text = row.get("text") or ""
            position = position_by_bead.get(bead_id, 0)
            if position > 0 and not raw_text.strip():
                # An empty description/notes/comment carries no evidence; only
                # the first event (which always carries the metadata header) is
                # emitted even when its prose is empty.
                result.note_skip("empty_field")
                continue
            position_by_bead[bead_id] = position + 1

            if position == 0:
                body = f"{_metadata_header(row)}\n\n{raw_text}" if raw_text else _metadata_header(row)
            else:
                body = raw_text
            text_value = redact_and_bound(body)

            session_id = f"{bead_id}@{ref}"
            result.records.append(
                self._record(
                    context,
                    result,
                    session_id=session_id,
                    bead_id=bead_id,
                    event_id=f"{position:05d}-{event_kind}-{source_id}",
                    timestamp=timestamp,
                    kind="note",
                    text=text_value,
                )
            )
            if position == 0 and not title_recorded:
                title_recorded = True
                result.title_revisions.append(
                    TitleRevision(
                        title=redact_and_bound(raw_text),
                        position=0,
                        observed_timestamp=timestamp,
                        source="bead.title",
                    )
                )

        result.line_count = rows
        if rows == 0:
            result.note_skip("empty_source")
        for session_id in sorted({record["session_id"] for record in result.records}):
            notes = ["commit_sha:none (Dolt bead snapshots contain no paired Git tool results)"]
            if normalize_repo_identity(context.repo) is None:
                notes.append("repo:none (no remote URL or valid configured owner/name observed)")
            result.session_evidence_notes[session_id] = notes
        return result

    @staticmethod
    def _record(
        context: AdapterContext,
        result: AdapterResult,
        *,
        session_id: str,
        bead_id: str,
        event_id: str,
        timestamp: str,
        kind: str,
        text: str | None,
    ) -> dict[str, Any]:
        return {
            "schema_version": SCHEMA_VERSION,
            "city_id": context.city_id,
            "host_id": context.host_id,
            "provider": PROVIDER,
            "session_id": session_id,
            "event_id": event_id,
            "timestamp": timestamp,
            "kind": kind,
            "title": None,
            "text": text,
            "tool_name": None,
            "tool_call_id": None,
            "command": None,
            "exit_code": None,
            "duration_ms": None,
            "model": None,
            "repo": normalize_repo_identity(context.repo),
            "commit_sha": None,
            "parent_session_id": None,
            "bead_id": bead_id,
            "formula_id": None,
            "usage": None,
        }


def _source_ref(source_path: str) -> str:
    """Extract the ref from a ``dolt://<database>@<ref>`` source id."""

    for scheme in ("dolt://", "beads://"):
        if source_path.startswith(scheme):
            remainder = source_path[len(scheme) :]
            _, _, ref = remainder.partition("@")
            return ref or remainder
    return ""


def read_beads(
    ref: str,
    *,
    context: AdapterContext,
    max_bytes: int | None = None,
    city: str = DEFAULT_CITY,
    database: str = DEFAULT_DATABASE,
    executable: str = DEFAULT_EXECUTABLE,
    timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
    runner: SqlRunner | None = None,
) -> AdapterResult:
    """Read one Dolt ref into normalized records, bounded by *max_bytes*.

    *runner* exists so tests can supply a synthetic CSV without a live Dolt
    server; the default is the ``gc dolt sql`` subprocess reader. The SQL is
    checked to be read-only before it runs.
    """

    ref = normalize_ref(ref)
    database = _identifier(database, what="database")
    sql = assert_read_only(_events_sql(ref, database))
    source_path = f"dolt://{database}@{ref}"
    command = DoltCommand(
        city=city,
        database=database,
        executable=executable,
        timeout_seconds=timeout_seconds,
    )
    stream = (runner or _default_sql_runner)(sql, command)
    data = _bounded_join(stream, max_bytes, source_path)

    adapter = BeadsAdapter()
    result = adapter.parse(
        data,
        context=context,
        generation=1,
        source_path=source_path,
        source_sha256=sha256_bytes(data),
    )
    result.source_size_bytes = len(data)
    # Import locally so this module can be imported by ``adapters/__init__``
    # without a circular import at module load time.
    from . import validated_records

    result.records = validated_records(result.records, source_path, result)
    return result


# -- ref diffing -----------------------------------------------------------


def parse_bead_fields(data: bytes, *, source_path: str = "dolt://beads") -> dict[str, dict[str, str]]:
    """Parse the CSV from :func:`_fields_sql` into ``{bead_id: fields}``.

    Every value is run through :func:`redact_and_bound` before it enters the
    map, so the raw ``title``/``description``/``notes``/``assignee`` text can
    never reach a snapshot diff (or the report built from it). This is the only
    place the fields map is populated, so callers never hold unredacted values.
    """

    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise AdapterError(f"bead field source is not valid UTF-8: {exc}", source_path) from exc
    reader = csv.DictReader(io.StringIO(text))
    if reader.fieldnames is None:
        return {}
    fields: dict[str, dict[str, str]] = {}
    for row in reader:
        bead_id = (row.get("bead_id") or "").strip()
        if not bead_id:
            continue
        fields[bead_id] = {name: redact_and_bound(row.get(name) or "") for name in DIFF_FIELDS}
    return fields


def fetch_bead_fields(
    ref: str,
    *,
    city: str = DEFAULT_CITY,
    database: str = DEFAULT_DATABASE,
    executable: str = DEFAULT_EXECUTABLE,
    timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
    max_bytes: int | None = None,
    runner: SqlRunner | None = None,
) -> dict[str, dict[str, str]]:
    """Read one ref's bead fields for a ref-to-ref diff."""

    ref = normalize_ref(ref)
    database = _identifier(database, what="database")
    sql = assert_read_only(_fields_sql(ref, database))
    source_path = f"dolt://{database}@{ref}#fields"
    command = DoltCommand(
        city=city,
        database=database,
        executable=executable,
        timeout_seconds=timeout_seconds,
    )
    stream = (runner or _default_sql_runner)(sql, command)
    data = _bounded_join(stream, max_bytes, source_path)
    return parse_bead_fields(data, source_path=source_path)


def diff_bead_snapshots(
    local: dict[str, dict[str, str]],
    remote: dict[str, dict[str, str]],
    *,
    fields: Sequence[str] = DIFF_FIELDS,
) -> list[dict[str, Any]]:
    """Return the structural diff between two bead-field snapshots.

    Each entry is ``{"bead_id", "change", "local", "remote", "changed_fields"}``
    where ``change`` is ``"only_local"``, ``"only_remote"`` or ``"fields"``.
    Only changed fields are copied into the local/remote maps, so the report
    quotes bead ids, field names and the differing values rather than whole
    bead bodies. Snapshots come from :func:`parse_bead_fields`, which redacts
    and bounds every value before it enters the map, so the quoted values are
    already safe to emit.
    """

    diffs: list[dict[str, Any]] = []
    for bead_id in sorted(set(local) | set(remote)):
        if bead_id not in local:
            diffs.append(
                {
                    "bead_id": bead_id,
                    "change": "only_remote",
                    "changed_fields": list(fields),
                    "local": None,
                    "remote": {name: remote[bead_id].get(name, "") for name in fields},
                }
            )
            continue
        if bead_id not in remote:
            diffs.append(
                {
                    "bead_id": bead_id,
                    "change": "only_local",
                    "changed_fields": list(fields),
                    "local": {name: local[bead_id].get(name, "") for name in fields},
                    "remote": None,
                }
            )
            continue
        changed = {
            name: {"local": local[bead_id].get(name, ""), "remote": remote[bead_id].get(name, "")}
            for name in fields
            if local[bead_id].get(name, "") != remote[bead_id].get(name, "")
        }
        if changed:
            diffs.append(
                {
                    "bead_id": bead_id,
                    "change": "fields",
                    "changed_fields": sorted(changed),
                    "local": {name: value["local"] for name, value in changed.items()},
                    "remote": {name: value["remote"] for name, value in changed.items()},
                }
            )
    return diffs


__all__ = [
    "ADAPTER_VERSION",
    "BeadsAdapter",
    "BeadsError",
    "DIFF_FIELDS",
    "DEFAULT_CITY",
    "DEFAULT_DATABASE",
    "DoltCommand",
    "PROVIDER",
    "assert_read_only",
    "diff_bead_snapshots",
    "fetch_bead_fields",
    "normalize_ref",
    "parse_bead_fields",
    "read_beads",
]
