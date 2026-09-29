"""Canonical serialization, hashing, and snapshot identity helpers.

Determinism is a contract here: two runs over the same evidence must produce
byte-identical hashes and reports. All JSON that feeds a hash is serialized with
sorted keys, no insignificant whitespace, and UTF-8 output.
"""

from __future__ import annotations

import hashlib
import json
from typing import Any, Iterable, Sequence

# Canonical event identity is the five-part key below. Title and text are never
# part of identity because they are untrusted, mutable prose.
IDENTITY_FIELDS = ("city_id", "host_id", "provider", "session_id", "event_id")


def canonical_json(value: Any) -> str:
    """Serialize *value* to canonical JSON (sorted keys, compact, UTF-8 safe).

    Non-finite floats (NaN/Infinity) are rejected: they have no valid JSON
    representation, so allowing them would corrupt both hashes and wire bodies.
    """
    return json.dumps(
        value,
        sort_keys=True,
        ensure_ascii=False,
        separators=(",", ":"),
        allow_nan=False,
    )


def sha256_text(text: str) -> str:
    """Return the hex sha256 of *text* encoded as UTF-8."""
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def sha256_bytes(data: bytes) -> str:
    """Return the hex sha256 of raw bytes."""
    return hashlib.sha256(data).hexdigest()


def canonical_hash(value: Any) -> str:
    """Return the hex sha256 of the canonical JSON form of *value*."""
    return sha256_text(canonical_json(value))


def event_identity(record: dict[str, Any]) -> tuple[str, str, str, str, str]:
    """Return the canonical (city, host, provider, session, event) identity."""
    return tuple(record[field] for field in IDENTITY_FIELDS)  # type: ignore[return-value]


def identity_key(identity: Sequence[str]) -> str:
    """Return a collision-free string form of an identity tuple.

    Identity components are arbitrary strings, so joining them with a delimiter
    would make ``("c|h", "x")`` and ``("c", "h|x")`` collide. The JSON-array
    encoding used here cannot collide: it escapes and length-delimits every
    component. Use this for report keys, CLI records, and log labels; never use
    a bare delimiter join.
    """
    return canonical_json(list(identity))


def event_snapshot_hash(identity: Sequence[str], payload_hash: str) -> str:
    """Hash an event subject by its canonical identity plus its payload hash."""
    return canonical_hash(["event", list(identity), payload_hash])


def session_snapshot_hash(
    session_identity: Sequence[str],
    ordered_events: Iterable[tuple[str, str]],
) -> str:
    """Hash a session subject by its identity plus the ordered event payloads.

    *ordered_events* must already be ordered deterministically (timestamp then
    event id); this function sorts defensively so the result cannot depend on
    caller iteration order.
    """
    canonical_events = sorted((event_id, payload_hash) for event_id, payload_hash in ordered_events)
    return canonical_hash(["session", list(session_identity), canonical_events])


# Text-mode classification deliberately occupies a separate snapshot namespace
# from metadata-mode classification. Keep this helper aligned with the existing
# collector namespace so stores and migrations can resolve both scopes.
SESSION_TEXT_SNAPSHOT_NAMESPACE = "session-text"


def session_text_snapshot_hash(snapshot_hash: str) -> str:
    """Return the text-mode namespace for a raw session snapshot hash."""
    return canonical_hash([SESSION_TEXT_SNAPSHOT_NAMESPACE, snapshot_hash])