"""Tests for the M7 shadow policy recommendations.

The suite pins the four acceptance properties:

* only current, enabled catalog candidates that satisfy their capability and
  constraint declarations can be recommended;
* as-of features exclude anything observed after the decision, and the future
  completion label never changes a historical recommendation;
* offline replay is deterministic, capability/injection cases abstain, and a
  disagreement against current routing is reported instead of applied;
* recommendations persist with eligibility, confidence/uncertainty, fallback
  path and a disagreement flag.
"""

from __future__ import annotations

import json
import os
import tempfile
import unittest

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory.errors import PolicyError
from agent_observatory.evaluation import report_json
from agent_observatory.policy import (
    CANDIDATE_KINDS,
    PolicyConfig,
    as_of_features,
    bind_classifications_from_store,
    build_shadow_report,
    load_catalog,
    load_recommendation_bundle,
    normalize_catalog,
    normalize_recommendation_bundle,
    recommendation_rows,
    temporal_audit,
)
from agent_observatory.store import ObservatoryStore

PACKAGE_ROOT = support.PACKAGE_ROOT
FIXTURES = os.path.join(PACKAGE_ROOT, "tests", "fixtures", "policy")

AS_OF = "2026-09-22T00:00:00Z"
OBSERVED = "2026-09-21T23:00:00Z"


def candidate(candidate_id, kind, **overrides):
    entry = {"candidate_id": candidate_id, "kind": kind}
    entry.update(overrides)
    return entry


def catalog(entries=None, defaults=None, **overrides):
    raw = {
        "schema_version": "1.0",
        "catalog_version": "cat-1",
        "candidates": entries or [],
        "defaults": defaults or {},
    }
    raw.update(overrides)
    return raw


def record(episode_id, **overrides):
    entry = {
        "episode_id": episode_id,
        "as_of": AS_OF,
        "observed_at": OBSERVED,
        "intent": "implementation",
        "confidence": 0.95,
    }
    entry.update(overrides)
    return entry


def bundle(records, **overrides):
    raw = {"schema_version": "1.0", "episodes": records}
    raw.update(overrides)
    return raw


SIMPLE_CATALOG = catalog(
    [
        candidate(
            "formula:standard", "formula", constraints={"intents": ["implementation", "bugfix"]}
        ),
        candidate(
            "formula:deep",
            "formula",
            priority=5,
            capabilities=["repo_write"],
            constraints={"intents": ["implementation"]},
        ),
        candidate(
            "provider:claude",
            "provider",
            capabilities=["repo_write"],
            constraints={"intents": ["implementation"]},
        ),
    ],
    defaults={"formula": "formula:standard", "provider": "provider:claude"},
)


def build(records, catalog_raw=SIMPLE_CATALOG, config=None):
    return build_shadow_report(
        normalize_recommendation_bundle(bundle(records)),
        normalize_catalog(catalog_raw),
        config,
    )


def by_episode_kind(report):
    return {(item["episode_id"], item["kind"]): item for item in report["recommendations"]}


class CatalogValidationTest(unittest.TestCase):
    def test_rejects_unknown_key(self):
        with self.assertRaises(PolicyError):
            normalize_catalog(catalog(nope=1))

    def test_rejects_bad_schema_version(self):
        with self.assertRaises(PolicyError):
            normalize_catalog(catalog(schema_version="2.0"))

    def test_rejects_unknown_candidate_kind(self):
        with self.assertRaises(PolicyError):
            normalize_catalog(catalog([candidate("x:1", "spaceship")]))

    def test_rejects_duplicate_candidate_id(self):
        with self.assertRaises(PolicyError):
            normalize_catalog(
                catalog([candidate("x:1", "formula"), candidate("x:1", "provider")])
            )

    def test_rejects_unknown_constraint_key(self):
        with self.assertRaises(PolicyError):
            normalize_catalog(
                catalog([candidate("f:1", "formula", constraints={"wat": True})])
            )

    def test_rejects_unknown_forbidden_flag(self):
        with self.assertRaises(PolicyError):
            normalize_catalog(
                catalog([candidate("f:1", "formula", constraints={"forbidden_flags": ["bogus"]})])
            )

    def test_rejects_default_naming_unknown_candidate(self):
        with self.assertRaises(PolicyError):
            normalize_catalog(catalog([], defaults={"formula": "formula:ghost"}))

    def test_rejects_default_kind_mismatch(self):
        with self.assertRaises(PolicyError):
            normalize_catalog(
                catalog([candidate("p:1", "provider")], defaults={"formula": "p:1"})
            )

    def test_catalog_hash_is_stable(self):
        first = normalize_catalog(catalog([candidate("f:1", "formula")]))
        second = normalize_catalog(catalog([candidate("f:1", "formula")]))
        self.assertEqual(first.catalog_hash(), second.catalog_hash())


class BundleValidationTest(unittest.TestCase):
    def test_rejects_unknown_key(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(bundle([record("e1")], nope=1))

    def test_rejects_bad_schema_version(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(bundle([record("e1")], schema_version="9"))

    def test_rejects_duplicate_episode_id(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(bundle([record("e1"), record("e1")]))

    def test_rejects_unknown_flag(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(bundle([record("e1", flags=["bogus"])]))

    def test_rejects_confidence_out_of_range(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(bundle([record("e1", confidence=1.5)]))

    def test_rejects_boolean_confidence(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(bundle([record("e1", confidence=True)]))

    def test_rejects_naive_timestamp(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(bundle([record("e1", as_of="2026-09-22T00:00:00")]))

    def test_rejects_unknown_current_routing_kind(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(
                bundle([record("e1", current_routing={"spaceship": "x"})])
            )

    def test_rejects_reserved_feature_name(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(
                bundle(
                    [
                        record(
                            "e1",
                            features=[{"name": "intent", "value": "bugfix", "available_at": OBSERVED}],
                        )
                    ]
                )
            )

    def test_rejects_duplicate_feature_name(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(
                bundle(
                    [
                        record(
                            "e1",
                            features=[
                                {"name": "a", "value": "1", "available_at": OBSERVED},
                                {"name": "a", "value": "2", "available_at": OBSERVED},
                            ],
                        )
                    ]
                )
            )

    def test_rejects_non_finite_probability(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(
                bundle([record("e1", probabilities={"implementation": float("nan")})])
            )

    def test_rejects_unknown_probability_key_type(self):
        with self.assertRaises(PolicyError):
            normalize_recommendation_bundle(bundle([record("e1", probabilities={1: 0.5})]))

    def test_dataset_hash_is_stable(self):
        first = normalize_recommendation_bundle(bundle([record("e1")]))
        second = normalize_recommendation_bundle(bundle([record("e1")]))
        self.assertEqual(first.dataset_hash(), second.dataset_hash())


class CapabilityConstraintTest(unittest.TestCase):
    def test_only_catalog_candidates_can_be_recommended(self):
        report = build([record("e1")])
        catalog_ids = {candidate.candidate_id for candidate in normalize_catalog(SIMPLE_CATALOG).candidates}
        for item in report["recommendations"]:
            if item["recommended_candidate"] is not None:
                self.assertIn(item["recommended_candidate"], catalog_ids)

    def test_required_capability_selects_matching_candidate(self):
        item = by_episode_kind(build([record("e1", required_capabilities=["repo_write"])]))[
            ("e1", "formula")
        ]
        # formula:standard declares no capabilities, so only formula:deep can serve it.
        self.assertEqual(item["recommended_candidate"], "formula:deep")
        self.assertEqual(item["eligibility"], "eligible")

    def test_missing_capability_falls_back(self):
        report = build([record("e1", required_capabilities=["network"])])
        item = by_episode_kind(report)[("e1", "formula")]
        self.assertEqual(item["decision"], "fallback")
        self.assertEqual(item["eligibility"], "ineligible")
        self.assertEqual(item["eligibility_reason"], "no_allowed_candidate")
        self.assertEqual(item["fallback_candidate"], "formula:standard")
        self.assertEqual(item["fallback_path"], "catalog_default")
        reasons = {entry["reason"] for entry in item["rejected_candidates"]}
        self.assertIn("capability_mismatch", reasons)

    def test_disabled_candidate_is_never_recommended(self):
        raw = catalog(
            [
                candidate("formula:on", "formula", constraints={"intents": ["implementation"]}),
                candidate(
                    "formula:off",
                    "formula",
                    enabled=False,
                    priority=99,
                    constraints={"intents": ["implementation"]},
                ),
            ]
        )
        item = by_episode_kind(build([record("e1")], catalog_raw=raw))[("e1", "formula")]
        self.assertEqual(item["recommended_candidate"], "formula:on")
        self.assertIn(
            {"candidate_id": "formula:off", "reason": "disabled"}, item["rejected_candidates"]
        )

    def test_intent_mismatch_falls_back_to_existing_policy(self):
        raw = catalog(
            [
                candidate("formula:impl", "formula", constraints={"intents": ["implementation"]}),
            ],
            defaults={"formula": "formula:impl"},
        )
        item = by_episode_kind(build([record("e1", intent="planning_spec")], catalog_raw=raw))[
            ("e1", "formula")
        ]
        self.assertEqual(item["decision"], "fallback")
        self.assertEqual(item["fallback_candidate"], "formula:impl")
        self.assertEqual(item["fallback_path"], "catalog_default")

    def test_provider_constraint_rejects_unknown_provider(self):
        raw = catalog(
            [
                candidate(
                    "provider:claude",
                    "provider",
                    constraints={"intents": ["implementation"], "providers": ["claude"]},
                )
            ],
            defaults={"provider": "provider:claude"},
        )
        item = by_episode_kind(build([record("e1", provider="dsh")], catalog_raw=raw))[
            ("e1", "provider")
        ]
        self.assertEqual(item["recommended_candidate"], None)
        self.assertIn(
            {"candidate_id": "provider:claude", "reason": "provider_mismatch"},
            item["rejected_candidates"],
        )

    def test_scope_constraint(self):
        raw = catalog(
            [
                candidate(
                    "formula:small",
                    "formula",
                    constraints={"intents": ["implementation"], "scopes": ["small"]},
                )
            ]
        )
        allowed = by_episode_kind(build([record("e1", scope="small")], catalog_raw=raw))[
            ("e1", "formula")
        ]
        rejected = by_episode_kind(build([record("e1", scope="large")], catalog_raw=raw))[
            ("e1", "formula")
        ]
        self.assertEqual(allowed["recommended_candidate"], "formula:small")
        self.assertEqual(rejected["recommended_candidate"], None)
        self.assertIn(
            {"candidate_id": "formula:small", "reason": "scope_mismatch"},
            rejected["rejected_candidates"],
        )

    def test_priority_order_is_deterministic(self):
        raw = catalog(
            [
                candidate("formula:a", "formula", priority=1, constraints={"intents": ["implementation"]}),
                candidate("formula:b", "formula", priority=9, constraints={"intents": ["implementation"]}),
                candidate("formula:c", "formula", priority=9, constraints={"intents": ["implementation"]}),
            ]
        )
        report = build([record("e1")], catalog_raw=raw)
        item = by_episode_kind(report)[("e1", "formula")]
        # Highest priority wins; equal priority breaks lexicographically.
        self.assertEqual(item["recommended_candidate"], "formula:b")
        self.assertEqual(item["alternatives"], ["formula:c", "formula:a"])

    def test_current_routing_candidate_absent_from_catalog_is_flagged(self):
        raw = catalog(
            [candidate("formula:on", "formula", constraints={"intents": ["implementation"]})],
            defaults={"formula": "formula:on"},
        )
        item = by_episode_kind(
            build([record("e1", current_routing={"formula": "formula:retired"})], catalog_raw=raw)
        )[("e1", "formula")]
        self.assertFalse(item["current_candidate_allowed"])
        self.assertTrue(item["disagreement"])
        self.assertEqual(item["current_candidate"], "formula:retired")
        self.assertEqual(item["recommended_candidate"], "formula:on")


class TemporalAsOfTest(unittest.TestCase):
    def test_future_feature_is_excluded_not_used(self):
        rec = record(
            "e1",
            features=[{"name": "completion", "value": "accepted", "available_at": "2026-09-23T00:00:00Z"}],
        )
        normalized = normalize_recommendation_bundle(bundle([rec])).records[0]
        features = as_of_features(normalized)
        audit = temporal_audit(normalized)
        self.assertNotIn("completion", features)
        self.assertEqual(audit["excluded_future_features"], ["completion"])
        self.assertTrue(audit["leak_free"])

    def test_available_feature_is_used(self):
        rec = record(
            "e1",
            features=[{"name": "compile_cache", "value": "warm", "available_at": OBSERVED}],
        )
        normalized = normalize_recommendation_bundle(bundle([rec])).records[0]
        self.assertEqual(as_of_features(normalized)["compile_cache"], "warm")

    def test_completion_label_never_changes_recommendation(self):
        base = record("e1", outcome="accepted", outcome_observed_at="2026-09-25T00:00:00Z")
        other = record("e1", outcome="rejected", outcome_observed_at="2026-09-26T00:00:00Z")
        first = build([base])
        second = build([other])
        # Decisions are identical; only the dataset provenance (which pins the
        # observed outcome for replay) differs.
        self.assertEqual(first["recommendations"], second["recommendations"])
        self.assertEqual(
            first["shadow"]["decision_inputs_hash"],
            second["shadow"]["decision_inputs_hash"],
        )

    def test_classification_after_decision_abstains(self):
        rec = record("e1", observed_at="2026-09-23T00:00:00Z")
        report = build([rec])
        item = by_episode_kind(report)[("e1", "formula")]
        self.assertEqual(item["decision"], "fallback")
        self.assertEqual(item["eligibility"], "abstained")
        self.assertEqual(item["eligibility_reason"], "as_of_before_classification")
        self.assertFalse(item["temporal_leak_free"])
        self.assertFalse(report["shadow"]["leak_free"])
        self.assertEqual(report["shadow"]["leak_free_episodes"], 0)

    def test_temporal_audit_records_outcome_after_decision(self):
        rec = record("e1", outcome="accepted", outcome_observed_at="2026-09-25T00:00:00Z")
        normalized = normalize_recommendation_bundle(bundle([rec])).records[0]
        audit = temporal_audit(normalized)
        self.assertTrue(audit["outcome_observed_after_decision"])
        self.assertTrue(audit["leak_free"])


class GateTest(unittest.TestCase):
    def test_injected_flag_abstains(self):
        item = by_episode_kind(build([record("e1", flags=["injected"])]))[("e1", "formula")]
        self.assertEqual(item["eligibility"], "abstained")
        self.assertEqual(item["eligibility_reason"], "injected_state")

    def test_uncertain_and_contested_and_changed_intent_and_rare_abstain(self):
        for flag, reason in (
            ("uncertain", "uncertain"),
            ("contested", "contested"),
            ("changed_intent", "changed_intent"),
            ("rare", "rare_class"),
        ):
            item = by_episode_kind(build([record("e1", flags=[flag])]))[("e1", "formula")]
            self.assertEqual(item["eligibility"], "abstained", flag)
            self.assertEqual(item["eligibility_reason"], reason, flag)

    def test_unknown_intent_abstains(self):
        item = by_episode_kind(build([record("e1", intent="unknown")]))[("e1", "formula")]
        self.assertEqual(item["eligibility_reason"], "unknown_intent")

    def test_missing_intent_abstains(self):
        rec = record("e1")
        rec.pop("intent")
        item = by_episode_kind(build([rec]))[("e1", "formula")]
        self.assertEqual(item["eligibility_reason"], "unknown_intent")

    def test_low_confidence_abstains(self):
        item = by_episode_kind(build([record("e1", confidence=0.5)]))[("e1", "formula")]
        self.assertEqual(item["eligibility_reason"], "low_confidence")

    def test_missing_confidence_abstains(self):
        rec = record("e1")
        rec.pop("confidence")
        item = by_episode_kind(build([rec]))[("e1", "formula")]
        self.assertEqual(item["eligibility_reason"], "low_confidence")

    def test_high_entropy_abstains_when_configured(self):
        rec = record(
            "e1",
            probabilities={"implementation": 0.5, "bugfix": 0.5},
        )
        config = PolicyConfig(max_entropy_bits=0.5)
        item = by_episode_kind(build([rec], config=config))[("e1", "formula")]
        self.assertEqual(item["eligibility_reason"], "high_entropy")
        self.assertAlmostEqual(item["entropy_bits"], 1.0)

    def test_confidence_threshold_override(self):
        item = by_episode_kind(
            build([record("e1", confidence=0.7)], config=PolicyConfig(confidence_threshold=0.6))
        )[("e1", "formula")]
        self.assertEqual(item["eligibility"], "eligible")

    def test_extra_gate_flags_have_reasons(self):
        rec = record("e1", flags=["non_english"])
        config = PolicyConfig(gate_flags=("non_english",))
        item = by_episode_kind(build([rec], config=config))[("e1", "formula")]
        self.assertEqual(item["eligibility_reason"], "non_english")

    def test_invalid_config_rejected(self):
        with self.assertRaises(PolicyError):
            PolicyConfig(confidence_threshold=2.0).validated()
        with self.assertRaises(PolicyError):
            PolicyConfig(gate_flags=("bogus",)).validated()


class ShadowReportTest(unittest.TestCase):
    def test_disagreement_is_reported_not_applied(self):
        report = build(
            [record("e1", current_routing={"formula": "formula:standard"})]
        )
        item = by_episode_kind(report)[("e1", "formula")]
        self.assertEqual(item["decision"], "recommended")
        self.assertTrue(item["disagreement"])
        self.assertEqual(report["shadow"]["disagreements"][0]["episode_id"], "e1")
        self.assertFalse(report["shadow"]["executes_changes"])
        self.assertEqual(report["shadow"]["mode"], "shadow")

    def test_agreement_when_current_matches(self):
        report = build([record("e1", current_routing={"formula": "formula:deep"})])
        item = by_episode_kind(report)[("e1", "formula")]
        self.assertEqual(item["decision"], "agree")
        self.assertFalse(item["disagreement"])

    def test_totals_and_agreement_rate(self):
        report = build([record("e1", current_routing={"formula": "formula:standard"})])
        totals = report["shadow"]["totals"]
        self.assertEqual(totals["recommendations"], len(CANDIDATE_KINDS))
        self.assertEqual(totals["recommended"], 1)
        self.assertEqual(totals["agree"], 1)
        self.assertEqual(totals["fallback"], len(CANDIDATE_KINDS) - 2)
        self.assertEqual(report["shadow"]["agreement_rate"], 0.5)

    def test_report_is_deterministic(self):
        records = [record("e1"), record("e2", confidence=0.4)]
        first = build(records)
        second = build(records)
        self.assertEqual(first["report_hash"], second["report_hash"])
        self.assertEqual(report_json(first), report_json(second))

    def test_report_hash_tracks_catalog_version(self):
        first = build([record("e1")], catalog_raw=SIMPLE_CATALOG)
        changed = json.loads(json.dumps(SIMPLE_CATALOG))
        changed["catalog_version"] = "cat-2"
        second = build([record("e1")], catalog_raw=changed)
        self.assertNotEqual(first["report_hash"], second["report_hash"])

    def test_fallback_reasons_are_counted(self):
        report = build([record("e1", confidence=0.2)])
        self.assertEqual(report["shadow"]["fallback_reasons"], {"low_confidence": len(CANDIDATE_KINDS)})

    def test_notes_state_advisory_only(self):
        report = build([record("e1")])
        self.assertTrue(any("advisory only" in note for note in report["notes"]))
        self.assertIn("provenance", report)
        self.assertIn("missingness", report)


class PersistenceTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db = os.path.join(self.tmp.name, "projection.db")

    def test_store_schema_version(self):
        with ObservatoryStore(self.db) as store:
            self.assertEqual(store.schema_version(), ObservatoryStore.SCHEMA_VERSION)

    def test_save_recommendations_is_idempotent(self):
        report = build([record("e1", current_routing={"formula": "formula:standard"})])
        rows = recommendation_rows(report)
        with ObservatoryStore(self.db) as store:
            first = store.save_recommendations(rows)
            self.assertEqual(first.inserted, len(rows))
            self.assertEqual(first.deduplicated, 0)
            self.assertEqual(store.recommendation_count(), len(rows))
            second = store.save_recommendations(rows)
            self.assertEqual(second.inserted, 0)
            self.assertEqual(second.deduplicated, len(rows))
            self.assertEqual(store.recommendation_count(), len(rows))

    def test_persisted_row_keeps_eligibility_and_disagreement(self):
        report = build([record("e1", current_routing={"formula": "formula:standard"})])
        with ObservatoryStore(self.db) as store:
            store.save_recommendations(recommendation_rows(report))
            rows = list(store.iter_recommendations())
        formula = next(row for row in rows if row["kind"] == "formula")
        self.assertEqual(formula["eligibility"], "eligible")
        self.assertTrue(formula["disagreement"])
        self.assertEqual(formula["recommended_candidate"], "formula:deep")
        self.assertEqual(formula["current_candidate"], "formula:standard")
        self.assertAlmostEqual(formula["confidence"], 0.95)
        self.assertAlmostEqual(formula["uncertainty"], 0.05)
        self.assertEqual(formula["payload"]["recommended_candidate"], "formula:deep")

    def test_persisted_fallback_row_keeps_path(self):
        report = build([record("e1", required_capabilities=["network"])])
        with ObservatoryStore(self.db) as store:
            store.save_recommendations(recommendation_rows(report))
            rows = list(store.iter_recommendations())
        formula = next(row for row in rows if row["kind"] == "formula")
        self.assertEqual(formula["decision"], "fallback")
        self.assertEqual(formula["fallback_candidate"], "formula:standard")
        self.assertEqual(formula["fallback_path"], "catalog_default")


    def test_shadow_replaces_inline_labels_from_bound_classification(self):
        key = ("city-a", "host-a", "codex", "session-1")
        path = support.write_jsonl(
            os.path.join(self.tmp.name, "shadow-session.jsonl"),
            [support.make_record(event_id="e1", session_id=key[3])],
        )
        raw_bundle = {
            "schema_version": "1.0",
            "episodes": [
                {
                    "episode_id": "bound-episode",
                    "session": list(key),
                    "as_of": AS_OF,
                    "observed_at": OBSERVED,
                    "intent": "stale-inline-label",
                    "confidence": 0.1,
                },
                {
                    "episode_id": "missing-episode",
                    "session": ["city-a", "host-a", "codex", "not-imported"],
                    "as_of": AS_OF,
                    "observed_at": OBSERVED,
                    "intent": "must-not-be-guessed",
                    "confidence": 0.99,
                },
            ],
        }
        with ObservatoryStore(self.db) as store:
            store.import_jsonl(path)
            snapshot = store.session_snapshot(key)
            store.save_classification(
                subject_kind="session",
                snapshot_hash=snapshot,
                taxonomy_version="2.0.0",
                question_hash="q" * 64,
                model_version="jev-1.13.0",
                request_hash="r" * 64,
                response_hash="a" * 64,
                answers=[
                    {
                        "question_id": "primary_intent",
                        "question_type": "choice",
                        "answer": {
                            "choice": "bugfix",
                            "confidence": 0.95,
                            "probabilities": {"bugfix": 0.95, "implementation": 0.05},
                        },
                    },
                    {
                        "question_id": "scope",
                        "question_type": "choice",
                        "answer": {"choice": "small", "confidence": 0.9},
                    },
                ],
            )
            bound = bind_classifications_from_store(
                normalize_recommendation_bundle(raw_bundle), store
            )
        records = {item.episode_id: item for item in bound.records}
        self.assertEqual(records["bound-episode"].intent, "bugfix")
        self.assertEqual(records["bound-episode"].scope, "small")
        self.assertEqual(records["bound-episode"].confidence, 0.95)
        self.assertEqual(
            records["bound-episode"].evidence["store_classification"]["binding_method"],
            "snapshot",
        )
        self.assertIsNone(records["missing-episode"].intent)
        self.assertIsNone(records["missing-episode"].confidence)
        self.assertEqual(records["missing-episode"].evidence["store_classification"]["status"], "unmatched")


class FixtureReplayTest(unittest.TestCase):
    def setUp(self):
        self.catalog = load_catalog(os.path.join(FIXTURES, "catalog.json"))
        self.bundle = load_recommendation_bundle(os.path.join(FIXTURES, "shadow_bundle.json"))
        self.report = build_shadow_report(self.bundle, self.catalog)
        self.index = by_episode_kind(self.report)

    def test_fixture_replay_is_deterministic(self):
        second = build_shadow_report(self.bundle, self.catalog)
        self.assertEqual(self.report["report_hash"], second["report_hash"])
        self.assertEqual(report_json(self.report), report_json(second))

    def test_fixture_has_one_recommendation_per_kind(self):
        self.assertEqual(
            len(self.report["recommendations"]), len(self.bundle.records) * len(CANDIDATE_KINDS)
        )

    def test_all_recommended_candidates_are_current_catalog_members(self):
        ids = {candidate.candidate_id for candidate in self.catalog.candidates}
        for item in self.report["recommendations"]:
            if item["recommended_candidate"] is not None:
                self.assertIn(item["recommended_candidate"], ids)

    def test_impl_episode_recommends_higher_priority_mechanical_formula(self):
        item = self.index[("e-impl", "formula")]
        self.assertEqual(item["decision"], "recommended")
        self.assertEqual(item["recommended_candidate"], "formula:mechanical")
        self.assertTrue(item["disagreement"])

    def test_impl_episode_excludes_future_feature(self):
        item = self.index[("e-impl", "formula")]
        self.assertEqual(item["excluded_future_features"], ["compile_cache"])
        self.assertTrue(item["temporal_leak_free"])
        self.assertTrue(item["outcome_observed_after_decision"])

    def test_capability_case_falls_back(self):
        item = self.index[("e-cap-missing", "formula")]
        self.assertEqual(item["decision"], "fallback")
        self.assertEqual(item["eligibility"], "ineligible")

    def test_injected_case_abstains(self):
        item = self.index[("e-injected", "formula")]
        self.assertEqual(item["eligibility"], "abstained")
        self.assertEqual(item["eligibility_reason"], "injected_state")

    def test_unknown_intent_abstains(self):
        item = self.index[("e-unknown", "formula")]
        self.assertEqual(item["eligibility_reason"], "unknown_intent")

    def test_future_classification_is_not_leak_free(self):
        item = self.index[("e-future-classification", "formula")]
        self.assertEqual(item["eligibility_reason"], "as_of_before_classification")
        self.assertFalse(item["temporal_leak_free"])
        self.assertFalse(self.report["shadow"]["leak_free"])
        self.assertEqual(self.report["shadow"]["leak_free_episodes"], 6)

    def test_fixture_disagreement_is_recorded(self):
        episodes = {item["episode_id"] for item in self.report["shadow"]["disagreements"]}
        self.assertIn("e-impl", episodes)
        self.assertIn("e-planning", episodes)

    def test_fixture_report_is_byte_json_serializable(self):
        payload = report_json(self.report)
        self.assertEqual(json.loads(payload)["kind"], "shadow_policy_recommendations")


if __name__ == "__main__":
    unittest.main()
