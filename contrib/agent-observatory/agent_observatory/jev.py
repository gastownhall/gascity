"""Jev request building and saved-response validation.

No network transport lives in this slice. The builder emits a ``POST
/v1/systemone`` request body that can be sent later; the response importer
validates a saved response against the exact request that produced it and stores
an immutable classification.

The serialized request is capped at a conservative number of UTF-8 bytes. This
is a *byte safety cap, not tokenizer proof*: it bounds the wire payload without
claiming to equal a model's token limit. Exceeding the cap fails loudly instead
of silently truncating.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from typing import Any, Sequence

from .canonical import canonical_hash, canonical_json
from .errors import ContractError, RequestByteCapExceeded, RequestError, ResponseError
from .store import ObservatoryStore
from .taxonomy import CHOICE, NOUL, Taxonomy

# Conservative serialized-body cap. See module docstring: byte cap, not tokens.
REQUEST_BYTE_CAP = 24 * 1024

_PROBABILITY_SUM_TOLERANCE = 1e-6


def _reject_json_constant(value: str) -> Any:
    raise ValueError(f"non-finite JSON constant {value!r} is not allowed")


def _is_finite_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _is_nonnegative_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool) and value >= 0


@dataclass(frozen=True)
class JevRequest:
    """A serialized Jev/systemone request plus its deterministic provenance."""

    body: dict[str, Any]
    serialized: str
    request_hash: str
    snapshot_hash: str
    subject_kind: str
    taxonomy_version: str
    question_hash: str
    model: str
    session_key: tuple[str, str, str, str] | None = None

    @property
    def byte_length(self) -> int:
        return len(self.serialized.encode("utf-8"))


def build_request(
    state: Any,
    taxonomy: Taxonomy,
    *,
    snapshot_hash: str,
    subject_kind: str = "session",
    session_key: Sequence[str] | None = None,
) -> JevRequest:
    """Build a deterministic request body from explicitly supplied sanitized state.

    *state* must be a JSON object supplied by the caller. The builder never reads
    imported event text on its own, so untrusted transcript content cannot leak
    into a request implicitly.
    """
    if not isinstance(state, dict):
        raise RequestError(f"state must be a JSON object, got {type(state).__name__}")
    if not snapshot_hash:
        raise RequestError("snapshot_hash is required")
    if subject_kind not in {"event", "session"}:
        raise RequestError(f"subject_kind must be 'event' or 'session', got {subject_kind!r}")
    normalized_session_key = None
    if session_key is not None:
        if isinstance(session_key, (str, bytes)):
            raise RequestError("session_key must be a four-item sequence, not text")
        parts = tuple(session_key)
        if len(parts) != 4 or not all(isinstance(part, str) and part for part in parts):
            raise RequestError("session_key must contain four non-empty strings")
        normalized_session_key = parts

    body = {
        "model": taxonomy.model,
        "state": state,
        "questions": taxonomy.as_request_questions(),
    }
    try:
        serialized = canonical_json(body)
    except (TypeError, ValueError) as exc:
        raise RequestError(f"state is not JSON-serializable: {exc}") from exc

    byte_length = len(serialized.encode("utf-8"))
    if byte_length > REQUEST_BYTE_CAP:
        raise RequestByteCapExceeded(byte_length, REQUEST_BYTE_CAP)

    # The request identity covers the subject snapshot and kind as well as the
    # wire body, so two subjects that share identical state cannot collide.
    request_hash = canonical_hash(
        {
            "body": body,
            "snapshot_hash": snapshot_hash,
            "subject_kind": subject_kind,
        }
    )

    return JevRequest(
        body=body,
        serialized=serialized,
        request_hash=request_hash,
        snapshot_hash=snapshot_hash,
        subject_kind=subject_kind,
        taxonomy_version=taxonomy.taxonomy_version,
        question_hash=taxonomy.question_hash(),
        model=taxonomy.model,
        session_key=normalized_session_key,
    )


def persist_request(
    store: ObservatoryStore,
    request: JevRequest,
    *,
    source_path: str | None = None,
    source_sha256: str | None = None,
) -> bool:
    """Store a generated request so a later response can be validated against it."""
    return store.save_request(
        request_hash=request.request_hash,
        snapshot_hash=request.snapshot_hash,
        subject_kind=request.subject_kind,
        taxonomy_version=request.taxonomy_version,
        question_hash=request.question_hash,
        model=request.model,
        request_json=request.serialized,
        request_bytes=request.byte_length,
        source_path=source_path,
        source_sha256=source_sha256,
        session_key=request.session_key,
    )


def parse_json_document(text: str, *, what: str) -> Any:
    """Parse a JSON document, rejecting NaN/Infinity."""
    try:
        return json.loads(text, parse_constant=_reject_json_constant)
    except json.JSONDecodeError as exc:
        raise ResponseError(f"{what} is not valid JSON: {exc}") from exc
    except ValueError as exc:
        raise ResponseError(f"{what} contains a non-finite number: {exc}") from exc


def _validate_usage(usage: Any) -> dict[str, int]:
    if not isinstance(usage, dict):
        raise ResponseError("response usage must be an object")
    normalized: dict[str, int] = {}
    for field in ("input_tokens", "output_tokens"):
        if field not in usage:
            raise ResponseError(f"response usage is missing required field {field!r}")
        value = usage[field]
        if not _is_nonnegative_int(value):
            raise ResponseError(f"response usage.{field} must be a nonnegative integer, got {value!r}")
        normalized[field] = value
    for field in ("cache_read_tokens", "cache_write_tokens", "total_tokens"):
        if field in usage and usage[field] is not None:
            value = usage[field]
            if not _is_nonnegative_int(value):
                raise ResponseError(
                    f"response usage.{field} must be a nonnegative integer, got {value!r}"
                )
            normalized[field] = value
    return normalized


def validate_response(response: Any, request_body: dict[str, Any]) -> list[dict[str, Any]]:
    """Validate *response* against the request body; return normalized answers.

    Follows the real ``/v1/systemone`` response shape: answers use lowercase
    ``type`` values and carry ``choice``/``noul`` fields (never ``value``).
    Checks the model string, usage counters, question ids and types, choice
    probability distributions (all criteria options present, finite, in [0,1],
    summing to ~1) with confidence in [0,1], and noul probabilities in [0,1]
    with no confidence field. Unknown top-level keys are rejected so a
    server-injected extra field cannot poison replay deduplication.
    """
    if not isinstance(response, dict):
        raise ResponseError("response must be a JSON object")

    # Reject unknown top-level keys the same way answer keys are rejected. A
    # server-injected field such as ``request_id`` would otherwise be hashed
    # into ``response_hash`` and make every replay of the same logical response
    # look like a different label (a spurious LabelConflictError).
    allowed_top_level = {"model", "usage", "answers"}
    unknown_top_level = sorted(set(response) - allowed_top_level)
    if unknown_top_level:
        raise ContractError(
            "response contains unknown key(s): " + ", ".join(unknown_top_level)
        )

    expected_model = request_body.get("model")
    if response.get("model") != expected_model:
        raise ResponseError(
            f"response model {response.get('model')!r} does not match request model {expected_model!r}"
        )

    _validate_usage(response.get("usage"))

    questions = request_body.get("questions")
    if not isinstance(questions, dict):
        raise ResponseError("stored request has no questions map")
    answers = response.get("answers")
    if not isinstance(answers, dict):
        raise ResponseError("response answers must be an object")

    missing = sorted(set(questions) - set(answers))
    unexpected = sorted(set(answers) - set(questions))
    if missing:
        raise ResponseError(f"response is missing answers for: {', '.join(missing)}")
    if unexpected:
        raise ResponseError(f"response contains unknown answers: {', '.join(unexpected)}")

    normalized: list[dict[str, Any]] = []
    for question_id, question in questions.items():
        answer = answers[question_id]
        if not isinstance(answer, dict):
            raise ResponseError(f"answer {question_id!r} must be an object")
        question_type = question.get("type")
        if answer.get("type") != question_type:
            raise ResponseError(
                f"answer {question_id!r} type {answer.get('type')!r} does not match "
                f"question type {question_type!r}"
            )
        if question_type == CHOICE:
            normalized.append(
                {
                    "question_id": question_id,
                    "question_type": CHOICE,
                    "answer": _validate_choice_answer(question_id, question, answer),
                }
            )
        elif question_type == NOUL:
            normalized.append(
                {
                    "question_id": question_id,
                    "question_type": NOUL,
                    "answer": _validate_noul_answer(question_id, answer),
                }
            )
        else:
            raise ResponseError(f"unsupported question type {question_type!r}")
    return normalized


def _reject_unknown_answer_keys(
    question_id: str, answer: dict[str, Any], allowed: frozenset[str]
) -> None:
    """Reject answer keys outside the wire contract, naming every offender.

    Unknown keys are not silently dropped: dropping them let two responses with
    identical stored answers but different junk keys hash differently, which
    surfaced as a spurious LabelConflictError instead of a clean dedupe.
    """
    unknown = sorted(set(answer) - allowed)
    if unknown:
        raise ContractError(
            f"answer {question_id!r} contains unknown key(s): {', '.join(unknown)}"
        )


def _validate_choice_answer(question_id: str, question: dict[str, Any], answer: dict[str, Any]) -> dict[str, Any]:
    criteria = question.get("criteria")
    if not isinstance(criteria, dict) or not criteria:
        raise ResponseError(f"stored choice question {question_id!r} has no criteria map")
    options = list(criteria)

    value = answer.get("choice")
    if value not in options:
        raise ResponseError(
            f"answer {question_id!r} choice {value!r} is not one of {options!r}"
        )

    confidence = answer.get("confidence")
    if not _is_finite_number(confidence) or not (0.0 <= float(confidence) <= 1.0):
        raise ResponseError(
            f"answer {question_id!r} confidence must be a finite number in [0,1], got {confidence!r}"
        )

    probabilities = answer.get("probabilities")
    if not isinstance(probabilities, dict):
        raise ResponseError(f"answer {question_id!r} probabilities must be an object")
    missing = sorted(set(options) - set(probabilities))
    unexpected = sorted(set(probabilities) - set(options))
    if missing:
        raise ResponseError(
            f"answer {question_id!r} probabilities missing options: {', '.join(missing)}"
        )
    if unexpected:
        raise ResponseError(
            f"answer {question_id!r} probabilities contain unknown options: {', '.join(unexpected)}"
        )

    normalized_probabilities: dict[str, float] = {}
    total = 0.0
    for option in options:
        probability = probabilities[option]
        if not _is_finite_number(probability) or not (0.0 <= float(probability) <= 1.0):
            raise ResponseError(
                f"answer {question_id!r} probability for {option!r} must be a finite "
                f"number in [0,1], got {probability!r}"
            )
        normalized_probabilities[option] = float(probability)
        total += float(probability)

    if abs(total - 1.0) > _PROBABILITY_SUM_TOLERANCE:
        raise ResponseError(
            f"answer {question_id!r} probabilities sum to {total!r}, not ~1"
        )

    _reject_unknown_answer_keys(
        question_id, answer, frozenset({"type", "choice", "confidence", "probabilities"})
    )
    return {
        "choice": value,
        "confidence": float(confidence),
        "probabilities": normalized_probabilities,
    }


def _validate_noul_answer(question_id: str, answer: dict[str, Any]) -> dict[str, Any]:
    if "confidence" in answer:
        raise ResponseError(f"Noul answer {question_id!r} must not carry a confidence field")
    value = answer.get("noul")
    if not _is_finite_number(value) or not (0.0 <= float(value) <= 1.0):
        raise ResponseError(
            f"Noul answer {question_id!r} noul must be a finite number in [0,1], got {value!r}"
        )
    _reject_unknown_answer_keys(question_id, answer, frozenset({"type", "noul"}))
    return {"noul": float(value)}


@dataclass
class ResponseImport:
    """Outcome of importing one saved Jev response."""

    classification_id: int
    deduplicated: bool
    response_hash: str
    model: str
    taxonomy_version: str
    snapshot_hash: str
    answers: list[dict[str, Any]]


def import_response(
    store: ObservatoryStore,
    response: Any,
    *,
    request_hash: str,
    source_path: str | None = None,
    source_sha256: str | None = None,
) -> ResponseImport:
    """Validate a saved response against a stored request and persist it.

    Replaying the identical response deduplicates. A different response for the
    same subject, taxonomy, questions, and model is rejected as a label conflict
    rather than overwriting the stored classification.
    """
    request_record = store.get_request(request_hash)
    if request_record is None:
        raise ResponseError(f"no stored request with hash {request_hash!r}")
    request_body = parse_json_document(request_record["request_json"], what="stored request")

    answers = validate_response(response, request_body)
    # Hash the validated shape (not the raw server payload): unknown top-level
    # keys and unknown usage keys have already been rejected/dropped, and the
    # answers are normalized, so the same logical response always produces the
    # same hash.
    normalized_usage = _validate_usage(response.get("usage"))
    response_hash = canonical_hash(
        {
            "model": response["model"],
            "usage": normalized_usage,
            "answers": answers,
        }
    )

    classification_id, deduplicated = store.save_classification(
        subject_kind=request_record["subject_kind"],
        snapshot_hash=request_record["snapshot_hash"],
        taxonomy_version=request_record["taxonomy_version"],
        question_hash=request_record["question_hash"],
        model_version=request_record["model"],
        request_hash=request_hash,
        response_hash=response_hash,
        answers=answers,
        source_path=source_path,
        source_sha256=source_sha256,
        session_key=(
            tuple(request_record[field] for field in ("city_id", "host_id", "provider", "session_id"))
            if all(request_record.get(field) for field in ("city_id", "host_id", "provider", "session_id"))
            else None
        ),
        binding_method="request",
    )
    return ResponseImport(
        classification_id=classification_id,
        deduplicated=deduplicated,
        response_hash=response_hash,
        model=request_record["model"],
        taxonomy_version=request_record["taxonomy_version"],
        snapshot_hash=request_record["snapshot_hash"],
        answers=answers,
    )
