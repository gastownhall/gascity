"""Tests for the schema-versioned, transactional SQLite projection."""

from __future__ import annotations

import json
import os
import sqlite3
import tempfile
import unittest

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory.errors import (
    ContractError,
    LabelConflictError,
    SchemaVersionError,
)
from agent_observatory.store import ObservatoryStore


class StoreImportTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db_path = os.path.join(self.tmp.name, "projection.db")

    def _write(self, name, records):
        return support.write_jsonl(os.path.join(self.tmp.name, name), records)

    def _first_timestamp(self, store):
        row = store.conn.execute(
            "SELECT first_timestamp FROM sessions WHERE city_id = 'city-a' AND "
            "host_id = 'host-a' AND provider = 'codex' AND session_id = 'session-1'"
        ).fetchone()
        return row["first_timestamp"] if row is not None else None

    def _save_classification(self, store, response_hash, answers=None):
        if answers is None:
            answers = [{"question_id": "q1", "question_type": "noul", "answer": {"noul": 0.5}}]
        return store.save_classification(
            subject_kind="session",
            snapshot_hash="s" * 64,
            taxonomy_version="1.1.0",
            question_hash="q" * 64,
            model_version="jev-1.13.0",
            request_hash="r" * 64,
            response_hash=response_hash,
            answers=answers,
        )

    def test_repeated_file_is_idempotent_and_does_not_duplicate_events(self):
        path = self._write("a.jsonl", [support.make_record(event_id="e1"), support.make_record(event_id="e2")])
        with ObservatoryStore(self.db_path) as store:
            first = store.import_jsonl(path)
            self.assertEqual(first.inserted, 2)
            second = store.import_jsonl(path)
            self.assertTrue(second.skipped_identical_file)
            self.assertEqual(store.event_count(), 2)

    def test_appended_imports_add_new_events_without_duplicating_old(self):
        path = self._write("a.jsonl", [support.make_record(event_id="e1")])
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            support.write_jsonl(path, [support.make_record(event_id="e1"), support.make_record(event_id="e2")])
            result = store.import_jsonl(path)
            self.assertEqual(result.inserted, 1)
            self.assertEqual(result.duplicates, 1)
            self.assertEqual(store.event_count(), 2)

    def test_same_events_from_a_different_path_are_deduplicated(self):
        first = self._write("a.jsonl", [support.make_record(event_id="e1")])
        second = self._write("b.jsonl", [support.make_record(event_id="e1")])
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(first)
            result = store.import_jsonl(second)
            self.assertEqual(result.inserted, 0)
            self.assertEqual(result.duplicates, 1)
            self.assertEqual(store.event_count(), 1)

    def test_same_session_different_provider_and_host_are_distinct_events(self):
        records = [
            support.make_record(event_id="e1", provider="codex", host_id="h1"),
            support.make_record(event_id="e1", provider="claude", host_id="h1"),
            support.make_record(event_id="e1", provider="codex", host_id="h2"),
        ]
        path = self._write("providers.jsonl", records)
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            self.assertEqual(store.event_count(), 3)

    def test_conflicting_event_identity_is_skipped_at_record_level(self):
        path = self._write(
            "conflict.jsonl",
            [
                support.make_record(event_id="e1", command="ls"),
                support.make_record(event_id="e2", command="pwd"),
                support.make_record(event_id="e1", command="rm -rf /"),  # conflict
            ],
        )
        with ObservatoryStore(self.db_path) as store:
            result = store.import_jsonl(path)
            self.assertEqual(result.inserted, 2)
            self.assertEqual(result.duplicates, 0)
            self.assertEqual(result.skipped_conflicts, 1)
            self.assertEqual(len(result.notes), 1)
            self.assertIn("e1", result.notes[0])
            self.assertIn(":3", result.notes[0])
            self.assertEqual(store.event_count(), 2)
            # The first occurrence wins; the conflicting payload is never stored.
            event = store.get_event(("city-a", "host-a", "codex", "session-1", "e1"))
            self.assertEqual(event["command"], "ls")

    def test_conflict_across_files_skips_only_the_conflicting_record(self):
        first = self._write("first.jsonl", [support.make_record(event_id="e1", command="ls")])
        second = self._write(
            "second.jsonl",
            [
                support.make_record(event_id="e9", command="pwd"),
                support.make_record(event_id="e1", command="rm -rf /"),
            ],
        )
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(first)
            result = store.import_jsonl(second)
            self.assertEqual(result.inserted, 1)
            self.assertEqual(result.skipped_conflicts, 1)
            self.assertEqual(len(result.notes), 1)
            self.assertEqual(store.event_count(), 2)
            self.assertIsNotNone(store.get_event(("city-a", "host-a", "codex", "session-1", "e9")))
            unchanged = store.get_event(("city-a", "host-a", "codex", "session-1", "e1"))
            self.assertEqual(unchanged["command"], "ls")

    def test_truncated_input_reports_line_and_commits_nothing(self):
        path = os.path.join(self.tmp.name, "truncated.jsonl")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(support.make_record(event_id="e1")) + "\n")
            handle.write('{"schema_version": "1.0", "city_id": "city-a"')
        with ObservatoryStore(self.db_path) as store:
            with self.assertRaises(ContractError) as caught:
                store.import_jsonl(path)
            self.assertIn(":2", str(caught.exception))
            self.assertEqual(store.event_count(), 0)

    def test_unicode_line_separators_inside_json_string_do_not_split_records(self):
        # U+2028/U+2029 are legal unescaped inside JSON strings (RFC 8259).
        # str.splitlines() would tear this single record apart and reject it.
        path = os.path.join(self.tmp.name, "u2028.jsonl")
        record = support.make_record(event_id="e1", text="before\u2028middle\u2029after")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(record, ensure_ascii=False) + "\n")
        with ObservatoryStore(self.db_path) as store:
            result = store.import_jsonl(path)
            self.assertEqual(result.inserted, 1)
            self.assertEqual(result.lines_read, 1)
            event = store.get_event(("city-a", "host-a", "codex", "session-1", "e1"))
            self.assertEqual(event["text"], "before\u2028middle\u2029after")

    def test_unicode_line_separators_keep_error_line_numbers(self):
        path = os.path.join(self.tmp.name, "u2028-bad.jsonl")
        good = support.make_record(event_id="e1", text="before\u2028after")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(good, ensure_ascii=False) + "\n")
            handle.write('{"schema_version": "1.0", "city_id": "city-a"')
        with ObservatoryStore(self.db_path) as store:
            with self.assertRaises(ContractError) as caught:
                store.import_jsonl(path)
            self.assertIn(":2", str(caught.exception))
            self.assertEqual(store.event_count(), 0)

    def test_provenance_source_file_hash_and_line_are_preserved(self):
        path = self._write("prov.jsonl", [support.make_record(event_id="e1"), support.make_record(event_id="e2")])
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            event = store.get_event(("city-a", "host-a", "codex", "session-1", "e2"))
            self.assertEqual(event["source_path"], path)
            self.assertEqual(event["source_line"], 2)
            self.assertEqual(len(event["source_sha256"]), 64)

    def test_unknown_optional_data_stays_null(self):
        path = self._write("nulls.jsonl", [support.make_record(event_id="e1")])
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            event = store.get_event(("city-a", "host-a", "codex", "session-1", "e1"))
            for column in ("title", "text", "tool_name", "exit_code", "duration_ms", "model"):
                self.assertIsNone(event[column], column)

    def test_usage_missing_versus_zero_is_distinguishable(self):
        records = [
            support.make_record(event_id="e1"),
            support.make_record(event_id="e2", usage={"input_tokens": 0}),
        ]
        path = self._write("usage.jsonl", records)
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            self.assertIsNone(store.get_usage(("city-a", "host-a", "codex", "session-1", "e1")))
            usage = store.get_usage(("city-a", "host-a", "codex", "session-1", "e2"))
            self.assertEqual(usage["input_tokens"], 0)
            self.assertIsNone(usage["output_tokens"])

    def test_schema_version_is_recorded_and_reopen_succeeds(self):
        with ObservatoryStore(self.db_path) as store:
            self.assertEqual(store.schema_version(), ObservatoryStore.SCHEMA_VERSION)
        with ObservatoryStore(self.db_path) as reopened:
            self.assertEqual(reopened.schema_version(), ObservatoryStore.SCHEMA_VERSION)

    def test_session_role_comes_from_gc_session_name_not_message_text(self):
        path = self._write(
            "roles.jsonl",
            [
                support.make_record(
                    event_id="gc-event",
                    session_id="gateway-llm-dell--dsh-luna-1-pool",
                    text="[city] role=assistant",
                ),
                support.make_record(
                    event_id="ordinary-event",
                    session_id="session-uuid",
                    text="[city] gateway-llm/mayor",
                ),
            ],
        )
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            roles = {
                row["session_id"]: row["role"]
                for row in store.conn.execute("SELECT session_id, role FROM sessions")
            }
        self.assertEqual(roles["gateway-llm-dell--dsh-luna-1-pool"], "dsh-luna pool")
        self.assertIsNone(roles["session-uuid"])

    def test_events_are_ordered_chronologically_across_offsets_and_fractions(self):
        path = self._write(
            "time.jsonl",
            [
                # Lexically this ".5Z" string sorts before the bare "Z" string,
                # but it is later in time.
                support.make_record(event_id="e1", timestamp="2026-09-21T10:00:00.5Z"),
                support.make_record(event_id="e2", timestamp="2026-09-21T10:00:00Z"),
                # Lexically this "09:" string sorts first, but -05:00 makes it
                # 14:00 UTC, the latest of the three.
                support.make_record(event_id="e3", timestamp="2026-09-21T09:00:00-05:00"),
            ],
        )
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            ordered = store.session_events(("city-a", "host-a", "codex", "session-1"))
            self.assertEqual([event["event_id"] for event in ordered], ["e2", "e1", "e3"])
            self.assertEqual([event["event_id"] for event in store.iter_events()], ["e2", "e1", "e3"])

    def test_raw_timestamp_provenance_is_stored(self):
        path = self._write(
            "raw.jsonl",
            [support.make_record(event_id="e1", timestamp="2026-09-21T12:00:00.5+02:00")],
        )
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            event = store.get_event(("city-a", "host-a", "codex", "session-1", "e1"))
            self.assertEqual(event["observed_timestamp"], "2026-09-21T12:00:00.5+02:00")
            self.assertEqual(event["timestamp"], "2026-09-21T10:00:00.500000Z")

    def test_unicode_line_separators_inside_strings_do_not_split_records(self):
        # U+2028/U+2029/U+0085 are legal unescaped inside JSON strings; splitting
        # on them (str.splitlines) would reject this valid file wholesale.
        path = os.path.join(self.tmp.name, "unicode_separators.jsonl")
        text_value = "before\u2028after\u2029more\u0085end"
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(
                json.dumps(support.make_record(event_id="e1", text=text_value), ensure_ascii=False)
                + "\n"
            )
            handle.write(json.dumps(support.make_record(event_id="e2"), ensure_ascii=False) + "\n")
        with ObservatoryStore(self.db_path) as store:
            result = store.import_jsonl(path)
            self.assertEqual(result.inserted, 2)
            self.assertEqual(store.event_count(), 2)
            event = store.get_event(("city-a", "host-a", "codex", "session-1", "e1"))
            self.assertEqual(event["text"], text_value)

    def test_skipped_file_line_count_ignores_unicode_separators(self):
        path = os.path.join(self.tmp.name, "unicode_replay.jsonl")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(
                json.dumps(support.make_record(event_id="e1", text="a\u2028b"), ensure_ascii=False)
                + "\n"
            )
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            replay = store.import_jsonl(path)
            self.assertTrue(replay.skipped_identical_file)
            self.assertEqual(replay.lines_read, 1)

    def test_malformed_line_number_is_lf_based_with_unicode_separators(self):
        path = os.path.join(self.tmp.name, "bad_unicode.jsonl")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(
                json.dumps(support.make_record(event_id="e1", text="a\u2028b"), ensure_ascii=False)
                + "\n"
            )
            handle.write('{"schema_version": "1.0",\n')
        with ObservatoryStore(self.db_path) as store:
            with self.assertRaises(ContractError) as caught:
                store.import_jsonl(path)
            self.assertIn(":2", str(caught.exception))
            self.assertEqual(store.event_count(), 0)

    def test_later_import_of_earlier_event_lowers_session_first_timestamp(self):
        later = self._write(
            "later.jsonl",
            [support.make_record(event_id="e2", timestamp="2026-09-21T10:00:00Z")],
        )
        earlier = self._write(
            "earlier.jsonl",
            [support.make_record(event_id="e1", timestamp="2026-09-21T09:00:00Z")],
        )
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(later)
            self.assertEqual(self._first_timestamp(store), "2026-09-21T10:00:00.000000Z")
            store.import_jsonl(earlier)
            self.assertEqual(self._first_timestamp(store), "2026-09-21T09:00:00.000000Z")

    def test_importing_a_later_event_does_not_raise_first_timestamp(self):
        earlier = self._write(
            "first.jsonl",
            [support.make_record(event_id="e1", timestamp="2026-09-21T09:00:00Z")],
        )
        later = self._write(
            "second.jsonl",
            [support.make_record(event_id="e2", timestamp="2026-09-21T11:00:00Z")],
        )
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(earlier)
            store.import_jsonl(later)
            self.assertEqual(self._first_timestamp(store), "2026-09-21T09:00:00.000000Z")

    def _racing_find(self, store):
        """Return a finder that misses the first lookup, simulating a UNIQUE race."""
        original = store._find_classification
        calls = {"count": 0}

        def racing_find(**kwargs):
            calls["count"] += 1
            if calls["count"] == 1:
                return None
            return original(**kwargs)

        return racing_find

    def test_save_classification_unique_race_deduplicates(self):
        with ObservatoryStore(self.db_path) as store:
            first_id, first_dedup = self._save_classification(store, "a" * 64)
            self.assertFalse(first_dedup)
            store._find_classification = self._racing_find(store)
            second_id, second_dedup = self._save_classification(store, "a" * 64)
            self.assertEqual(second_id, first_id)
            self.assertTrue(second_dedup)
            self.assertEqual(store.classification_count(), 1)

    def test_save_classification_unique_race_conflict_raises_label_conflict(self):
        with ObservatoryStore(self.db_path) as store:
            self._save_classification(store, "a" * 64)
            store._find_classification = self._racing_find(store)
            with self.assertRaises(LabelConflictError):
                self._save_classification(store, "b" * 64)
            self.assertEqual(store.classification_count(), 1)

    def test_unknown_future_schema_version_is_rejected(self):
        with ObservatoryStore(self.db_path):
            pass
        conn = sqlite3.connect(self.db_path)
        try:
            conn.execute("PRAGMA user_version = 999")
            conn.commit()
        finally:
            conn.close()
        with self.assertRaises(SchemaVersionError):
            ObservatoryStore(self.db_path)


if __name__ == "__main__":
    unittest.main()
