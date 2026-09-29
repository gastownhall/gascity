"""Tests for immutable classification/session provenance and bound reads."""

from __future__ import annotations

import json
import os
import tempfile
import unittest

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory.canonical import session_text_snapshot_hash
from agent_observatory.errors import ObservatoryError
from agent_observatory.jev import build_request, import_response, persist_request
from agent_observatory.silver import SilverEpisode, predictions_from_store
from agent_observatory.store import ObservatoryStore
from agent_observatory import load_taxonomy


class ClassificationBindingTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.store = ObservatoryStore(os.path.join(self.tmp.name, "projection.db"))
        self.addCleanup(self.store.close)
        self.key = ("city-a", "host-a", "codex", "session-1")
        self.taxonomy = load_taxonomy()

    def _import_event(self, event_id):
        path = os.path.join(self.tmp.name, f"{event_id}.jsonl")
        support.write_jsonl(
            path,
            [support.make_record(event_id=event_id, session_id=self.key[3])],
        )
        self.store.import_jsonl(path)

    def test_request_records_exact_session_before_a_later_response(self):
        self._import_event("e1")
        snapshot = self.store.session_snapshot(self.key)
        request = build_request(
            {"summary": "fix a bug"},
            self.taxonomy,
            snapshot_hash=snapshot,
            session_key=self.key,
        )
        self.assertTrue(persist_request(self.store, request))
        stored_request = self.store.get_request(request.request_hash)
        self.assertEqual(
            tuple(stored_request[field] for field in ("city_id", "host_id", "provider", "session_id")),
            self.key,
        )

        # The response may arrive after more events have changed the live session
        # snapshot; its binding follows the immutable request identity.
        self._import_event("e2")
        imported = import_response(
            self.store,
            support.valid_response(request.body),
            request_hash=request.request_hash,
        )
        classification = self.store.latest_classification_for_session(self.key)
        self.assertEqual(classification["classification_id"], imported.classification_id)
        self.assertEqual(classification["snapshot_hash"], snapshot)
        self.assertEqual(classification["binding_method"], "request")

    def test_request_without_explicit_identity_resolves_text_namespace(self):
        self._import_event("e1")
        raw_snapshot = self.store.session_snapshot(self.key)
        text_snapshot = session_text_snapshot_hash(raw_snapshot)
        request = build_request(
            {"summary": "fix a bug"},
            self.taxonomy,
            snapshot_hash=text_snapshot,
        )
        persist_request(self.store, request)
        stored = self.store.get_request(request.request_hash)
        self.assertEqual(
            tuple(stored[field] for field in ("city_id", "host_id", "provider", "session_id")),
            self.key,
        )

    def test_replayed_classification_cannot_be_rebound_to_another_session(self):
        self._import_event("e1")
        other_key = ("city-a", "host-a", "codex", "session-2")
        other_path = support.write_jsonl(
            os.path.join(self.tmp.name, "other-session.jsonl"),
            [support.make_record(event_id="e2", session_id=other_key[3])],
        )
        self.store.import_jsonl(other_path)
        classification = {
            "subject_kind": "session",
            "snapshot_hash": "s" * 64,
            "taxonomy_version": "1.1.0",
            "question_hash": "q" * 64,
            "model_version": "jev-1.13.0",
            "request_hash": "r" * 64,
            "response_hash": "h" * 64,
            "answers": [],
        }
        classification_id, _ = self.store.save_classification(
            **classification,
            session_key=self.key,
            binding_method="explicit",
        )

        with self.assertRaises(ObservatoryError) as caught:
            self.store.save_classification(
                **classification,
                session_key=other_key,
                binding_method="explicit",
            )
        self.assertIn("already bound to a different session", str(caught.exception))
        self.assertEqual(
            self.store.classifications_for_session(self.key)[0]["classification_id"],
            classification_id,
        )
        self.assertEqual(self.store.classifications_for_session(other_key), [])

    def test_predictions_read_by_binding_after_current_snapshot_changes(self):
        self._import_event("e1")
        snapshot = self.store.session_snapshot(self.key)
        classification_id, deduplicated = self.store.save_classification(
            subject_kind="session",
            snapshot_hash=snapshot,
            taxonomy_version="1.1.0",
            question_hash="q" * 64,
            model_version="jev-1.13.0",
            request_hash="r" * 64,
            response_hash="h" * 64,
            answers=[
                {
                    "question_id": "primary_intent",
                    "question_type": "choice",
                    "answer": {
                        "choice": "bugfix",
                        "confidence": 0.9,
                        "probabilities": {"bugfix": 1.0},
                    },
                }
            ],
        )
        self.assertFalse(deduplicated)
        self._import_event("e2")
        self.assertNotEqual(self.store.session_snapshot(self.key), snapshot)

        episode = SilverEpisode(
            episode_id="episode-1",
            group_key="session:" + json.dumps(self.key, separators=(",", ":")),
            provider="codex",
            observed_at="2026-09-21T10:00:00Z",
            text="Please fix the scheduler bug",
        )
        predictions = predictions_from_store(self.store, [episode])
        self.assertEqual(len(predictions), 1)
        self.assertEqual(predictions[0].primary("primary_intent"), "bugfix")
        self.assertEqual(
            self.store.latest_classification_for_session(self.key)["classification_id"],
            classification_id,
        )


if __name__ == "__main__":
    unittest.main()
