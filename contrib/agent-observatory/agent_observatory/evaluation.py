"""Classification evaluation: grouped temporal holdout, baselines, calibration.

This module turns pinned gold annotations and saved/derived predictions into a
deterministic evaluation report. It exists so a semantic classifier (Jev) is
never judged on its own self-report:

* the *gold* labels are human annotations, stored separately from predictions;
* the split is grouped and temporal so resumed/subagent sessions and related
  work items cannot leak between the tuning and holdout partitions, and the
  audit is explicit;
* deterministic metadata/title-only baselines are scored on the same holdout so
  a semantic model has to beat weak evidence, not an empty baseline;
* per-class precision/recall, macro and multi-label F1, confusion, coverage,
  abstention and calibration (Brier + expected calibration error) are recorded;
* a conservative automation gate keeps rare, uncertain, contested and
  injected-state cases report-only regardless of model confidence.

Nothing here calls the network. Metrics are descriptive: they establish
classifier quality, never orchestration benefit or causality.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

from .annotations import GoldEpisode, GoldSet
from .canonical import canonical_hash, canonical_json
from .contract import normalize_timestamp
from .errors import EvaluationError
from .jev import _probability_sum_tolerance
from .taxonomy import Taxonomy

EVALUATION_REPORT_VERSION = "1"
DEFAULT_MULTI_LABEL_FACETS = ("secondary_activity", "target")
DEFAULT_PRIMARY_FACET = "primary_intent"
_PRIMARY_RESERVED_KEYS = {"episode_id", "predictor", "confidence", "probabilities"}


@dataclass(frozen=True)
class EvaluationConfig:
    """Thresholds and split parameters for one evaluation run."""

    primary_facet: str = DEFAULT_PRIMARY_FACET
    multi_label_facets: tuple[str, ...] = DEFAULT_MULTI_LABEL_FACETS
    holdout_fraction: float = 0.4
    confidence_threshold: float = 0.9
    min_class_support: int = 2
    calibration_bins: int = 5
    z: float = 1.96

    def validated(self, taxonomy: Taxonomy) -> "EvaluationConfig":
        if not (0.0 < self.holdout_fraction < 1.0):
            raise EvaluationError("holdout_fraction must be strictly between 0 and 1")
        if not (0.0 <= self.confidence_threshold <= 1.0):
            raise EvaluationError("confidence_threshold must be in [0,1]")
        if self.min_class_support < 1:
            raise EvaluationError("min_class_support must be >= 1")
        if self.calibration_bins < 1:
            raise EvaluationError("calibration_bins must be >= 1")
        if self.primary_facet not in taxonomy.facet_by_id():
            raise EvaluationError(
                f"primary facet {self.primary_facet!r} is not declared in the taxonomy"
            )
        for facet_id in self.multi_label_facets:
            facet = taxonomy.facet_by_id().get(facet_id)
            if facet is None:
                raise EvaluationError(f"multi-label facet {facet_id!r} is not declared")
            if facet.cardinality != "many":
                raise EvaluationError(f"facet {facet_id!r} is not many-valued")
        return self


@dataclass(frozen=True)
class Prediction:
    """One predictor's labels for one episode."""

    episode_id: str
    predictor: str
    labels: Mapping[str, tuple[str, ...]]
    confidence: float | None = None
    probabilities: Mapping[str, float] | None = None
    taxonomy_version: str | None = None
    question_hash: str | None = None
    model: str | None = None

    def label_set(self, facet_id: str) -> tuple[str, ...]:
        return tuple(self.labels.get(facet_id, ()))

    def primary(self, facet_id: str = DEFAULT_PRIMARY_FACET) -> str | None:
        values = self.label_set(facet_id)
        return values[0] if values else None

    def content(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "episode_id": self.episode_id,
            "predictor": self.predictor,
            "labels": {key: list(value) for key, value in sorted(self.labels.items())},
        }
        if self.confidence is not None:
            payload["confidence"] = self.confidence
        if self.probabilities is not None:
            payload["probabilities"] = dict(sorted(self.probabilities.items()))
        return payload

    def prediction_hash(self) -> str:
        return canonical_hash(["prediction", self.content()])


@dataclass(frozen=True)
class HoldoutSplit:
    """A grouped, temporal tuning/holdout partition."""

    tuning: tuple[GoldEpisode, ...]
    holdout: tuple[GoldEpisode, ...]
    tuning_groups: tuple[str, ...]
    holdout_groups: tuple[str, ...]
    boundary_time: str | None


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise EvaluationError(message)


def _is_finite_unit_interval(value: Any) -> bool:
    """True when *value* is a real number, finite and within ``[0, 1]``.

    Guards numeric overflow so a malformed distribution (``1e999`` -> ``inf``,
    or an integer too large to convert to float) can never escape as an
    uncaught ``OverflowError``/``ValueError``.
    """
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    if isinstance(value, int):
        return 0 <= value <= 1
    return math.isfinite(value) and 0.0 <= value <= 1.0


def grouped_temporal_split(
    episodes: Sequence[GoldEpisode],
    *,
    holdout_fraction: float = 0.4,
) -> HoldoutSplit:
    """Split by continuation group and time, deterministically.

    Groups are ordered by their earliest observed time (tie-broken by group key),
    the earliest fraction becomes tuning and the latest fraction becomes holdout.
    No group ever appears on both sides.
    """
    if not (0.0 < holdout_fraction < 1.0):
        raise EvaluationError("holdout_fraction must be strictly between 0 and 1")
    if not episodes:
        return HoldoutSplit((), (), (), (), None)

    group_earliest: dict[str, str] = {}
    for episode in episodes:
        # Compare canonical UTC microseconds, not raw strings: a hand-built
        # GoldEpisode may still carry a mixed offset, where lexicographic order
        # is not chronological order.
        observed_at = normalize_timestamp(episode.observed_at)
        previous = group_earliest.get(episode.group_key)
        if previous is None or observed_at < previous:
            group_earliest[episode.group_key] = observed_at

    ordered_groups = sorted(group_earliest.items(), key=lambda item: (item[1], item[0]))
    total = len(ordered_groups)
    holdout_count = int(round(total * holdout_fraction))
    if total >= 2:
        holdout_count = max(1, min(total - 1, holdout_count))
    else:
        holdout_count = min(total, max(0, holdout_count))
    tuning_group_set = {group for group, _ in ordered_groups[: total - holdout_count]}
    holdout_group_set = {group for group, _ in ordered_groups[total - holdout_count :]}

    tuning = tuple(ep for ep in episodes if ep.group_key in tuning_group_set)
    holdout = tuple(ep for ep in episodes if ep.group_key in holdout_group_set)
    boundary_time = min((normalize_timestamp(ep.observed_at) for ep in holdout), default=None)
    return HoldoutSplit(
        tuning=tuning,
        holdout=holdout,
        tuning_groups=tuple(sorted(tuning_group_set)),
        holdout_groups=tuple(sorted(holdout_group_set)),
        boundary_time=boundary_time,
    )


def audit_split(split: HoldoutSplit) -> dict[str, Any]:
    """Return a leak-free audit of *split* (group disjointness and time order)."""
    violations: list[str] = []
    tuning_ids = {episode.episode_id for episode in split.tuning}
    holdout_ids = {episode.episode_id for episode in split.holdout}
    if tuning_ids & holdout_ids:
        violations.append("episode appears in both tuning and holdout")
    overlap = set(split.tuning_groups) & set(split.holdout_groups)
    if overlap:
        violations.append("group appears in both partitions: " + ", ".join(sorted(overlap)))
    for episode in split.tuning:
        if episode.group_key not in set(split.tuning_groups):
            violations.append(f"tuning episode {episode.episode_id} outside tuning groups")
    for episode in split.holdout:
        if episode.group_key not in set(split.holdout_groups):
            violations.append(f"holdout episode {episode.episode_id} outside holdout groups")

    temporal_order = True
    if split.tuning and split.holdout:
        latest_tuning_group_time = max(
            min(normalize_timestamp(ep.observed_at) for ep in split.tuning if ep.group_key == group)
            for group in split.tuning_groups
        )
        earliest_holdout_group_time = min(
            min(normalize_timestamp(ep.observed_at) for ep in split.holdout if ep.group_key == group)
            for group in split.holdout_groups
        )
        temporal_order = latest_tuning_group_time <= earliest_holdout_group_time
        if not temporal_order:
            violations.append("a tuning group is not earlier than every holdout group")

    return {
        "leak_free": not violations,
        "temporal_order_ok": temporal_order,
        "violations": violations,
        "tuning_episodes": len(split.tuning),
        "holdout_episodes": len(split.holdout),
        "tuning_groups": len(split.tuning_groups),
        "holdout_groups": len(split.holdout_groups),
        "boundary_time": split.boundary_time,
    }


# -- metric primitives -----------------------------------------------------


def _safe_div(numerator: float, denominator: float) -> float:
    return numerator / denominator if denominator else 0.0


def _prf(tp: int, fp: int, fn: int) -> dict[str, Any]:
    precision = _safe_div(tp, tp + fp)
    recall = _safe_div(tp, tp + fn)
    f1 = _safe_div(2 * precision * recall, precision + recall)
    return {
        "precision": precision,
        "recall": recall,
        "f1": f1,
        "support": tp + fn,
        "predicted": tp + fp,
        "true_positive": tp,
    }


def single_label_metrics(
    pairs: Sequence[tuple[str | None, str | None]],
) -> dict[str, Any]:
    """Per-class P/R/F1 plus macro/micro F1 and a confusion matrix.

    ``pairs`` are ``(gold_label, predicted_label)`` with ``None`` for an
    abstention or an unknown/missing label. Abstentions count as misses in the
    recall denominator and never inflate precision.
    """
    gold_labels = sorted({gold for gold, _ in pairs if gold is not None})
    predicted_labels = sorted({pred for _, pred in pairs if pred is not None})
    all_labels = sorted(set(gold_labels) | set(predicted_labels))
    matrix: dict[str, dict[str, int]] = {label: {} for label in all_labels}
    for gold, pred in pairs:
        gold_key = gold if gold is not None else "(missing)"
        pred_key = pred if pred is not None else "(abstain)"
        matrix.setdefault(gold_key, {})
        matrix[gold_key][pred_key] = matrix[gold_key].get(pred_key, 0) + 1

    per_class: dict[str, Any] = {}
    correct = 0
    predicted = 0
    for label in all_labels:
        tp = sum(1 for gold, pred in pairs if gold == label and pred == label)
        fp = sum(1 for gold, pred in pairs if gold != label and pred == label)
        fn = sum(1 for gold, pred in pairs if gold == label and pred != label)
        per_class[label] = _prf(tp, fp, fn)
    for gold, pred in pairs:
        if pred is not None:
            predicted += 1
        if gold is not None and gold == pred:
            correct += 1
    total = len(pairs)
    supports = [per_class[label]["support"] for label in gold_labels]
    f1_values = [per_class[label]["f1"] for label in gold_labels]
    macro_f1 = _safe_div(sum(f1_values), len(f1_values))
    micro_tp = sum(per_class[label]["true_positive"] for label in all_labels)
    micro_fp = sum(per_class[label]["predicted"] - per_class[label]["true_positive"] for label in all_labels)
    micro_fn = sum(per_class[label]["support"] - per_class[label]["true_positive"] for label in all_labels)
    return {
        "per_class": per_class,
        "labels": all_labels,
        "macro_f1": macro_f1,
        "micro_f1": _prf(micro_tp, micro_fp, micro_fn)["f1"],
        "accuracy": _safe_div(correct, total),
        "coverage": _safe_div(predicted, total),
        "abstentions": total - predicted,
        "total": total,
        "gold_class_support": dict(zip(gold_labels, supports)),
        "confusion": matrix,
    }


def multi_label_metrics(
    pairs: Sequence[tuple[Iterable[str], Iterable[str]]],
) -> dict[str, Any]:
    """Per-label and micro/macro F1 for a many-valued facet."""
    gold_sets = [set(gold) for gold, _ in pairs]
    pred_sets = [set(pred) for _, pred in pairs]
    labels = sorted({label for values in gold_sets + pred_sets for label in values})
    per_label: dict[str, Any] = {}
    for label in labels:
        tp = sum(1 for gold, pred in zip(gold_sets, pred_sets) if label in gold and label in pred)
        fp = sum(1 for gold, pred in zip(gold_sets, pred_sets) if label not in gold and label in pred)
        fn = sum(1 for gold, pred in zip(gold_sets, pred_sets) if label in gold and label not in pred)
        per_label[label] = _prf(tp, fp, fn)
    supported = [label for label in labels if per_label[label]["support"] > 0]
    macro_f1 = _safe_div(sum(per_label[label]["f1"] for label in supported), len(supported))
    micro_tp = sum(per_label[label]["true_positive"] for label in labels)
    micro_fp = sum(per_label[label]["predicted"] - per_label[label]["true_positive"] for label in labels)
    micro_fn = sum(per_label[label]["support"] - per_label[label]["true_positive"] for label in labels)
    exact = sum(1 for gold, pred in zip(gold_sets, pred_sets) if gold == pred)
    return {
        "per_label": per_label,
        "labels": labels,
        "macro_f1": macro_f1,
        "micro_f1": _prf(micro_tp, micro_fp, micro_fn)["f1"],
        "exact_match": _safe_div(exact, len(pairs)),
        "total": len(pairs),
    }


def brier_score(
    probabilities: Sequence[Mapping[str, float]],
    gold_labels: Sequence[str | None],
    labels: Sequence[str],
) -> float | None:
    """Mean multiclass Brier score over samples that carry a distribution."""
    samples: list[float] = []
    for distribution, gold in zip(probabilities, gold_labels):
        if gold is None:
            continue
        total = 0.0
        for label in labels:
            target = 1.0 if label == gold else 0.0
            total += (float(distribution.get(label, 0.0)) - target) ** 2
        samples.append(total)
    return _safe_div(sum(samples), len(samples)) if samples else None


def reliability_bins(
    confidences: Sequence[float],
    correct: Sequence[bool],
    *,
    bins: int = 5,
) -> dict[str, Any]:
    """Reliability bins and expected calibration error for a point predictor."""
    if len(confidences) != len(correct):
        raise EvaluationError("confidences and correctness must have the same length")
    buckets: list[dict[str, Any]] = []
    ece = 0.0
    total = len(confidences)
    for index in range(bins):
        low = index / bins
        high = (index + 1) / bins
        members = [
            (confidence, is_correct)
            for confidence, is_correct in zip(confidences, correct)
            if (confidence > low and confidence <= high) or (index == 0 and confidence == 0.0)
        ]
        if not members:
            buckets.append(
                {"low": low, "high": high, "count": 0, "mean_confidence": None, "accuracy": None}
            )
            continue
        mean_confidence = sum(confidence for confidence, _ in members) / len(members)
        accuracy = sum(1 for _, is_correct in members if is_correct) / len(members)
        ece += (len(members) / total) * abs(accuracy - mean_confidence)
        buckets.append(
            {
                "low": low,
                "high": high,
                "count": len(members),
                "mean_confidence": mean_confidence,
                "accuracy": accuracy,
            }
        )
    return {"bins": buckets, "expected_calibration_error": ece, "samples": total}


def wilson_lower_bound(successes: int, total: int, *, z: float = 1.96) -> float | None:
    """Lower bound of the Wilson score interval for a binomial proportion."""
    if total <= 0:
        return None
    phat = successes / total
    denominator = 1 + z * z / total
    centre = phat + z * z / (2 * total)
    margin = z * math.sqrt((phat * (1 - phat) + z * z / (4 * total)) / total)
    return max(0.0, (centre - margin) / denominator)


# -- baselines -------------------------------------------------------------


_TITLE_RULES: tuple[tuple[tuple[str, ...], str], ...] = (
    (("adversarial", "bypass", "edge case", "failure case"), "adversarial_review"),
    (("review", "audit", "inspect the diff", "critique"), "pr_review"),
    (("fix", "bug", "regression", "flaky", "failure", "defect", "repair", "broken"), "bugfix"),
    (("plan", "spec", "scope", "prd", "design", "brief", "proposal"), "planning_spec"),
    (("test", "lint", "ci", "build", "typecheck", "format", "pipeline"), "test_lint_build_ci"),
    (("worktree", "dependency", "dependencies", "dispatch", "session", "branch", "agent ops"),
     "dependency_worktree_agent_ops"),
    (("doc", "research", "investigate", "readme", "explain", "notes"), "research_docs"),
    (("implement", "feature", "add ", "support ", "create", "introduce"), "implementation"),
)


def title_only_intent(title: str | None) -> str:
    """Deterministic keyword baseline over the episode title only."""
    if not title:
        return "unknown"
    lowered = title.lower()
    for needles, label in _TITLE_RULES:
        if any(needle in lowered for needle in needles):
            return label
    return "unknown"


def metadata_only_intent(metadata: Mapping[str, Any]) -> str:
    """Deterministic baseline over explicit structured metadata only.

    Deliberately weak: it uses only recorded fields such as ``bead_kind``,
    ``tool_categories``, ``changed_paths`` and ``formula_id`` and never reads the
    title or body text.
    """
    tool_categories = {str(value) for value in metadata.get("tool_categories", [])}
    changed_paths = [str(path) for path in metadata.get("changed_paths", [])]
    bead_kind = str(metadata.get("bead_kind") or "").lower()
    if metadata.get("review_requested") or metadata.get("is_pr"):
        return "pr_review"
    if bead_kind in {"bug", "defect", "regression"}:
        return "bugfix"
    if tool_categories & {"test", "lint", "typecheck", "build", "ci"}:
        return "test_lint_build_ci"
    if tool_categories & {"package", "worktree", "dispatch"}:
        return "dependency_worktree_agent_ops"
    if changed_paths and all(
        path.startswith(("docs/", "engdocs/", "specs/", "plans/")) or path.endswith(".md")
        for path in changed_paths
    ):
        return "research_docs"
    if metadata.get("formula_id") or metadata.get("is_spec"):
        return "planning_spec"
    return "unknown"


def baseline_predictions(
    episodes: Sequence[GoldEpisode],
    *,
    kind: str,
    primary_facet: str = DEFAULT_PRIMARY_FACET,
) -> tuple[Prediction, ...]:
    """Build deterministic title-only or metadata-only predictions."""
    if kind == "title_only":
        predictor = title_only_intent
    elif kind == "metadata_only":
        predictor = metadata_only_intent
    else:
        raise EvaluationError(f"unknown baseline kind {kind!r}")
    predictions = []
    for episode in episodes:
        if kind == "title_only":
            label = title_only_intent(episode.title)
        else:
            label = metadata_only_intent(episode.metadata)
        predictions.append(
            Prediction(
                episode_id=episode.episode_id,
                predictor=kind,
                labels={primary_facet: (label,)},
            )
        )
    return tuple(predictions)


# -- prediction loading ----------------------------------------------------


def _reject_json_constant(value: str) -> Any:
    raise ValueError(f"non-finite JSON constant {value!r} is not allowed")


def load_predictions(
    path: str | Path,
    taxonomy: Taxonomy | None = None,
    *,
    primary_facet: str = DEFAULT_PRIMARY_FACET,
) -> tuple[Prediction, ...]:
    """Load a prediction document.

    Each entry names ``episode_id`` and any facet id; single facets take a string
    (or one-element list), many-valued facets take a list. ``confidence`` and
    ``probabilities`` are optional and apply to *primary_facet*.

    ``probabilities`` is validated the way the saved-response path validates a
    choice distribution: every value must be a finite number in ``[0, 1]``, every
    key must be a label of *primary_facet*, and the sum error must fit the same
    option-count-aware tolerance. Out-of-range, non-finite, non-numeric, or
    mislabelled values raise :class:`EvaluationError` instead of reaching the
    Brier score or the canonical serializer.
    """
    prediction_path = Path(path)
    try:
        raw = json.loads(prediction_path.read_text(encoding="utf-8"), parse_constant=_reject_json_constant)
    except OSError as exc:
        raise EvaluationError(f"cannot read predictions {prediction_path}: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise EvaluationError(f"predictions {prediction_path} are not valid JSON: {exc}") from exc
    except ValueError as exc:
        raise EvaluationError(f"predictions {prediction_path} contain a non-finite number: {exc}") from exc

    _require(isinstance(raw, dict), "predictions document must be an object")
    predictor = raw.get("predictor")
    _require(isinstance(predictor, str) and predictor, "predictions predictor is required")
    model = raw.get("model")
    question_hash = raw.get("question_hash")
    taxonomy_version = raw.get("taxonomy_version")
    entries = raw.get("predictions")
    _require(isinstance(entries, list) and entries, "predictions requires a non-empty predictions list")

    facets = taxonomy.facet_by_id() if taxonomy is not None else {}
    predictions: list[Prediction] = []
    for entry in entries:
        _require(isinstance(entry, dict), "each prediction must be an object")
        episode_id = entry.get("episode_id")
        _require(isinstance(episode_id, str) and episode_id, "prediction episode_id is required")
        labels: dict[str, tuple[str, ...]] = {}
        for key, value in entry.items():
            if key in _PRIMARY_RESERVED_KEYS:
                continue
            _require(
                not facets or key in facets,
                f"prediction for {episode_id!r} names unknown facet {key!r}",
            )
            if isinstance(value, str):
                values = (value,)
            elif isinstance(value, list) and all(isinstance(item, str) for item in value):
                values = tuple(value)
            else:
                raise EvaluationError(
                    f"prediction for {episode_id!r} facet {key!r} must be a string or list of strings"
                )
            if facets and key in facets:
                allowed = taxonomy.label_keys(key)
                unknown = sorted(set(values) - allowed)
                _require(
                    not unknown,
                    f"prediction for {episode_id!r} facet {key!r} has unknown labels: "
                    + ", ".join(unknown),
                )
                if facets[key].cardinality == "one":
                    _require(len(values) == 1, f"prediction {key!r} is single-valued")
            labels[key] = values
        confidence = entry.get("confidence")
        _require(
            confidence is None or (
                isinstance(confidence, (int, float))
                and not isinstance(confidence, bool)
                and math.isfinite(confidence)
                and 0.0 <= float(confidence) <= 1.0
            ),
            f"prediction for {episode_id!r} confidence must be in [0,1]",
        )
        probabilities = entry.get("probabilities")
        if probabilities is not None:
            _require(
                isinstance(probabilities, dict),
                f"prediction for {episode_id!r} probabilities must be an object",
            )
            allowed_probability_labels = (
                taxonomy.label_keys(primary_facet)
                if facets and primary_facet in facets
                else None
            )
            normalized_probabilities: dict[str, float] = {}
            probability_total = 0.0
            for label, value in probabilities.items():
                if allowed_probability_labels is not None:
                    _require(
                        label in allowed_probability_labels,
                        f"prediction for {episode_id!r} probabilities name unknown "
                        f"label {label!r}",
                    )
                _require(
                    _is_finite_unit_interval(value),
                    f"prediction for {episode_id!r} probability for {label!r} must be "
                    "a finite number in [0,1]",
                )
                normalized_probabilities[label] = float(value)
                probability_total += float(value)
            _require(
                abs(probability_total - 1.0)
                <= _probability_sum_tolerance(len(normalized_probabilities)),
                f"prediction for {episode_id!r} probabilities sum to "
                f"{probability_total!r}, not ~1",
            )
            probabilities = normalized_probabilities
        predictions.append(
            Prediction(
                episode_id=episode_id,
                predictor=predictor,
                labels=labels,
                confidence=float(confidence) if confidence is not None else None,
                probabilities=probabilities,
                taxonomy_version=taxonomy_version,
                question_hash=question_hash,
                model=model,
            )
        )
    return tuple(predictions)


def write_predictions(path: str | Path, predictions: Sequence[Prediction], *, model: str = "") -> None:
    """Write predictions in the loader's document shape (deterministic order)."""
    payload = {
        "predictor": predictions[0].predictor if predictions else "unknown",
        "model": model,
        "predictions": [
            {
                "episode_id": prediction.episode_id,
                **{
                    key: (value[0] if len(value) == 1 else list(value))
                    for key, value in sorted(prediction.labels.items())
                },
                **({"confidence": prediction.confidence} if prediction.confidence is not None else {}),
                **({"probabilities": dict(sorted(prediction.probabilities.items()))}
                   if prediction.probabilities is not None else {}),
            }
            for prediction in predictions
        ],
    }
    Path(path).write_text(canonical_json(payload), encoding="utf-8")


# -- eligibility gate and per-predictor evaluation -------------------------


def automation_eligibility(
    episode: GoldEpisode,
    prediction: Prediction | None,
    *,
    primary_facet: str,
    rare_labels: frozenset[str],
    min_class_support: int,
    confidence_threshold: float,
) -> tuple[bool, str]:
    """Return ``(eligible, reason)`` for the high-confidence automation gate.

    Rare, uncertain, contested and injected-state cases are never eligible, no
    matter how confident the prediction is. Missing/unknown predictions abstain.
    """
    if "injected" in episode.flags:
        return False, "injected_state"
    if "uncertain" in episode.flags or "contested" in episode.flags:
        return False, "uncertain"
    if "changed_intent" in episode.flags:
        return False, "changed_intent"
    if prediction is None or prediction.primary(primary_facet) is None:
        return False, "no_prediction"
    label = prediction.primary(primary_facet)
    if label == "unknown":
        return False, "unknown_label"
    if "rare" in episode.flags or label in rare_labels:
        return False, "rare_class"
    if prediction.confidence is None or prediction.confidence < confidence_threshold:
        return False, "low_confidence"
    return True, "eligible"


def evaluate_predictor(
    tuning: Sequence[GoldEpisode],
    holdout: Sequence[GoldEpisode],
    predictions: Sequence[Prediction],
    taxonomy: Taxonomy,
    config: EvaluationConfig,
) -> dict[str, Any]:
    """Evaluate one predictor on the tuning and holdout partitions."""
    by_id = {prediction.episode_id: prediction for prediction in predictions}
    gold_ids = {episode.episode_id for episode in tuning} | {
        episode.episode_id for episode in holdout
    }
    unknown_ids = sorted(set(by_id) - gold_ids)
    if unknown_ids:
        raise EvaluationError(
            "predictions name episode ids absent from the gold set: "
            + ", ".join(unknown_ids)
        )
    missing_ids = sorted(gold_ids - set(by_id))
    tuning_metrics = _partition_metrics(tuning, by_id, taxonomy, config)
    holdout_metrics = _partition_metrics(holdout, by_id, taxonomy, config)
    holdout_metrics["automation_gate"] = _automation_gate(holdout, by_id, tuning, taxonomy, config)

    calibrations = _calibration_metrics(holdout, by_id, taxonomy, config)
    result: dict[str, Any] = {
        "predictor": predictions[0].predictor if predictions else "unknown",
        "missing_predictions": {
            "count": len(missing_ids),
            "episode_ids": missing_ids,
        },
        "tuning": tuning_metrics,
        "holdout": holdout_metrics,
        "calibration": calibrations,
    }
    if predictions:
        result["model"] = predictions[0].model
        result["taxonomy_version"] = predictions[0].taxonomy_version
        result["question_hash"] = predictions[0].question_hash
    # Precision lower bounds for the primary facet, computed on the holdout.
    lower_bounds = {}
    for label, stats in holdout_metrics["primary"]["per_class"].items():
        lower_bounds[label] = wilson_lower_bound(
            stats["true_positive"], stats["predicted"], z=config.z
        )
    result["holdout"]["primary_precision_lower_bound"] = lower_bounds
    return result


def _primary_pairs(
    episodes: Sequence[GoldEpisode],
    by_id: Mapping[str, Prediction],
    primary_facet: str,
) -> list[tuple[str | None, str | None]]:
    pairs = []
    for episode in episodes:
        prediction = by_id.get(episode.episode_id)
        pairs.append(
            (
                episode.primary(primary_facet),
                prediction.primary(primary_facet) if prediction is not None else None,
            )
        )
    return pairs


def _partition_metrics(
    episodes: Sequence[GoldEpisode],
    by_id: Mapping[str, Prediction],
    taxonomy: Taxonomy,
    config: EvaluationConfig,
) -> dict[str, Any]:
    primary_pairs = _primary_pairs(episodes, by_id, config.primary_facet)
    metrics: dict[str, Any] = {
        "episodes": len(episodes),
        "primary": single_label_metrics(primary_pairs),
        "multi_label": {},
    }
    for facet_id in config.multi_label_facets:
        facet_pairs = [
            (
                episode.label_set(facet_id),
                by_id[episode.episode_id].label_set(facet_id)
                if episode.episode_id in by_id
                else (),
            )
            for episode in episodes
        ]
        metrics["multi_label"][facet_id] = multi_label_metrics(facet_pairs)
    return metrics


def _calibration_metrics(
    episodes: Sequence[GoldEpisode],
    by_id: Mapping[str, Prediction],
    taxonomy: Taxonomy,
    config: EvaluationConfig,
) -> dict[str, Any]:
    primary_facet = config.primary_facet
    labels = sorted(taxonomy.label_keys(primary_facet))
    probabilities: list[Mapping[str, float]] = []
    gold_labels: list[str | None] = []
    confidences: list[float] = []
    correct: list[bool] = []
    for episode in episodes:
        prediction = by_id.get(episode.episode_id)
        if prediction is None:
            continue
        gold = episode.primary(primary_facet)
        predicted = prediction.primary(primary_facet)
        if prediction.probabilities is not None and gold is not None and predicted is not None:
            probabilities.append(prediction.probabilities)
            gold_labels.append(gold)
        if prediction.confidence is not None and predicted is not None and gold is not None:
            confidences.append(prediction.confidence)
            correct.append(predicted == gold)
    brier = brier_score(probabilities, gold_labels, labels)
    bins = reliability_bins(confidences, correct, bins=config.calibration_bins) if confidences else None
    return {
        "brier_score": brier,
        "reliability": bins,
        "probability_samples": len(probabilities),
        "confidence_samples": len(confidences),
    }


def _automation_gate(
    holdout: Sequence[GoldEpisode],
    by_id: Mapping[str, Prediction],
    tuning: Sequence[GoldEpisode],
    taxonomy: Taxonomy,
    config: EvaluationConfig,
) -> dict[str, Any]:
    support: dict[str, int] = {label: 0 for label in taxonomy.label_keys(config.primary_facet)}
    for episode in tuning:
        label = episode.primary(config.primary_facet)
        if label is not None:
            support[label] = support.get(label, 0) + 1
    # A class unseen in tuning is report-only: its holdout precision cannot be
    # estimated, so it must never auto-route.
    rare_labels = frozenset(
        label for label, count in support.items() if count < config.min_class_support
    )
    eligible: list[str] = []
    abstained: list[dict[str, str]] = []
    violations: list[str] = []
    for episode in holdout:
        ok, reason = automation_eligibility(
            episode,
            by_id.get(episode.episode_id),
            primary_facet=config.primary_facet,
            rare_labels=rare_labels,
            min_class_support=config.min_class_support,
            confidence_threshold=config.confidence_threshold,
        )
        if ok:
            if episode.flags & {"injected", "uncertain", "contested", "changed_intent", "rare"}:
                violations.append(episode.episode_id)
            eligible.append(episode.episode_id)
        else:
            abstained.append({"episode_id": episode.episode_id, "reason": reason})
    reasons: dict[str, int] = {}
    for item in abstained:
        reasons[item["reason"]] = reasons.get(item["reason"], 0) + 1
    return {
        "confidence_threshold": config.confidence_threshold,
        "min_class_support": config.min_class_support,
        "rare_labels": sorted(rare_labels),
        "eligible": eligible,
        "eligible_count": len(eligible),
        "abstained": abstained,
        "abstention_reasons": reasons,
        "coverage": _safe_div(len(eligible), len(holdout)),
        "violations": violations,
    }


# -- top-level report ------------------------------------------------------


def evaluate_gold_set(
    gold_set: GoldSet,
    predictors: Mapping[str, Sequence[Prediction]],
    taxonomy: Taxonomy,
    config: EvaluationConfig | None = None,
) -> dict[str, Any]:
    """Build a deterministic evaluation report for one gold set.

    *predictors* maps a predictor name to its predictions. Deterministic
    ``title_only`` and ``metadata_only`` baselines are always added on the same
    holdout, so a semantic model is compared against weak evidence.
    """
    rules = (config or EvaluationConfig()).validated(taxonomy)
    if gold_set.taxonomy_version != taxonomy.taxonomy_version:
        raise EvaluationError(
            f"gold taxonomy {gold_set.taxonomy_version!r} does not match taxonomy "
            f"{taxonomy.taxonomy_version!r}"
        )

    split = grouped_temporal_split(gold_set.episodes, holdout_fraction=rules.holdout_fraction)
    audit = audit_split(split)

    all_predictions: dict[str, tuple[Prediction, ...]] = dict(predictors)
    # Baselines are derived from title/metadata only (never from gold labels), so
    # they can be scored on both partitions without leaking anything.
    all_partitions = split.tuning + split.holdout
    all_predictions.setdefault(
        "title_only", baseline_predictions(all_partitions, kind="title_only", primary_facet=rules.primary_facet)
    )
    all_predictions.setdefault(
        "metadata_only",
        baseline_predictions(all_partitions, kind="metadata_only", primary_facet=rules.primary_facet),
    )

    evaluations = {
        name: evaluate_predictor(split.tuning, split.holdout, predictions, taxonomy, rules)
        for name, predictions in sorted(all_predictions.items())
    }

    flag_counts: dict[str, int] = {}
    for episode in gold_set.episodes:
        for flag in sorted(episode.flags):
            flag_counts[flag] = flag_counts.get(flag, 0) + 1

    return {
        "report_version": EVALUATION_REPORT_VERSION,
        "kind": "classifier_evaluation",
        "provenance": {
            "taxonomy_version": taxonomy.taxonomy_version,
            "facet_hash": taxonomy.facet_hash(),
            "question_hash": taxonomy.question_hash(),
            "model": taxonomy.model,
            "gold_set_version": gold_set.gold_set_version,
            "gold_set_hash": gold_set.gold_set_hash(),
        },
        "gold": {
            "episodes": len(gold_set.episodes),
            "groups": len(gold_set.group_keys()),
            "flag_counts": flag_counts,
            "annotators": sorted({episode.annotator for episode in gold_set.episodes}),
            "adjudications": sorted({episode.adjudication for episode in gold_set.episodes}),
        },
        "split": audit,
        "config": {
            "primary_facet": rules.primary_facet,
            "multi_label_facets": list(rules.multi_label_facets),
            "holdout_fraction": rules.holdout_fraction,
            "confidence_threshold": rules.confidence_threshold,
            "min_class_support": rules.min_class_support,
            "calibration_bins": rules.calibration_bins,
        },
        "evaluations": evaluations,
        "notes": [
            "Metrics are descriptive classifier quality, not orchestration benefit or causality.",
            "Rare, uncertain, contested and injected-state cases are report-only and never auto-routed.",
            "The synthetic/pinned fixture proves evaluator mechanics; real per-class quality requires an independently annotated gold set.",
        ],
        "evaluation_hash": canonical_hash(
            {
                "provenance": {
                    "taxonomy_version": taxonomy.taxonomy_version,
                    "facet_hash": taxonomy.facet_hash(),
                    "question_hash": taxonomy.question_hash(),
                    "model": taxonomy.model,
                    "gold_set_version": gold_set.gold_set_version,
                    "gold_set_hash": gold_set.gold_set_hash(),
                },
                "split": audit,
                "predictions": [
                    prediction.prediction_hash()
                    for name in sorted(all_predictions)
                    for prediction in all_predictions[name]
                ],
            }
        ),
    }


def _round_floats(value: Any, digits: int = 6) -> Any:
    if isinstance(value, float):
        return round(value, digits)
    if isinstance(value, dict):
        return {key: _round_floats(item, digits) for key, item in value.items()}
    if isinstance(value, list):
        return [_round_floats(item, digits) for item in value]
    return value


def report_json(report: Mapping[str, Any]) -> str:
    """Serialize a report deterministically with stable float precision."""
    return canonical_json(_round_floats(dict(report)))
