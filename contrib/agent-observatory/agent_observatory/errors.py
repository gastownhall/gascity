"""Error types for the agent observatory.

Every error carries enough context to point at the exact input that failed
(source path and, where relevant, 1-based line number) so callers never have to
guess which record was bad.
"""

from __future__ import annotations


class ObservatoryError(Exception):
    """Base class for all observatory errors."""


class ContractError(ObservatoryError):
    """A normalized record violates the import contract."""

    def __init__(self, message: str, source_path: str | None = None, line_number: int | None = None):
        self.source_path = source_path
        self.line_number = line_number
        location = ""
        if source_path is not None and line_number is not None:
            location = f"{source_path}:{line_number}: "
        elif source_path is not None:
            location = f"{source_path}: "
        super().__init__(location + message)


class ImportConflictError(ObservatoryError):
    """Retained for callers; no longer raised by import."""


class SchemaVersionError(ObservatoryError):
    """The SQLite schema version is missing, unknown, or incompatible."""


class TaxonomyError(ObservatoryError):
    """The question taxonomy is missing or malformed."""


class RequestError(ObservatoryError):
    """A Jev request could not be built."""


class RequestByteCapExceeded(RequestError):
    """The serialized request exceeds the conservative UTF-8 byte cap."""

    def __init__(self, actual_bytes: int, cap_bytes: int):
        self.actual_bytes = actual_bytes
        self.cap_bytes = cap_bytes
        super().__init__(
            f"serialized request is {actual_bytes} UTF-8 bytes, exceeding the "
            f"{cap_bytes}-byte safety cap (byte cap is not tokenizer proof)"
        )


class ResponseError(ObservatoryError):
    """A saved Jev response is invalid or does not match its request."""


class LabelConflictError(ObservatoryError):
    """A classification already exists for this subject and would be overwritten."""


class AnnotationError(ObservatoryError):
    """A gold annotation set or annotation record is invalid."""


class PricingError(ObservatoryError):
    """A checked-in model price or pricing-seed document is invalid."""



class EpisodeError(ObservatoryError):
    """Episode segmentation or split input is invalid."""


class EvaluationError(ObservatoryError):
    """An evaluation request is inconsistent (for example gold/prediction mismatch)."""


class RegistryError(ObservatoryError):
    """A change/exposure registry input is missing or malformed.

    The registry input is an explicit, versioned JSON bundle (see
    :mod:`agent_observatory.changes`); there is no live PR crawler.
    """


class RegistryConflictError(RegistryError):
    """An existing immutable change/activation would be silently overwritten.

    Changes and their activation records are append-only evidence: importing the
    same identity with different content is a conflict, not an update.
    """


class ImpactError(ObservatoryError):
    """An accepted-task impact bundle or report request is missing or malformed.

    The impact input is an explicit, versioned JSON bundle (see
    :mod:`agent_observatory.impact`); the report never fabricates an accepted
    task, a baseline, a price or a matched control.
    """


class PolicyError(ObservatoryError):
    """A shadow-policy catalog or recommendation bundle is missing or malformed.

    The policy input is an explicit, versioned JSON catalog plus a bundle of
    validated classifications (see :mod:`agent_observatory.policy`). Shadow
    recommendations are advisory only: the module never writes routing,
    dispatch or configuration, and only candidates present in the supplied
    catalog can be recommended.
    """


class CanaryError(ObservatoryError):
    """A canary registration, assignment bundle, or request cap is invalid.

    The canary input is an explicit, pre-registered policy artifact plus a
    bundle of units (see :mod:`agent_observatory.canary`). A canary is opt-in
    and defaults to the prior policy; the module writes no routing, dispatch or
    configuration, and a kill switch restores the prior policy immediately.
    """


class SilverError(ObservatoryError):
    """A silver-set candidate, judge prompt, answer, or report is invalid.

    Silver labels are machine-built from two independent LLM judges (see
    :mod:`agent_observatory.silver`); they are a reference, never human ground
    truth, and a judge answer that cannot be validated is an error rather than a
    silently coerced ``unknown``.
    """
