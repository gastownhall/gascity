"""Actual-exposure join for the optimization registry (M5).

``measurement.md`` is explicit that a merged change is not an exposed change:

* a worktree cut before a merge does not contain it;
* a develop merge is not a production deployment;
* a session that ran after a merge but whose observed revision does not contain
  the change did not use it;
* when the evidence cannot decide, exposure is ``unknown`` -- never assumed.

This module joins registered changes/activations against observed session
evidence (``commit_sha``/``model`` values from the projection plus explicitly
supplied ``session_fingerprints``) and reports one of ``exposed``/``unexposed``/
``unknown`` per change. It never invents a baseline, a price, or a causal claim:
those stay ``unknown`` and are surfaced by :func:`build_ledger`.
"""

from __future__ import annotations

from typing import Any, Iterable, Sequence

from .canonical import identity_key
from .contract import normalize_timestamp

EXPOSURE_STATUSES = ("exposed", "unexposed", "unknown")
LEDGER_VERSION = "1.0"


class CommitGraph:
    """A deterministic commit-parent graph used for ancestry decisions.

    ``contains(ancestor, descendant)`` answers "is *ancestor* reachable by
    walking parents from *descendant*". It returns ``None`` -- not ``False`` --
    when either commit is missing from the graph, because an incomplete graph
    cannot prove absence.
    """

    def __init__(self, parents: dict[str, Sequence[str]] | None = None):
        self._parents: dict[str, tuple[str, ...]] = {
            sha: tuple(parents_list) for sha, parents_list in (parents or {}).items()
        }

    def has(self, sha: str) -> bool:
        return sha in self._parents

    def ancestors(self, sha: str) -> set[str]:
        seen: set[str] = set()
        stack = [sha]
        while stack:
            current = stack.pop()
            for parent in self._parents.get(current, ()):
                if parent not in seen:
                    seen.add(parent)
                    stack.append(parent)
        return seen

    def contains(self, ancestor: str, descendant: str) -> bool | None:
        if ancestor == descendant:
            return True if self.has(ancestor) else None
        if descendant not in self._parents or ancestor not in self._parents:
            return None
        return ancestor in self.ancestors(descendant)


def session_evidence_from_store(store: Any) -> list[dict[str, Any]]:
    """Build one evidence record per session from the projection.

    Only fields the normalized contract actually carries are used. A session
    with no observed commit produces no commit evidence, which is why its
    exposure stays ``unknown`` rather than becoming a false ``unexposed``.
    """
    sessions: dict[tuple[str, str, str, str], dict[str, Any]] = {}
    enrichments = store.load_session_enrichments()
    for event in store.iter_events():
        key = (event["city_id"], event["host_id"], event["provider"], event["session_id"])
        entry = sessions.get(key)
        if entry is None:
            entry = {
                "session": list(key),
                "commit_shas": set(),
                "models": set(),
                "repo": None,
                "_repo_candidates": set(),
                "_repo_ambiguous": False,
                "host_id": key[1],
                "provider": key[2],
                "first_timestamp": event["timestamp"],
                "last_timestamp": event["timestamp"],
                "event_count": 0,
                "fingerprints": [],
                "derived": True,
            }
            sessions[key] = entry
        entry["event_count"] += 1
        if event.get("commit_sha"):
            entry["commit_shas"].add(event["commit_sha"])
        if event.get("model"):
            entry["models"].add(event["model"])
        if event.get("repo"):
            entry["_repo_candidates"].add(event["repo"])
        if event["timestamp"] < entry["first_timestamp"]:
            entry["first_timestamp"] = event["timestamp"]
        if event["timestamp"] > entry["last_timestamp"]:
            entry["last_timestamp"] = event["timestamp"]

    for key, entry in sessions.items():
        direct_repos = entry.pop("_repo_candidates")
        if len(direct_repos) == 1:
            entry["repo"] = next(iter(direct_repos))
        elif len(direct_repos) > 1:
            entry["_repo_ambiguous"] = True
        else:
            entry["repo"] = enrichments.get(key, {}).get("repo")
    return [_finalize_session(entry) for entry in sessions.values()]


def _finalize_session(entry: dict[str, Any]) -> dict[str, Any]:
    entry["commit_shas"] = sorted(entry["commit_shas"])
    entry["models"] = sorted(entry["models"])
    entry.pop("_repo_candidates", None)
    return entry


def attach_session_fingerprints(
    sessions: Iterable[dict[str, Any]],
    fingerprints: Iterable[dict[str, Any]],
) -> list[dict[str, Any]]:
    """Merge explicit fingerprint evidence into the derived session records.

    Fingerprint evidence for a session that has no imported events is still
    retained so an explicitly recorded exposure can be joined.
    """
    by_key: dict[tuple[str, ...], dict[str, Any]] = {
        tuple(session["session"]): session for session in sessions
    }
    for fingerprint in fingerprints:
        key = tuple(fingerprint["session"])
        entry = by_key.get(key)
        if entry is None:
            entry = {
                "session": list(key),
                "commit_shas": [],
                "models": [],
                "repo": None,
                "_repo_ambiguous": False,
                "host_id": key[1],
                "provider": key[2],
                "first_timestamp": None,
                "last_timestamp": None,
                "event_count": 0,
                "fingerprints": [],
                "derived": False,
            }
            by_key[key] = entry
        fingerprint_repo = fingerprint.get("repo")
        if fingerprint_repo and not entry.get("_repo_ambiguous"):
            if entry["repo"] is None:
                entry["repo"] = fingerprint_repo
            elif entry["repo"] != fingerprint_repo:
                entry["repo"] = None
                entry["_repo_ambiguous"] = True
        entry["fingerprints"].append(
            {
                "type": fingerprint["type"],
                "value": fingerprint["value"],
                "observed_at": fingerprint.get("observed_at"),
                "evidence": fingerprint.get("evidence"),
            }
        )
        if fingerprint["type"] == "commit_sha":
            entry["commit_shas"] = sorted(set(entry["commit_shas"]) | {fingerprint["value"]})
        if fingerprint["type"] == "model":
            entry["models"] = sorted(set(entry["models"]) | {fingerprint["value"]})
    sessions = list(by_key.values())
    for entry in sessions:
        entry.pop("_repo_ambiguous", None)
    return sessions


def _session_in_scope(change: dict[str, Any], session: dict[str, Any]) -> bool:
    """A session is a candidate only when its observed repo matches the change.

    A session whose events carried no ``repo`` is repo-less, so it is a candidate
    only for a repo-less change. Treating it as a candidate everywhere would let
    one shared model fingerprint produce cross-repo exposure rows and pollute
    ``candidate_sessions``/``counts``.
    """
    return change.get("repo") == session.get("repo")


def applicable_activations(
    change: dict[str, Any],
    activations: Sequence[dict[str, Any]],
    session: dict[str, Any],
) -> list[dict[str, Any]]:
    """Return the activations of *change* that cover *session*'s scope.

    ``target`` matches the session's repo, host, or provider. A missing target
    applies broadly (for example an implicit merge on the change's own repo).
    """
    result = []
    for activation in activations:
        if activation["change_id"] != change["change_id"]:
            continue
        target = activation.get("target")
        if target:
            scope = {session.get("repo"), session.get("host_id"), session.get("provider")}
            if target not in scope:
                continue
        result.append(activation)
    return result


def _normalized_or_none(value: Any) -> str | None:
    """Canonicalize a timestamp for comparison, preserving ``None``.

    Window comparisons must not compare raw strings: an offset such as
    ``...-04:00`` sorts before ``...Z`` lexically even when it is later in time.
    Both the activation bounds (already canonical from the change bundle) and
    caller-supplied session bounds are normalized here so the comparison is
    chronological.
    """
    if value is None:
        return None
    return normalize_timestamp(value)


def _activation_window_verdict(
    activation: dict[str, Any], session: dict[str, Any]
) -> str:
    """Return ``active``/``before``/``after``/``pending`` for one activation."""
    if activation.get("pending"):
        return "pending"
    activated_at = _normalized_or_none(activation.get("activated_at"))
    deactivated_at = _normalized_or_none(activation.get("deactivated_at"))
    first = _normalized_or_none(session.get("first_timestamp"))
    last = _normalized_or_none(session.get("last_timestamp"))
    if activated_at is not None and last is not None and last < activated_at:
        return "before"
    if deactivated_at is not None and first is not None and first > deactivated_at:
        return "after"
    return "active"


def _evaluate_session(
    change: dict[str, Any],
    activations: Sequence[dict[str, Any]],
    graph: CommitGraph,
    session: dict[str, Any],
) -> dict[str, Any]:
    """Return the exposure verdict for one (change, session) pair."""
    applicable = applicable_activations(change, activations, session)
    if not applicable:
        return {
            "status": "unknown",
            "reason": "no activation evidence covers this session scope",
            "evidence": [],
        }

    observed_commits = list(dict.fromkeys(session.get("commit_shas") or []))
    observed_fingerprints = list(session.get("fingerprints") or [])
    # The projection carries observed models directly; fold them in so a model
    # switch is matched by the model actually seen, not only by an explicit
    # fingerprint row.
    known_model_values = {
        item["value"] for item in observed_fingerprints if item["type"] == "model"
    }
    for model in session.get("models") or []:
        if model not in known_model_values:
            observed_fingerprints.append(
                {"type": "model", "value": model, "observed_at": None, "evidence": "projection model"}
            )
    exposed: list[dict[str, Any]] = []
    unexposed: list[dict[str, Any]] = []
    unknown: list[dict[str, Any]] = []

    # Commit-ancestry targets: an explicit commit fingerprint, or the change's own
    # merge commit for a merge activation.
    commit_targets: dict[str, dict[str, Any]] = {}
    fingerprint_targets: list[tuple[dict[str, Any], dict[str, str]]] = []
    for activation in applicable:
        fingerprint = activation.get("fingerprint")
        if activation["mechanism"] == "merge" and change.get("merge_sha"):
            commit_targets.setdefault(
                change["merge_sha"], {"mechanism": "merge", "activation_id": activation["activation_id"]}
            )
        if fingerprint and fingerprint["type"] == "commit_sha":
            commit_targets.setdefault(
                fingerprint["value"],
                {"mechanism": activation["mechanism"], "activation_id": activation["activation_id"]},
            )
        elif fingerprint:
            fingerprint_targets.append((activation, fingerprint))
        elif change.get("artifact_digest") and activation["mechanism"] != "merge":
            fingerprint_targets.append(
                (activation, {"type": "artifact_digest", "value": change["artifact_digest"]})
            )

    for target_sha, target_meta in commit_targets.items():
        window = _window_for_activation_id(applicable, target_meta["activation_id"], session)
        if window == "pending":
            unexposed.append(_evidence("pending_activation", target_meta, target_sha))
            continue
        if window == "before":
            unexposed.append(_evidence("before_activation", target_meta, target_sha))
            continue
        if window == "after":
            unexposed.append(_evidence("after_deactivation", target_meta, target_sha))
            continue
        if not observed_commits:
            unknown.append(_evidence("no_commit_evidence", target_meta, target_sha))
            continue
        session_had_decision = False
        for observed in observed_commits:
            if observed == target_sha:
                exposed.append(_evidence("exact_commit", target_meta, target_sha, observed))
                break
            verdict = graph.contains(target_sha, observed)
            if verdict is True:
                exposed.append(_evidence("commit_ancestry", target_meta, target_sha, observed))
                break
            if verdict is False:
                session_had_decision = True
            else:
                unknown.append(_evidence("ancestry_undecidable", target_meta, target_sha, observed))
        else:
            # The loop completed without an exposure. Only declare ``unexposed``
            # when every observed commit was decided *and* nothing was
            # undecidable: one unknown commit could still contain the change.
            if session_had_decision and not unknown:
                unexposed.append(_evidence("commit_does_not_contain", target_meta, target_sha))
            elif not unknown:
                unknown.append(_evidence("ancestry_undecidable_all", target_meta, target_sha))

    for activation, fingerprint in fingerprint_targets:
        window = _activation_window_verdict(activation, session)
        target_meta = {"mechanism": activation["mechanism"], "activation_id": activation["activation_id"]}
        matching = [
            item
            for item in observed_fingerprints
            if item["type"] == fingerprint["type"] and item["value"] == fingerprint["value"]
        ]
        # Precedence (F4): a matching fingerprint is direct evidence that the
        # change was actually in use. When the window says ``pending`` or
        # ``before``, the window and the evidence contradict each other, so the
        # evidence cannot decide: report ``unknown`` instead of silently
        # declaring ``unexposed`` and under-counting real exposure. Absent a
        # match the window still stands (a mismatch agrees with non-use).
        if matching and window in ("pending", "before"):
            unknown.append(
                _evidence(
                    "evidence_conflicts_window", target_meta, fingerprint["value"], fingerprint["type"]
                )
            )
            continue
        if window == "pending":
            unexposed.append(_evidence("pending_activation", target_meta, fingerprint["value"]))
            continue
        if window == "before":
            unexposed.append(_evidence("before_activation", target_meta, fingerprint["value"]))
            continue
        if window == "after":
            unexposed.append(_evidence("after_deactivation", target_meta, fingerprint["value"]))
            continue
        if matching:
            exposed.append(
                _evidence("fingerprint_match", target_meta, fingerprint["value"], fingerprint["type"])
            )
            continue
        conflicting = [
            item for item in observed_fingerprints if item["type"] == fingerprint["type"]
        ]
        if conflicting:
            unexposed.append(
                _evidence("fingerprint_mismatch", target_meta, fingerprint["value"], fingerprint["type"])
            )
        else:
            unknown.append(
                _evidence("fingerprint_unobserved", target_meta, fingerprint["value"], fingerprint["type"])
            )

    if exposed:
        return {"status": "exposed", "reason": exposed[0]["verdict"], "evidence": exposed}
    if unknown:
        return {"status": "unknown", "reason": unknown[0]["verdict"], "evidence": unknown}
    if unexposed:
        return {"status": "unexposed", "reason": unexposed[0]["verdict"], "evidence": unexposed}
    return {
        "status": "unknown",
        "reason": "activation present but produced no decision",
        "evidence": [],
    }


def _window_for_activation_id(
    activations: Sequence[dict[str, Any]], activation_id: str, session: dict[str, Any]
) -> str:
    for activation in activations:
        if activation["activation_id"] == activation_id:
            return _activation_window_verdict(activation, session)
    return "active"


def _evidence(
    verdict: str,
    target_meta: dict[str, Any],
    value: str,
    observed: str | None = None,
) -> dict[str, Any]:
    item: dict[str, Any] = {
        "verdict": verdict,
        "mechanism": target_meta.get("mechanism"),
        "activation_id": target_meta.get("activation_id"),
        "fingerprint_value": value,
    }
    if observed is not None:
        item["observed"] = observed
    return item


def evaluate_change(
    change: dict[str, Any],
    activations: Sequence[dict[str, Any]],
    *,
    graph: CommitGraph,
    sessions: Sequence[dict[str, Any]],
) -> dict[str, Any]:
    """Return the exposure rollup for one change across all candidate sessions."""
    rows: list[dict[str, Any]] = []
    for session in sessions:
        if not _session_in_scope(change, session):
            continue
        verdict = _evaluate_session(change, activations, graph, session)
        rows.append({"session": list(session["session"]), **verdict})

    rows.sort(key=lambda row: identity_key(row["session"]))
    counts = {status: 0 for status in EXPOSURE_STATUSES}
    for row in rows:
        counts[row["status"]] += 1

    if counts["exposed"]:
        status = "exposed"
    elif counts["unknown"]:
        status = "unknown"
    elif rows:
        status = "unexposed"
    else:
        status = "unknown"

    return {
        "status": status,
        "candidate_sessions": len(rows),
        "no_candidates": not rows,
        "counts": counts,
        "rows": rows,
        "exposed_sessions": [row["session"] for row in rows if row["status"] == "exposed"],
        "unexposed_sessions": [row["session"] for row in rows if row["status"] == "unexposed"],
        "unknown_sessions": [row["session"] for row in rows if row["status"] == "unknown"],
    }


def baseline_status(change: dict[str, Any]) -> str:
    """``present`` only when a complete baseline was explicitly supplied."""
    baseline = change.get("baseline")
    if not isinstance(baseline, dict) or not baseline:
        return "unknown"
    if baseline.get("metric") and baseline.get("value") is not None:
        return "present"
    return "unknown"


def price_status(change: dict[str, Any]) -> str:
    """``present`` only when an explicit price map was supplied.

    Prices are never synthesized: a missing price stays ``unknown`` and cannot
    be spent against (see the transport budget rules).
    """
    prices = change.get("prices")
    return "present" if isinstance(prices, dict) and bool(prices) else "unknown"


def build_ledger(
    changes: Sequence[dict[str, Any]],
    activations: Sequence[dict[str, Any]],
    sessions: Sequence[dict[str, Any]],
    *,
    graph: CommitGraph | None = None,
    generated_by: str | None = None,
) -> dict[str, Any]:
    """Build the deterministic optimization ledger.

    Every screened change appears in ``changes`` -- including non-optimization
    and unclassified ones -- so the screening denominator is never silently
    reduced. Only ``optimization`` changes are registered interventions and only
    those contribute to ``exposure_totals``.
    """
    graph = graph or CommitGraph()
    ordered_changes = sorted(changes, key=lambda change: change["change_id"])
    by_change: dict[str, list[dict[str, Any]]] = {}
    for activation in activations:
        by_change.setdefault(activation["change_id"], []).append(activation)

    entries: list[dict[str, Any]] = []
    screening_counts = {"optimization": 0, "non_optimization": 0, "unknown": 0}
    by_kind: dict[str, int] = {}
    by_repo: dict[str, int] = {}
    exposure_totals = {"exposed": 0, "unexposed": 0, "unknown": 0}
    baseline_unknown = 0
    price_unknown = 0

    for change in ordered_changes:
        screening = change.get("classification") or "unknown"
        screening_counts[screening] = screening_counts.get(screening, 0) + 1
        by_kind[change["kind"]] = by_kind.get(change["kind"], 0) + 1
        by_repo[change["repo"]] = by_repo.get(change["repo"], 0) + 1
        baseline = baseline_status(change)
        price = price_status(change)
        if baseline == "unknown":
            baseline_unknown += 1
        if price == "unknown":
            price_unknown += 1

        registered = screening == "optimization"
        entry: dict[str, Any] = {
            "change_id": change["change_id"],
            "repo": change["repo"],
            "kind": change["kind"],
            "pr": change.get("pr"),
            "title": change.get("title"),
            "base_sha": change.get("base_sha"),
            "head_sha": change.get("head_sha"),
            "merge_sha": change.get("merge_sha"),
            "merged_at": change.get("merged_at"),
            "artifact_digest": change.get("artifact_digest"),
            "classification": screening,
            "categories": list(change.get("categories") or []),
            "classification_evidence": list(change.get("classification_evidence") or []),
            "baseline": baseline,
            "price": price,
            "registered_intervention": registered,
            "activations": [
                {
                    "activation_id": activation["activation_id"],
                    "mechanism": activation["mechanism"],
                    "target": activation.get("target"),
                    "activated_at": activation.get("activated_at"),
                    "deactivated_at": activation.get("deactivated_at"),
                    "pending": bool(activation.get("pending")),
                    "fingerprint": activation.get("fingerprint"),
                    "status": "pending" if activation.get("pending") else "active",
                }
                for activation in sorted(
                    by_change.get(change["change_id"], []),
                    key=lambda item: item["activation_id"],
                )
            ],
        }
        if registered:
            rollup = evaluate_change(
                change,
                by_change.get(change["change_id"], []),
                graph=graph,
                sessions=sessions,
            )
            entry["exposure"] = rollup
            exposure_totals[rollup["status"]] = exposure_totals.get(rollup["status"], 0) + 1

        entries.append(entry)

    screened = len(entries)
    return {
        "ledger_version": LEDGER_VERSION,
        "generated_by": generated_by,
        "screening": {
            "screened": screened,
            "optimization": screening_counts.get("optimization", 0),
            "non_optimization": screening_counts.get("non_optimization", 0),
            "unknown": screening_counts.get("unknown", 0),
            "by_kind": dict(sorted(by_kind.items())),
            "by_repo": dict(sorted(by_repo.items())),
        },
        "denominator": {
            "screened_total": screened,
            "registered_interventions": screening_counts.get("optimization", 0),
            "non_optimization_retained": screening_counts.get("non_optimization", 0),
            "unknown_retained": screening_counts.get("unknown", 0),
            "note": (
                "Every screened change remains in the denominator; non-optimization "
                "and unclassified changes are never dropped."
            ),
        },
        "exposure_totals": {
            "exposed": exposure_totals.get("exposed", 0),
            "unexposed": exposure_totals.get("unexposed", 0),
            "unknown": exposure_totals.get("unknown", 0),
            "note": "Totals cover registered optimization interventions only; a merge alone is not exposure.",
        },
        "missingness": {
            "baseline_unknown": baseline_unknown,
            "price_unknown": price_unknown,
            "note": "Missing baselines and prices remain unknown; they are never synthesized.",
        },
        "changes": entries,
        "notes": [
            "A merged change is not an exposed change: exposure requires observed commit/fingerprint evidence.",
            "Ambiguous exposure is reported as unknown, never as exposed or unexposed.",
            "This ledger is registry evidence; it does not estimate a causal effect or a dollar benefit.",
        ],
    }


__all__ = [
    "CommitGraph",
    "EXPOSURE_STATUSES",
    "LEDGER_VERSION",
    "applicable_activations",
    "attach_session_fingerprints",
    "baseline_status",
    "build_ledger",
    "evaluate_change",
    "price_status",
    "session_evidence_from_store",
]
