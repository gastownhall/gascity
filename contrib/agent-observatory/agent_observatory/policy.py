"""Shadow orchestration recommendations (M7).

``requirements.md`` R4 asks for orchestration recommendations that are *advisory
only*: a candidate action must come from the **current configured catalog**, the
fallback must be deterministic, and no learned recommendation may bypass the
existing routing. ``implementation-plan.md`` calls this the shadow-policy stage:
map validated classifications to currently configured
formulas/providers/skills/context/test plans, compare them to the existing
routing decision, and never modify it.

This module is that mapping, and it is deliberately conservative:

* **Catalog-bounded.** A recommendation can only name a candidate that exists in
  the supplied catalog, is enabled, and satisfies its intent, provider, scope,
  repository, host and capability constraints. A classification cannot name an
  arbitrary candidate; an unknown or uncovered intent simply finds no candidate
  and falls back.
* **Capability-checked.** The task declares the capabilities it requires; a
  candidate is allowed only when it declares all of them. A candidate that lacks
  a required capability is reported as a rejected candidate, never silently
  substituted.
* **Temporal (as-of).** Each record carries the decision time ``as_of`` and the
  time its classification was observed. A classification observed after the
  decision cannot inform it, and a feature that was not available at ``as_of`` is
  excluded from the decision and listed in the audit. The future completion
  label (``outcome``) is never a feature at all, so completion cannot leak
  backwards into historical intake decisions.
* **Uncertainty-aware.** Injected/uncertain/contested/changed-intent/rare
  cases, unknown intents, low-confidence predictions and (optionally) high
  entropy abstain. An abstention is a *fallback* to the existing policy, never a
  guessed recommendation.
* **Shadow-only.** The report records every recommendation's eligibility,
  confidence/uncertainty, fallback path and disagreement against the current
  routing; ``shadow.executes_changes`` is always ``false``.

The report is deterministic: identical catalogs and bundles produce
byte-identical output and a stable ``report_hash``. Recommendations are
append-only in the projection, keyed by their content hash, so a replay
deduplicates and a changed policy version is retained as new evidence.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any, Mapping, Sequence

from .annotations import KNOWN_FLAGS
from .canonical import canonical_hash
from .contract import normalize_timestamp
from .errors import ContractError, PolicyError

POLICY_BUNDLE_VERSION = "1.0"
POLICY_CATALOG_SCHEMA_VERSION = "1.0"
POLICY_REPORT_VERSION = "1"
POLICY_REPORT_KIND = "shadow_policy_recommendations"
POLICY_EVALUATOR_VERSION = "1"

# The configured surfaces a recommendation may name. This is the plan's
# "formulas/providers/skills/context/test plans" split, kept explicit so an
# unknown surface cannot be smuggled in as a kind.
CANDIDATE_KINDS = ("formula", "provider", "skill", "context_bundle", "test_plan")

DECISIONS = ("recommended", "agree", "fallback")
ELIGIBILITIES = ("eligible", "abstained", "ineligible")
FALLBACK_PATHS = ("current_routing", "catalog_default", "unavailable")

DEFAULT_CONFIDENCE_THRESHOLD = 0.9
DEFAULT_GATE_FLAGS = ("injected", "uncertain", "contested", "changed_intent", "rare")

# Abstention reason for each blocking flag, mirroring the evaluation gate so a
# reader sees the same vocabulary in both reports. Every known flag has a reason
# so an operator may add any of them to ``gate_flags`` without a crash.
FLAG_ABSTAIN_REASONS = {
    "injected": "injected_state",
    "uncertain": "uncertain",
    "contested": "contested",
    "changed_intent": "changed_intent",
    "rare": "rare_class",
    "non_english": "non_english",
    "synthetic": "synthetic_state",
}

# Feature names reserved by the record's own fields. A named feature that reuses
# one of these would let caller text shadow a validated field, so it is refused.
_RESERVED_FEATURE_NAMES = frozenset(
    {
        "intent",
        "scope",
        "confidence",
        "required_capabilities",
        "provider",
        "repo",
        "host",
        "flags",
        "labels",
        "as_of",
        "observed_at",
        "outcome",
    }
)

_CATALOG_KEYS = frozenset(
    {"schema_version", "catalog_version", "generated_by", "candidates", "defaults"}
)
_CANDIDATE_KEYS = frozenset(
    {
        "candidate_id",
        "kind",
        "label",
        "enabled",
        "priority",
        "capabilities",
        "constraints",
        "policy_version",
        "evidence",
    }
)
_CONSTRAINT_KEYS = frozenset(
    {"intents", "providers", "repos", "hosts", "scopes", "forbidden_flags", "min_confidence"}
)
_BUNDLE_KEYS = frozenset({"schema_version", "generated_by", "episodes"})
_RECORD_KEYS = frozenset(
    {
        "episode_id",
        "work_item_id",
        "session",
        "as_of",
        "observed_at",
        "intent",
        "scope",
        "confidence",
        "probabilities",
        "flags",
        "required_capabilities",
        "provider",
        "repo",
        "host",
        "labels",
        "features",
        "outcome",
        "outcome_observed_at",
        "current_routing",
        "evidence",
    }
)
_FEATURE_KEYS = frozenset({"name", "value", "available_at", "source"})


# -- typed model ------------------------------------------------------------


@dataclass(frozen=True)
class Candidate:
    """One currently configured orchestration candidate from the catalog."""

    candidate_id: str
    kind: str
    label: str
    enabled: bool = True
    priority: int = 0
    capabilities: tuple[str, ...] = ()
    constraints: Mapping[str, Any] = field(default_factory=dict)
    policy_version: str = ""
    evidence: Mapping[str, Any] | None = None

    def content(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "candidate_id": self.candidate_id,
            "kind": self.kind,
            "label": self.label,
            "enabled": self.enabled,
            "priority": self.priority,
            "capabilities": list(self.capabilities),
            "constraints": dict(sorted(self.constraints.items())),
        }
        if self.policy_version:
            payload["policy_version"] = self.policy_version
        if self.evidence is not None:
            payload["evidence"] = dict(self.evidence)
        return payload

    def candidate_hash(self) -> str:
        return canonical_hash(self.content())


@dataclass(frozen=True)
class PolicyCatalog:
    """The current configured candidate set plus the existing default routing."""

    catalog_version: str
    candidates: tuple[Candidate, ...]
    defaults: Mapping[str, str]
    generated_by: str | None = None

    def by_id(self) -> dict[str, Candidate]:
        return {candidate.candidate_id: candidate for candidate in self.candidates}

    def by_kind(self, kind: str) -> tuple[Candidate, ...]:
        return tuple(candidate for candidate in self.candidates if candidate.kind == kind)

    def content(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "catalog_version": self.catalog_version,
            "candidates": [candidate.content() for candidate in self.candidates],
            "defaults": dict(sorted(self.defaults.items())),
        }
        if self.generated_by is not None:
            payload["generated_by"] = self.generated_by
        return payload

    def catalog_hash(self) -> str:
        return canonical_hash(self.content())


@dataclass(frozen=True)
class Feature:
    """One named input feature and when it became observable."""

    name: str
    value: str
    available_at: str
    source: str = "classification"


@dataclass(frozen=True)
class ClassificationRecord:
    """One validated classification considered at a historical decision time."""

    episode_id: str
    as_of: str
    observed_at: str
    intent: str | None
    scope: str | None = None
    confidence: float | None = None
    probabilities: Mapping[str, float] | None = None
    flags: frozenset[str] = frozenset()
    required_capabilities: tuple[str, ...] = ()
    provider: str | None = None
    repo: str | None = None
    host: str | None = None
    labels: Mapping[str, tuple[str, ...]] = field(default_factory=dict)
    features: tuple[Feature, ...] = ()
    outcome: str | None = None
    outcome_observed_at: str | None = None
    current_routing: Mapping[str, str] = field(default_factory=dict)
    work_item_id: str | None = None
    evidence: Mapping[str, Any] | None = None
    session_key: tuple[str, str, str, str] | None = None

    def content(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "episode_id": self.episode_id,
            "as_of": self.as_of,
            "observed_at": self.observed_at,
            "intent": self.intent,
            "scope": self.scope,
            "confidence": self.confidence,
            "probabilities": (
                dict(sorted(self.probabilities.items())) if self.probabilities is not None else None
            ),
            "flags": sorted(self.flags),
            "required_capabilities": list(self.required_capabilities),
            "provider": self.provider,
            "repo": self.repo,
            "host": self.host,
            "labels": {key: list(value) for key, value in sorted(self.labels.items())},
            "features": [
                {
                    "name": feature.name,
                    "value": feature.value,
                    "available_at": feature.available_at,
                    "source": feature.source,
                }
                for feature in self.features
            ],
            "outcome": self.outcome,
            "outcome_observed_at": self.outcome_observed_at,
            "current_routing": dict(sorted(self.current_routing.items())),
            "work_item_id": self.work_item_id,
        }
        if self.evidence is not None:
            payload["evidence"] = dict(self.evidence)
        if self.session_key is not None:
            payload["session"] = list(self.session_key)
        return payload


@dataclass(frozen=True)
class RecommendationBundle:
    """Validated classifications (plus as-of provenance) for one shadow run."""

    records: tuple[ClassificationRecord, ...]
    schema_version: str = POLICY_BUNDLE_VERSION
    generated_by: str | None = None

    def dataset_hash(self) -> str:
        return canonical_hash(
            {
                "schema_version": self.schema_version,
                "generated_by": self.generated_by,
                "records": [record.content() for record in self.records],
            }
        )


@dataclass(frozen=True)
class PolicyConfig:
    """Thresholds for one shadow run."""

    confidence_threshold: float = DEFAULT_CONFIDENCE_THRESHOLD
    gate_flags: tuple[str, ...] = DEFAULT_GATE_FLAGS
    max_entropy_bits: float | None = None

    def validated(self) -> "PolicyConfig":
        if not (0.0 <= self.confidence_threshold <= 1.0):
            raise PolicyError("confidence_threshold must be in [0,1]")
        unknown = sorted(set(self.gate_flags) - KNOWN_FLAGS)
        if unknown:
            raise PolicyError("unknown gate flags: " + ", ".join(unknown))
        if self.max_entropy_bits is not None and self.max_entropy_bits < 0:
            raise PolicyError("max_entropy_bits must be nonnegative")
        return self


# -- validation helpers -----------------------------------------------------


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise PolicyError(message)


def _require_mapping(raw: Any, what: str) -> dict[str, Any]:
    if not isinstance(raw, dict):
        raise PolicyError(f"{what} must be a JSON object")
    return raw


def _reject_unknown_keys(raw: Mapping[str, Any], allowed: frozenset[str], what: str) -> None:
    unknown = sorted(set(raw) - allowed)
    if unknown:
        raise PolicyError(f"{what} has unknown keys: " + ", ".join(unknown))


def _require_str(raw: Mapping[str, Any], field: str, what: str) -> str:
    value = raw.get(field)
    _require(
        isinstance(value, str) and bool(value),
        f"{what} field {field!r} must be a non-empty string",
    )
    return value


def _optional_str(raw: Mapping[str, Any], field: str, what: str) -> str | None:
    value = raw.get(field)
    if value is None:
        return None
    _require(
        isinstance(value, str) and bool(value),
        f"{what} field {field!r} must be a non-empty string or null",
    )
    return value


def _optional_bool(raw: Mapping[str, Any], field: str, what: str) -> bool | None:
    value = raw.get(field)
    if value is None:
        return None
    _require(isinstance(value, bool), f"{what} field {field!r} must be a boolean")
    return value


def _optional_int(raw: Mapping[str, Any], field: str, what: str) -> int | None:
    value = raw.get(field)
    if value is None:
        return None
    _require(
        not isinstance(value, bool) and isinstance(value, int),
        f"{what} field {field!r} must be an integer",
    )
    return value


def _finite_number(value: Any, what: str) -> float:
    _require(
        not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value),
        f"{what} must be a finite number",
    )
    return float(value)


def _optional_unit_interval(raw: Mapping[str, Any], field: str, what: str) -> float | None:
    value = raw.get(field)
    if value is None:
        return None
    number = _finite_number(value, f"{what} field {field!r}")
    _require(0.0 <= number <= 1.0, f"{what} field {field!r} must be in [0,1]")
    return number


def _string_list(
    raw: Mapping[str, Any], field: str, what: str, *, required: bool
) -> tuple[str, ...]:
    value = raw.get(field)
    if value is None:
        _require(not required, f"{what} field {field!r} is required")
        return ()
    _require(
        isinstance(value, list) and all(isinstance(item, str) and item for item in value),
        f"{what} field {field!r} must be a list of non-empty strings",
    )
    _require(len(set(value)) == len(value), f"{what} field {field!r} contains duplicates")
    return tuple(value)


def _parse_timestamp(value: Any, what: str) -> str:
    if not isinstance(value, str) or not value:
        raise PolicyError(f"{what} must be a non-empty ISO-8601 timestamp")
    try:
        return normalize_timestamp(value)
    except ContractError as exc:
        raise PolicyError(f"{what}: {exc}") from exc


# -- catalog normalization --------------------------------------------------


def _normalize_constraints(raw: Any, candidate_id: str) -> dict[str, Any]:
    constraints = _require_mapping(raw, f"candidate {candidate_id!r} constraints")
    _reject_unknown_keys(constraints, _CONSTRAINT_KEYS, f"candidate {candidate_id!r} constraints")
    normalized: dict[str, Any] = {}
    for field in ("intents", "providers", "repos", "hosts", "scopes", "forbidden_flags"):
        values = _string_list(constraints, field, f"candidate {candidate_id!r} constraints", required=False)
        if values:
            normalized[field] = values
    min_confidence = _optional_unit_interval(
        constraints, "min_confidence", f"candidate {candidate_id!r} constraints"
    )
    if min_confidence is not None:
        normalized["min_confidence"] = min_confidence
    return normalized


def _normalize_candidate(raw: Any, seen: set[str]) -> Candidate:
    entry = _require_mapping(raw, "candidate")
    _reject_unknown_keys(entry, _CANDIDATE_KEYS, "candidate")
    candidate_id = _require_str(entry, "candidate_id", "candidate")
    _require(candidate_id not in seen, f"duplicate candidate_id {candidate_id!r}")
    seen.add(candidate_id)
    kind = _require_str(entry, "kind", "candidate")
    _require(kind in CANDIDATE_KINDS, f"candidate {candidate_id!r} has unknown kind {kind!r}")
    label = _optional_str(entry, "label", "candidate") or candidate_id
    enabled = _optional_bool(entry, "enabled", "candidate")
    priority = _optional_int(entry, "priority", "candidate")
    capabilities = _string_list(entry, "capabilities", "candidate", required=False)
    constraints = entry.get("constraints")
    normalized_constraints = (
        _normalize_constraints(constraints, candidate_id) if constraints is not None else {}
    )
    unknown_forbidden = sorted(set(normalized_constraints.get("forbidden_flags", ())) - KNOWN_FLAGS)
    _require(
        not unknown_forbidden,
        f"candidate {candidate_id!r} constraints name unknown flags: " + ", ".join(unknown_forbidden),
    )
    evidence = entry.get("evidence")
    if evidence is not None:
        _require_mapping(evidence, f"candidate {candidate_id!r} evidence")
    return Candidate(
        candidate_id=candidate_id,
        kind=kind,
        label=label,
        enabled=True if enabled is None else enabled,
        priority=0 if priority is None else priority,
        capabilities=capabilities,
        constraints=normalized_constraints,
        policy_version=_optional_str(entry, "policy_version", "candidate") or "",
        evidence=evidence,
    )


def normalize_catalog(raw: Any) -> PolicyCatalog:
    """Validate a current-catalog document and return a typed :class:`PolicyCatalog`."""
    entry = _require_mapping(raw, "policy catalog")
    _reject_unknown_keys(entry, _CATALOG_KEYS, "policy catalog")
    schema_version = entry.get("schema_version")
    _require(
        schema_version == POLICY_CATALOG_SCHEMA_VERSION,
        f"policy catalog schema_version {schema_version!r} is not supported; "
        f"expected {POLICY_CATALOG_SCHEMA_VERSION!r}",
    )
    catalog_version = _require_str(entry, "catalog_version", "policy catalog")

    raw_candidates = entry.get("candidates", [])
    _require(isinstance(raw_candidates, list), "policy catalog field 'candidates' must be a list")
    seen: set[str] = set()
    candidates = tuple(_normalize_candidate(candidate, seen) for candidate in raw_candidates)
    by_id = {candidate.candidate_id: candidate for candidate in candidates}

    raw_defaults = entry.get("defaults", {})
    defaults = _require_mapping(raw_defaults, "policy catalog defaults")
    for kind, candidate_id in defaults.items():
        _require(kind in CANDIDATE_KINDS, f"policy catalog defaults name unknown kind {kind!r}")
        _require(
            isinstance(candidate_id, str) and candidate_id in by_id,
            f"policy catalog default for {kind!r} names unknown candidate {candidate_id!r}",
        )
        _require(
            by_id[candidate_id].kind == kind,
            f"policy catalog default for {kind!r} names candidate {candidate_id!r} "
            f"of kind {by_id[candidate_id].kind!r}",
        )

    return PolicyCatalog(
        catalog_version=catalog_version,
        candidates=candidates,
        defaults={str(kind): str(candidate_id) for kind, candidate_id in defaults.items()},
        generated_by=_optional_str(entry, "generated_by", "policy catalog"),
    )


def load_catalog(path: str | Path) -> PolicyCatalog:
    """Load and validate a policy catalog JSON document."""
    catalog_path = Path(path)
    try:
        text = catalog_path.read_text(encoding="utf-8")
    except OSError as exc:
        raise PolicyError(f"cannot read policy catalog {catalog_path}: {exc}") from exc
    try:
        raw = json.loads(text, parse_constant=_reject_json_constant)
    except ValueError as exc:
        raise PolicyError(f"policy catalog {catalog_path} is not valid JSON: {exc}") from exc
    return normalize_catalog(raw)


# -- bundle normalization ---------------------------------------------------


def _reject_json_constant(value: str) -> Any:
    raise ValueError(f"non-finite JSON constant {value!r} is not allowed")


def _normalize_feature(raw: Any, episode_id: str, seen: set[str]) -> Feature:
    entry = _require_mapping(raw, f"episode {episode_id!r} feature")
    _reject_unknown_keys(entry, _FEATURE_KEYS, f"episode {episode_id!r} feature")
    name = _require_str(entry, "name", f"episode {episode_id!r} feature")
    _require(
        name not in _RESERVED_FEATURE_NAMES,
        f"episode {episode_id!r} feature name {name!r} is reserved",
    )
    _require(name not in seen, f"episode {episode_id!r} duplicate feature name {name!r}")
    seen.add(name)
    value = entry.get("value")
    _require(
        isinstance(value, (str, int, float)) and not isinstance(value, bool),
        f"episode {episode_id!r} feature {name!r} value must be a string or number",
    )
    if isinstance(value, float):
        _require(math.isfinite(value), f"episode {episode_id!r} feature {name!r} must be finite")
    return Feature(
        name=name,
        value=str(value),
        available_at=_parse_timestamp(
            entry.get("available_at"), f"episode {episode_id!r} feature {name!r} available_at"
        ),
        source=_optional_str(entry, "source", f"episode {episode_id!r} feature") or "classification",
    )


def _normalize_record(raw: Any, seen: set[str]) -> ClassificationRecord:
    entry = _require_mapping(raw, "episode record")
    _reject_unknown_keys(entry, _RECORD_KEYS, "episode record")
    episode_id = _require_str(entry, "episode_id", "episode record")
    _require(episode_id not in seen, f"duplicate episode_id {episode_id!r}")
    seen.add(episode_id)

    as_of = _parse_timestamp(entry.get("as_of"), f"episode {episode_id!r} as_of")
    observed_at = _parse_timestamp(
        entry.get("observed_at"), f"episode {episode_id!r} observed_at"
    )
    raw_session = entry.get("session")
    session_key: tuple[str, str, str, str] | None = None
    if raw_session is not None:
        _require(
            isinstance(raw_session, list)
            and len(raw_session) == 4
            and all(isinstance(part, str) and part for part in raw_session),
            f"episode {episode_id!r} session must be an array of four non-empty strings",
        )
        session_key = tuple(raw_session)  # type: ignore[assignment]

    flags = _string_list(entry, "flags", "episode record", required=False)
    unknown_flags = sorted(set(flags) - KNOWN_FLAGS)
    _require(
        not unknown_flags,
        f"episode {episode_id!r} has unknown flags: " + ", ".join(unknown_flags),
    )

    raw_probabilities = entry.get("probabilities")
    probabilities: dict[str, float] | None = None
    if raw_probabilities is not None:
        mapping = _require_mapping(raw_probabilities, f"episode {episode_id!r} probabilities")
        _require(bool(mapping), f"episode {episode_id!r} probabilities must not be empty")
        probabilities = {}
        for label, value in mapping.items():
            _require(
                isinstance(label, str) and label,
                f"episode {episode_id!r} probability keys must be non-empty strings",
            )
            probabilities[label] = _finite_number(
                value, f"episode {episode_id!r} probability {label!r}"
            )
            _require(
                0.0 <= probabilities[label] <= 1.0,
                f"episode {episode_id!r} probability {label!r} must be in [0,1]",
            )

    raw_labels = entry.get("labels")
    labels: dict[str, tuple[str, ...]] = {}
    if raw_labels is not None:
        mapping = _require_mapping(raw_labels, f"episode {episode_id!r} labels")
        for facet_id, values in mapping.items():
            _require(
                isinstance(facet_id, str) and facet_id,
                f"episode {episode_id!r} label facets must be non-empty strings",
            )
            _require(
                isinstance(values, list)
                and all(isinstance(item, str) and item for item in values)
                and len(set(values)) == len(values),
                f"episode {episode_id!r} label facet {facet_id!r} must be a list of "
                "unique non-empty strings",
            )
            labels[facet_id] = tuple(values)

    raw_features = entry.get("features", [])
    _require(isinstance(raw_features, list), f"episode {episode_id!r} features must be a list")
    feature_seen: set[str] = set()
    features = tuple(
        _normalize_feature(feature, episode_id, feature_seen) for feature in raw_features
    )

    raw_routing = entry.get("current_routing", {})
    routing = _require_mapping(raw_routing, f"episode {episode_id!r} current_routing")
    current_routing: dict[str, str] = {}
    for kind, candidate_id in routing.items():
        _require(
            kind in CANDIDATE_KINDS,
            f"episode {episode_id!r} current_routing names unknown kind {kind!r}",
        )
        _require(
            isinstance(candidate_id, str) and candidate_id,
            f"episode {episode_id!r} current_routing[{kind!r}] must be a non-empty string",
        )
        current_routing[kind] = candidate_id

    return ClassificationRecord(
        episode_id=episode_id,
        as_of=as_of,
        observed_at=observed_at,
        intent=_optional_str(entry, "intent", "episode record"),
        scope=_optional_str(entry, "scope", "episode record"),
        confidence=_optional_unit_interval(entry, "confidence", "episode record"),
        probabilities=probabilities,
        flags=frozenset(flags),
        required_capabilities=_string_list(
            entry, "required_capabilities", "episode record", required=False
        ),
        provider=_optional_str(entry, "provider", "episode record"),
        repo=_optional_str(entry, "repo", "episode record"),
        host=_optional_str(entry, "host", "episode record"),
        labels=labels,
        features=features,
        outcome=_optional_str(entry, "outcome", "episode record"),
        outcome_observed_at=(
            _parse_timestamp(
                entry.get("outcome_observed_at"), f"episode {episode_id!r} outcome_observed_at"
            )
            if entry.get("outcome_observed_at") is not None
            else None
        ),
        current_routing=current_routing,
        work_item_id=_optional_str(entry, "work_item_id", "episode record"),
        evidence=(
            _require_mapping(entry["evidence"], f"episode {episode_id!r} evidence")
            if entry.get("evidence") is not None
            else None
        ),
        session_key=session_key,
    )


def normalize_recommendation_bundle(raw: Any) -> RecommendationBundle:
    """Validate a recommendation bundle and return a typed bundle."""
    entry = _require_mapping(raw, "recommendation bundle")
    _reject_unknown_keys(entry, _BUNDLE_KEYS, "recommendation bundle")
    schema_version = entry.get("schema_version")
    _require(
        schema_version == POLICY_BUNDLE_VERSION,
        f"recommendation bundle schema_version {schema_version!r} is not supported; "
        f"expected {POLICY_BUNDLE_VERSION!r}",
    )
    raw_episodes = entry.get("episodes", [])
    _require(isinstance(raw_episodes, list), "recommendation bundle field 'episodes' must be a list")
    seen: set[str] = set()
    records = tuple(_normalize_record(record, seen) for record in raw_episodes)
    return RecommendationBundle(
        records=records,
        schema_version=schema_version,
        generated_by=_optional_str(entry, "generated_by", "recommendation bundle"),
    )


def load_recommendation_bundle(path: str | Path) -> RecommendationBundle:
    """Load and validate a recommendation bundle JSON document."""
    bundle_path = Path(path)
    try:
        text = bundle_path.read_text(encoding="utf-8")
    except OSError as exc:
        raise PolicyError(f"cannot read recommendation bundle {bundle_path}: {exc}") from exc
    try:
        raw = json.loads(text, parse_constant=_reject_json_constant)
    except ValueError as exc:
        raise PolicyError(f"recommendation bundle {bundle_path} is not valid JSON: {exc}") from exc
    return normalize_recommendation_bundle(raw)




def bind_classifications_from_store(
    bundle: RecommendationBundle, store: Any
) -> RecommendationBundle:
    """Replace inline predictions with the newest session-bound Jev answer.

    The bundle remains the source of temporal and routing context (``as_of``,
    ``observed_at``, current routing, outcomes, and features). Only prediction
    fields are read from SQLite, through ``classification_sessions``; an
    unbound/missing classification becomes unknown rather than a guessed label.
    """
    records: list[ClassificationRecord] = []
    for record in bundle.records:
        if record.session_key is None:
            records.append(record)
            continue
        stored = store.latest_classification_for_session(
            record.session_key, subject_kind="session"
        )
        evidence = dict(record.evidence or {})
        if stored is None:
            evidence["store_classification"] = {"status": "unmatched"}
            records.append(
                replace(
                    record,
                    intent=None,
                    scope=None,
                    confidence=None,
                    probabilities=None,
                    evidence=evidence,
                )
            )
            continue

        answers = {
            answer["question_id"]: answer["answer"] for answer in stored.get("answers", [])
        }
        primary = answers.get("primary_intent")
        intent = primary.get("choice") if isinstance(primary, Mapping) else None
        confidence = primary.get("confidence") if isinstance(primary, Mapping) else None
        probabilities = primary.get("probabilities") if isinstance(primary, Mapping) else None
        scope_answer = answers.get("scope")
        scope = scope_answer.get("choice") if isinstance(scope_answer, Mapping) else record.scope
        labels = dict(record.labels)
        for question_id, answer in answers.items():
            choice = answer.get("choice") if isinstance(answer, Mapping) else None
            if isinstance(choice, str) and choice:
                labels[question_id] = (choice,)
        evidence["store_classification"] = {
            "classification_id": stored["classification_id"],
            "snapshot_hash": stored["snapshot_hash"],
            "binding_method": stored["binding_method"],
        }
        records.append(
            replace(
                record,
                intent=intent if isinstance(intent, str) else None,
                scope=scope if isinstance(scope, str) else None,
                confidence=(
                    float(confidence)
                    if isinstance(confidence, (int, float)) and not isinstance(confidence, bool)
                    else None
                ),
                probabilities=(
                    dict(probabilities) if isinstance(probabilities, Mapping) else None
                ),
                labels=labels,
                evidence=evidence,
            )
        )
    return replace(bundle, records=tuple(records))

# -- as-of features and temporal audit --------------------------------------


def as_of_features(record: ClassificationRecord) -> dict[str, Any]:
    """Return the feature map available at the record's decision time.

    Core classification fields are available when the classification is
    observed, and a named feature is included only when its ``available_at`` is
    at or before ``as_of``. The future completion label is never included.
    """
    available: dict[str, Any] = {
        "intent": record.intent,
        "scope": record.scope,
        "confidence": record.confidence,
        "required_capabilities": tuple(record.required_capabilities),
        "provider": record.provider,
        "repo": record.repo,
        "host": record.host,
        "flags": record.flags,
        "labels": record.labels,
    }
    for feature in record.features:
        if feature.available_at <= record.as_of:
            available[feature.name] = feature.value
    return available


def temporal_audit(record: ClassificationRecord) -> dict[str, Any]:
    """Return the strict-causality audit for one record.

    ``excluded_future_features`` names every feature that was observed after the
    decision and therefore excluded. ``leak_free`` is false only when the
    classification itself was observed after the decision: then nothing about it
    can inform intake, so every recommendation falls back to existing policy.
    """
    excluded = sorted(
        feature.name for feature in record.features if feature.available_at > record.as_of
    )
    observed_available = record.observed_at <= record.as_of
    used = [
        "intent",
        "scope",
        "confidence",
        "required_capabilities",
        "provider",
        "repo",
        "host",
        "flags",
        "labels",
    ]
    used.extend(sorted(feature.name for feature in record.features if feature.available_at <= record.as_of))
    return {
        "as_of": record.as_of,
        "observed_at": record.observed_at,
        "classification_available": observed_available,
        "excluded_future_features": excluded,
        "used_features": used,
        "outcome_observed_at": record.outcome_observed_at,
        "outcome_observed_after_decision": (
            record.outcome_observed_at is not None
            and record.outcome_observed_at > record.as_of
        ),
        "leak_free": observed_available,
    }


def _json_feature(key: str, value: Any) -> Any:
    if key == "flags":
        return sorted(value)
    if key == "labels":
        return {facet: list(labels) for facet, labels in sorted(value.items())}
    if key == "required_capabilities":
        return list(value)
    return value


def decision_inputs(record: ClassificationRecord) -> dict[str, Any]:
    """Return the JSON-safe decision inputs available at ``as_of``.

    This is exactly what the recommender is allowed to see: the classification
    fields plus named features observed at or before the decision. The future
    completion label is absent, so the report's ``decision_inputs_hash`` is
    identical for two records that differ only in their future outcome.
    """
    features = as_of_features(record)
    return {key: _json_feature(key, value) for key, value in sorted(features.items())}


# -- candidate selection ----------------------------------------------------


def _candidate_rejection(candidate: Candidate, features: Mapping[str, Any]) -> str | None:
    """Return the first constraint *candidate* fails, or ``None`` when allowed."""
    constraints = candidate.constraints

    intents = constraints.get("intents")
    if intents and features.get("intent") not in intents:
        return "intent_mismatch"

    required = set(features.get("required_capabilities") or ())
    missing = sorted(required - set(candidate.capabilities))
    if missing:
        return "capability_mismatch"

    providers = constraints.get("providers")
    if providers and features.get("provider") not in providers:
        return "provider_mismatch"

    repos = constraints.get("repos")
    if repos and features.get("repo") not in repos:
        return "repo_mismatch"

    hosts = constraints.get("hosts")
    if hosts and features.get("host") not in hosts:
        return "host_mismatch"

    scopes = constraints.get("scopes")
    if scopes and features.get("scope") not in scopes:
        return "scope_mismatch"

    min_confidence = constraints.get("min_confidence")
    if min_confidence is not None:
        confidence = features.get("confidence")
        if confidence is None or confidence < min_confidence:
            return "low_confidence"

    flags = features.get("flags") or frozenset()
    for flag in constraints.get("forbidden_flags", ()):
        if flag in flags:
            return "forbidden_flag"
    return None


def _select_candidates(
    features: Mapping[str, Any], kind: str, catalog: PolicyCatalog
) -> tuple[list[Candidate], list[dict[str, str]]]:
    """Return the allowed candidates (deterministic order) and rejected reasons."""
    allowed: list[Candidate] = []
    rejected: list[dict[str, str]] = []
    for candidate in catalog.by_kind(kind):
        if not candidate.enabled:
            rejected.append({"candidate_id": candidate.candidate_id, "reason": "disabled"})
            continue
        reason = _candidate_rejection(candidate, features)
        if reason is None:
            allowed.append(candidate)
        else:
            rejected.append({"candidate_id": candidate.candidate_id, "reason": reason})
    allowed.sort(key=lambda candidate: (-candidate.priority, candidate.candidate_id))
    return allowed, rejected


def _current_candidate(
    record: ClassificationRecord, catalog: PolicyCatalog, kind: str
) -> str | None:
    if kind in record.current_routing:
        return record.current_routing[kind]
    return catalog.defaults.get(kind)


def _current_allowed(
    current: str | None, features: Mapping[str, Any], catalog: PolicyCatalog
) -> bool | None:
    if current is None:
        return None
    candidate = catalog.by_id().get(current)
    if candidate is None or not candidate.enabled:
        return False
    return _candidate_rejection(candidate, features) is None


def _fallback(
    record: ClassificationRecord,
    catalog: PolicyCatalog,
    kind: str,
    allowed: Sequence[Candidate],
) -> tuple[str | None, str]:
    """Return the deterministic existing-policy fallback for *kind*.

    The current routing decision wins even when it does not satisfy a declared
    capability: it is still the policy that would run, and silently substituting
    a different candidate would be an applied recommendation. Only when there is
    no existing decision does an allowed catalog candidate stand in.
    """
    current = _current_candidate(record, catalog, kind)
    if current is not None:
        path = "current_routing" if kind in record.current_routing else "catalog_default"
        return current, path
    if allowed:
        return allowed[0].candidate_id, "catalog_default"
    return None, "unavailable"


def _entropy_bits(probabilities: Mapping[str, float] | None) -> float | None:
    if not probabilities:
        return None
    total = sum(value for value in probabilities.values() if value > 0)
    if total <= 0:
        return None
    return -sum(
        (value / total) * math.log2(value / total)
        for value in probabilities.values()
        if value > 0
    )


def _abstention_reason(
    record: ClassificationRecord, audit: Mapping[str, Any], config: PolicyConfig
) -> str | None:
    if not audit["leak_free"]:
        return "as_of_before_classification"
    for flag in config.gate_flags:
        if flag in record.flags:
            return FLAG_ABSTAIN_REASONS[flag]
    if record.intent is None or record.intent == "unknown":
        return "unknown_intent"
    if record.confidence is None or record.confidence < config.confidence_threshold:
        return "low_confidence"
    if config.max_entropy_bits is not None:
        entropy = _entropy_bits(record.probabilities)
        if entropy is not None and entropy > config.max_entropy_bits:
            return "high_entropy"
    return None


def _recommendation(
    record: ClassificationRecord,
    kind: str,
    *,
    decision: str,
    eligibility: str,
    eligibility_reason: str,
    reason: str,
    selected: str | None,
    current: str | None,
    current_allowed: bool | None,
    fallback_candidate: str | None,
    fallback_path: str | None,
    allowed: Sequence[Candidate],
    rejected: Sequence[Mapping[str, str]],
    audit: Mapping[str, Any],
) -> dict[str, Any]:
    confidence = record.confidence
    content: dict[str, Any] = {
        "episode_id": record.episode_id,
        "work_item_id": record.work_item_id,
        "kind": kind,
        "as_of": record.as_of,
        "decision": decision,
        "eligibility": eligibility,
        "eligibility_reason": eligibility_reason,
        "reason": reason,
        "confidence": confidence,
        "uncertainty": None if confidence is None else 1.0 - confidence,
        "entropy_bits": _entropy_bits(record.probabilities),
        "probabilities": (
            dict(sorted(record.probabilities.items())) if record.probabilities is not None else None
        ),
        "recommended_candidate": selected,
        "current_candidate": current,
        "current_candidate_allowed": current_allowed,
        "fallback_candidate": fallback_candidate,
        "fallback_path": fallback_path,
        "alternatives": [
            candidate.candidate_id
            for candidate in allowed
            if candidate.candidate_id != selected
        ],
        "rejected_candidates": [dict(item) for item in rejected],
        "disagreement": selected is not None and selected != current,
        "temporal_leak_free": audit["leak_free"],
        "excluded_future_features": list(audit["excluded_future_features"]),
        "outcome_observed_after_decision": audit["outcome_observed_after_decision"],
    }
    content["recommendation_hash"] = canonical_hash(content)
    return content


def recommendations_for_record(
    record: ClassificationRecord,
    catalog: PolicyCatalog,
    config: PolicyConfig | None = None,
) -> list[dict[str, Any]]:
    """Return the shadow recommendations for one record, one per candidate kind."""
    rules = (config or PolicyConfig()).validated()
    features = as_of_features(record)
    audit = temporal_audit(record)
    abstention = _abstention_reason(record, audit, rules)

    results: list[dict[str, Any]] = []
    for kind in CANDIDATE_KINDS:
        current = _current_candidate(record, catalog, kind)
        current_allowed = _current_allowed(current, features, catalog)
        allowed, rejected = _select_candidates(features, kind, catalog)
        if abstention is not None:
            fallback_candidate, fallback_path = _fallback(record, catalog, kind, allowed)
            results.append(
                _recommendation(
                    record,
                    kind,
                    decision="fallback",
                    eligibility="abstained",
                    eligibility_reason=abstention,
                    reason=abstention,
                    selected=None,
                    current=current,
                    current_allowed=current_allowed,
                    fallback_candidate=fallback_candidate,
                    fallback_path=fallback_path,
                    allowed=allowed,
                    rejected=rejected,
                    audit=audit,
                )
            )
            continue
        if not allowed:
            fallback_candidate, fallback_path = _fallback(record, catalog, kind, allowed)
            results.append(
                _recommendation(
                    record,
                    kind,
                    decision="fallback",
                    eligibility="ineligible",
                    eligibility_reason="no_allowed_candidate",
                    reason="no_allowed_candidate",
                    selected=None,
                    current=current,
                    current_allowed=current_allowed,
                    fallback_candidate=fallback_candidate,
                    fallback_path=fallback_path,
                    allowed=allowed,
                    rejected=rejected,
                    audit=audit,
                )
            )
            continue
        selected = allowed[0].candidate_id
        decision = "recommended" if selected != current else "agree"
        results.append(
            _recommendation(
                record,
                kind,
                decision=decision,
                eligibility="eligible",
                eligibility_reason="eligible",
                reason="selected",
                selected=selected,
                current=current,
                current_allowed=current_allowed,
                fallback_candidate=None,
                fallback_path=None,
                allowed=allowed,
                rejected=rejected,
                audit=audit,
            )
        )
    return results


# -- report -----------------------------------------------------------------


def _totals(recommendations: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    totals = {
        "recommendations": len(recommendations),
        "recommended": 0,
        "agree": 0,
        "fallback": 0,
        "eligible": 0,
        "abstained": 0,
        "ineligible": 0,
        "disagreements": 0,
    }
    for item in recommendations:
        totals[item["decision"]] = totals.get(item["decision"], 0) + 1
        totals[item["eligibility"]] = totals.get(item["eligibility"], 0) + 1
        if item["disagreement"]:
            totals["disagreements"] += 1
    return totals


def build_shadow_report(
    bundle: RecommendationBundle,
    catalog: PolicyCatalog,
    config: PolicyConfig | None = None,
    *,
    generated_by: str | None = None,
) -> dict[str, Any]:
    """Build the deterministic shadow-policy report.

    The report never modifies routing: ``shadow.executes_changes`` is ``false``
    and every recommendation is advisory. A disagreement is recorded, not
    applied.
    """
    rules = (config or PolicyConfig()).validated()
    recommendations: list[dict[str, Any]] = []
    audits: dict[str, Any] = {}
    for record in bundle.records:
        recommendations.extend(recommendations_for_record(record, catalog, rules))
        audits[record.episode_id] = temporal_audit(record)

    totals = _totals(recommendations)
    eligible_decisions = totals["recommended"] + totals["agree"]
    agreement_rate = (
        totals["agree"] / eligible_decisions if eligible_decisions else None
    )

    by_kind: dict[str, dict[str, int]] = {}
    for item in recommendations:
        entry = by_kind.setdefault(
            item["kind"],
            {"recommended": 0, "agree": 0, "fallback": 0, "disagreements": 0, "recommendations": 0},
        )
        entry["recommendations"] += 1
        entry[item["decision"]] = entry.get(item["decision"], 0) + 1
        if item["disagreement"]:
            entry["disagreements"] += 1

    disagreements = [
        {
            "episode_id": item["episode_id"],
            "kind": item["kind"],
            "current_candidate": item["current_candidate"],
            "recommended_candidate": item["recommended_candidate"],
        }
        for item in recommendations
        if item["disagreement"]
    ]
    fallback_reasons: dict[str, int] = {}
    for item in recommendations:
        if item["decision"] == "fallback":
            fallback_reasons[item["reason"]] = fallback_reasons.get(item["reason"], 0) + 1

    leak_free_episodes = sum(1 for audit in audits.values() if audit["leak_free"])
    excluded_future_features = sorted(
        {
            feature
            for audit in audits.values()
            for feature in audit["excluded_future_features"]
        }
    )
    candidates_by_kind: dict[str, int] = {}
    for candidate in catalog.candidates:
        candidates_by_kind[candidate.kind] = candidates_by_kind.get(candidate.kind, 0) + 1

    decision_inputs_hash = canonical_hash(
        {
            "catalog_hash": catalog.catalog_hash(),
            "confidence_threshold": rules.confidence_threshold,
            "gate_flags": list(rules.gate_flags),
            "max_entropy_bits": rules.max_entropy_bits,
            "records": [
                {
                    "episode_id": record.episode_id,
                    "as_of": record.as_of,
                    "inputs": decision_inputs(record),
                }
                for record in bundle.records
            ],
        }
    )

    content: dict[str, Any] = {
        "report_version": POLICY_REPORT_VERSION,
        "kind": POLICY_REPORT_KIND,
        "generated_by": generated_by or bundle.generated_by,
        "provenance": {
            "bundle_schema_version": bundle.schema_version,
            "catalog_schema_version": POLICY_CATALOG_SCHEMA_VERSION,
            "catalog_version": catalog.catalog_version,
            "catalog_hash": catalog.catalog_hash(),
            "dataset_hash": bundle.dataset_hash(),
            "episodes": len(bundle.records),
            "evaluator_version": POLICY_EVALUATOR_VERSION,
            "confidence_threshold": rules.confidence_threshold,
            "gate_flags": list(rules.gate_flags),
            "max_entropy_bits": rules.max_entropy_bits,
        },
        "catalog": {
            "catalog_version": catalog.catalog_version,
            "candidates": len(catalog.candidates),
            "enabled": sum(1 for candidate in catalog.candidates if candidate.enabled),
            "by_kind": dict(sorted(candidates_by_kind.items())),
            "defaults": dict(sorted(catalog.defaults.items())),
        },
        "recommendations": recommendations,
        "shadow": {
            "mode": "shadow",
            "executes_changes": False,
            "totals": totals,
            "by_kind": dict(sorted(by_kind.items())),
            "agreement_rate": agreement_rate,
            "disagreements": disagreements,
            "fallback_reasons": dict(sorted(fallback_reasons.items())),
            "leak_free": leak_free_episodes == len(bundle.records),
            "leak_free_episodes": leak_free_episodes,
            "episodes": len(bundle.records),
            "excluded_future_features": excluded_future_features,
            "decision_inputs_hash": decision_inputs_hash,
        },
        "missingness": {
            "episodes_without_confidence": sum(1 for record in bundle.records if record.confidence is None),
            "episodes_without_probabilities": sum(
                1 for record in bundle.records if record.probabilities is None
            ),
            "episodes_without_current_routing": sum(
                1 for record in bundle.records if not record.current_routing
            ),
            "episodes_with_excluded_future_features": sum(
                1 for audit in audits.values() if audit["excluded_future_features"]
            ),
            "episodes_not_leak_free": len(audits) - leak_free_episodes,
            "note": "Missing confidence/probabilities/routing is unknown, never a fabricated recommendation.",
        },
        "notes": [
            "Shadow recommendations are advisory only; no routing, dispatch, config or "
            "execution is modified.",
            "Only enabled candidates that satisfy their catalog/capability constraints can "
            "be recommended.",
            "Uncertain, injected, contested, changed-intent, rare, unknown or low-confidence "
            "cases fall back to existing policy.",
            "As-of features exclude inputs observed after the decision time; completion "
            "labels never inform intake.",
            "A disagreement with current routing is reported, never applied.",
        ],
    }
    content["report_hash"] = canonical_hash(content)
    return content


def recommendation_rows(report: Mapping[str, Any]) -> list[dict[str, Any]]:
    """Flatten a shadow report into store rows (payload plus queryable columns)."""
    catalog_version = report["provenance"]["catalog_version"]
    evaluator_version = report["provenance"]["evaluator_version"]
    rows: list[dict[str, Any]] = []
    for item in report["recommendations"]:
        payload = dict(item)
        rows.append(
            {
                "episode_id": item["episode_id"],
                "work_item_id": item.get("work_item_id"),
                "kind": item["kind"],
                "decision": item["decision"],
                "eligibility": item["eligibility"],
                "eligibility_reason": item["eligibility_reason"],
                "confidence": item.get("confidence"),
                "uncertainty": item.get("uncertainty"),
                "recommended_candidate": item.get("recommended_candidate"),
                "current_candidate": item.get("current_candidate"),
                "fallback_candidate": item.get("fallback_candidate"),
                "fallback_path": item.get("fallback_path"),
                "disagreement": bool(item.get("disagreement")),
                "temporal_leak_free": bool(item.get("temporal_leak_free")),
                "as_of": item["as_of"],
                "catalog_version": catalog_version,
                "evaluator_version": evaluator_version,
                "payload": payload,
            }
        )
    return rows
