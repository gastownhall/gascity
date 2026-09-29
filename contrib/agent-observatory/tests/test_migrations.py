"""Tests for explicit, backed-up schema-4/5/6 migrations through schema 7."""

from __future__ import annotations

import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory import migrations
from agent_observatory.errors import ObservatoryError, SchemaVersionError
from agent_observatory.canonical import session_text_snapshot_hash, sha256_bytes
from agent_observatory.migrations import _V6_REQUIRED_TABLES, migrate_database
from agent_observatory.store import ObservatoryStore


class MigrationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db_path = os.path.join(self.tmp.name, "obs.db")

    def _make_schema6_fixture(self, db_path=None):
        if db_path is None:
            db_path = self.db_path
        jsonl_path = os.path.join(self.tmp.name, f"{os.path.basename(db_path)}.jsonl")
        support.write_jsonl(
            jsonl_path,
            [
                support.make_record(
                    event_id="event-1", session_id="session-1", command="python test"
                ),
                support.make_record(
                    event_id="event-2",
                    session_id="session-2",
                    timestamp="2026-09-21T10:00:01Z",
                ),
            ],
        )
        with ObservatoryStore(db_path) as store:
            store.import_jsonl(jsonl_path)
            store.save_classification(
                subject_kind="session",
                snapshot_hash="s" * 64,
                taxonomy_version="1.1.0",
                question_hash="q" * 64,
                model_version="jev-1.13.0",
                request_hash="r" * 64,
                response_hash="a" * 64,
                answers=[
                    {
                        "question_id": "q-choice",
                        "question_type": "choice",
                        "answer": {"choice": "implementation", "confidence": 0.8},
                    },
                    {
                        "question_id": "q-noul",
                        "question_type": "noul",
                        "answer": {"noul": 0.65},
                    },
                ],
            )
        self._downgrade_schema7_to_v6(db_path)
        return db_path

    @staticmethod
    def _downgrade_schema7_to_v6(db_path):
        connection = sqlite3.connect(db_path)
        try:
            connection.execute("DROP VIEW events_with_enrichment")
            connection.execute("DROP TABLE session_enrichment")
            # Schema 6 derived roles from provider names; these stand in for
            # legacy values that schema 7 must clear before trusted import.
            connection.execute("UPDATE sessions SET role = 'legacy-guessed-role'")
            connection.execute("UPDATE schema_meta SET value = '6' WHERE key = 'schema_version'")
            connection.execute("PRAGMA user_version = 6")
            connection.commit()
        finally:
            connection.close()

    @staticmethod
    def _downgrade_schema6_to_v5(db_path):
        connection = sqlite3.connect(db_path)
        try:
            version = int(connection.execute("PRAGMA user_version").fetchone()[0])
        finally:
            connection.close()
        if version == 7:
            MigrationTest._downgrade_schema7_to_v6(db_path)
        connection = sqlite3.connect(db_path)
        try:
            connection.execute("DROP VIEW event_usage_cost")
            connection.execute("DROP TABLE classification_sessions")
            connection.execute("DROP TABLE model_pricing")
            connection.execute("ALTER TABLE sessions DROP COLUMN role")
            for column in ("session_id", "provider", "host_id", "city_id"):
                connection.execute(f"ALTER TABLE jev_requests DROP COLUMN {column}")
            connection.execute("UPDATE schema_meta SET value = '5' WHERE key = 'schema_version'")
            connection.execute("PRAGMA user_version = 5")
            connection.commit()
        finally:
            connection.close()

    def _make_v5_fixture(self, db_path=None):
        if db_path is None:
            db_path = self.db_path
        self._make_schema6_fixture(db_path)
        self._downgrade_schema6_to_v5(db_path)
        return db_path

    def _make_v4_fixture(self, db_path=None):
        """Create a populated v4 projection from the current schema fixture."""
        if db_path is None:
            db_path = self.db_path
        self._make_v5_fixture(db_path)
        connection = sqlite3.connect(db_path)
        try:
            connection.execute("DROP TABLE recommendations")
            connection.execute("UPDATE schema_meta SET value = '4' WHERE key = 'schema_version'")
            connection.execute("PRAGMA user_version = 4")
            connection.commit()
        finally:
            connection.close()
        return db_path

    @staticmethod
    def _counts(db_path):
        connection = sqlite3.connect(db_path)
        try:
            return {
                table: connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
                for table in ("events", "sessions", "classifications", "classification_answers")
            }
        finally:
            connection.close()


    @staticmethod
    def _backup_paths_v5(db_path):
        directory = os.path.dirname(db_path)
        prefix = os.path.basename(db_path) + ".v5-pre-schema6-"
        return sorted(
            os.path.realpath(os.path.join(directory, name))
            for name in os.listdir(directory)
            if name.startswith(prefix) and name.endswith(".bak")
        )

    @staticmethod
    def _backup_paths_v6(db_path):
        directory = os.path.dirname(db_path)
        prefix = os.path.basename(db_path) + ".v6-pre-schema7-"
        return sorted(
            os.path.realpath(os.path.join(directory, name))
            for name in os.listdir(directory)
            if name.startswith(prefix) and name.endswith(".bak")
        )

    @staticmethod
    def _table_count(db_path, table):
        connection = sqlite3.connect(db_path)
        try:
            return connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
        finally:
            connection.close()

    @staticmethod
    def _backup_paths(db_path):
        directory = os.path.dirname(db_path)
        prefix = os.path.basename(db_path) + ".v4-pre-schema5-"
        return sorted(
            os.path.realpath(os.path.join(directory, name))
            for name in os.listdir(directory)
            if name.startswith(prefix) and name.endswith(".bak")
        )

    def test_migration_backs_up_v4_and_preserves_evidence_rows(self):
        self._make_v4_fixture()
        result = migrate_database(self.db_path)

        expected_counts = {
            "events": 2,
            "sessions": 2,
            "classifications": 1,
            "classification_answers": 2,
        }
        self.assertEqual(result.from_version, 4)
        self.assertEqual(result.to_version, 7)
        self.assertEqual(result.database_path, os.path.realpath(self.db_path))
        self.assertEqual(set(result.preserved_rows), _V6_REQUIRED_TABLES)
        self.assertEqual(
            {table: result.preserved_rows[table] for table in expected_counts}, expected_counts
        )
        self.assertEqual(result.preserved_rows["imported_files"], 1)
        self.assertEqual(len(result.backup_paths), 3)
        self.assertEqual(result.backup_path, result.backup_paths[-1])
        self.assertTrue(all(os.path.isfile(path) for path in result.backup_paths))
        self.assertEqual(self._backup_paths(self.db_path), [result.backup_paths[0]])
        self.assertEqual(self._backup_paths_v5(self.db_path), [result.backup_paths[1]])
        self.assertEqual(self._backup_paths_v6(self.db_path), [result.backup_paths[2]])
        self.assertEqual(self._counts(self.db_path), expected_counts)
        self.assertEqual(self._table_count(self.db_path, "imported_files"), 1)
        self.assertEqual(result.backfill_summary["classifications_total"], 1)
        self.assertEqual(result.backfill_summary["classifications_unbound"], 1)
        self.assertEqual(result.backfill_summary["classification_match_rate"], 0.0)
        self.assertEqual(result.backfill_summary["sessions_total"], 2)
        self.assertEqual(result.backfill_summary["sessions_with_role"], 0)
        self.assertEqual(result.backfill_summary["pricing_seed_rows"], 3)

        upgraded = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(upgraded.execute("PRAGMA user_version").fetchone()[0], 7)
            self.assertEqual(
                upgraded.execute(
                    "SELECT value FROM schema_meta WHERE key = 'schema_version'"
                ).fetchone()[0],
                "7",
            )
            self.assertEqual(upgraded.execute("SELECT COUNT(*) FROM recommendations").fetchone()[0], 0)
            self.assertEqual(upgraded.execute("SELECT COUNT(*) FROM classification_sessions").fetchone()[0], 0)
            self.assertEqual(upgraded.execute("SELECT COUNT(*) FROM model_pricing").fetchone()[0], 3)
            pricing_columns = {
                row[1]: row for row in upgraded.execute("PRAGMA table_info(model_pricing)")
            }
            for field in (
                "input_usd_per_million",
                "output_usd_per_million",
                "cache_read_usd_per_million",
                "cache_write_usd_per_million",
            ):
                self.assertEqual(pricing_columns[field][2].upper(), "TEXT")
            self.assertEqual(pricing_columns["provider"][3], 0)
            self.assertEqual(
                tuple(
                    pricing_columns[field][5]
                    for field in ("model_id", "provider", "effective_from")
                ),
                (1, 2, 3),
            )
            self.assertIsNotNone(
                upgraded.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'view' AND name = 'event_usage_cost'"
                ).fetchone()
            )
            self.assertIn(
                "role", {row[1] for row in upgraded.execute("PRAGMA table_info(sessions)")}
            )
            self.assertEqual(
                upgraded.execute("SELECT COUNT(*) FROM sessions WHERE role IS NOT NULL").fetchone()[0],
                0,
            )
            self.assertIsNotNone(
                upgraded.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_enrichment'"
                ).fetchone()
            )
            self.assertIsNotNone(
                upgraded.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'view' AND name = 'events_with_enrichment'"
                ).fetchone()
            )
            self.assertEqual(
                upgraded.execute(
                    "SELECT command FROM events WHERE event_id = 'event-1'"
                ).fetchone()[0],
                "python test",
            )
        finally:
            upgraded.close()

        v4_backup, v5_backup, v6_backup = result.backup_paths
        backup4 = sqlite3.connect(v4_backup)
        try:
            self.assertEqual(backup4.execute("PRAGMA user_version").fetchone()[0], 4)
            self.assertEqual(backup4.execute("SELECT COUNT(*) FROM events").fetchone()[0], 2)
            self.assertIsNone(
                backup4.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'recommendations'"
                ).fetchone()
            )
        finally:
            backup4.close()
        backup5 = sqlite3.connect(v5_backup)
        try:
            self.assertEqual(backup5.execute("PRAGMA user_version").fetchone()[0], 5)
            self.assertEqual(backup5.execute("SELECT COUNT(*) FROM recommendations").fetchone()[0], 0)
            self.assertNotIn(
                "role", {row[1] for row in backup5.execute("PRAGMA table_info(sessions)")}
            )
        finally:
            backup5.close()
        backup6 = sqlite3.connect(v6_backup)
        try:
            self.assertEqual(backup6.execute("PRAGMA user_version").fetchone()[0], 6)
            self.assertEqual(backup6.execute("SELECT COUNT(*) FROM events").fetchone()[0], 2)
            self.assertEqual(
                backup6.execute("SELECT COUNT(*) FROM sessions WHERE role IS NOT NULL").fetchone()[0],
                0,
            )
            self.assertIsNone(
                backup6.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_enrichment'"
                ).fetchone()
            )
        finally:
            backup6.close()

    def test_v5_migration_backfills_snapshots_and_leaves_roles_unknown(self):
        session_text = "gateway-llm-dell--dsh-luna-1-pool"
        mayor = "rig--mayor"
        lane_a = "rig--fleet-work-review-2-pool"
        lane_b = "rig--fleet-work-build-1-pool"
        single_path = os.path.join(self.tmp.name, "text-session.jsonl")
        mayor_path = os.path.join(self.tmp.name, "mayor.jsonl")
        shared_path = os.path.join(self.tmp.name, "shared.jsonl")
        support.write_jsonl(
            single_path,
            [support.make_record(event_id="e-text", session_id=session_text)],
        )
        support.write_jsonl(mayor_path, [support.make_record(event_id="e-mayor", session_id=mayor)])
        support.write_jsonl(
            shared_path,
            [
                support.make_record(event_id="e-lane-a", session_id=lane_a),
                support.make_record(event_id="e-lane-b", session_id=lane_b),
            ],
        )

        key_text = ("city-a", "host-a", "codex", session_text)
        key_mayor = ("city-a", "host-a", "codex", mayor)
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(single_path)
            store.import_jsonl(mayor_path)
            store.import_jsonl(shared_path)
            raw_snapshot = store.session_snapshot(key_text)
            text_snapshot = session_text_snapshot_hash(raw_snapshot)
            event_snapshot = store.event_snapshot(key_mayor, "e-mayor")

            def save_classification(
                snapshot_hash,
                suffix,
                *,
                subject_kind="session",
                source_path=None,
                request_source_path=None,
            ):
                request_hash = (suffix * 64)[:64]
                source_sha256 = (
                    sha256_bytes(Path(source_path).read_bytes()) if source_path is not None else None
                )
                if request_source_path is not None:
                    store.save_request(
                        request_hash=request_hash,
                        snapshot_hash=snapshot_hash,
                        subject_kind=subject_kind,
                        taxonomy_version="1.1.0",
                        question_hash="q" * 64,
                        model="jev-1.13.0",
                        request_json="{}",
                        request_bytes=2,
                        source_path=request_source_path,
                        source_sha256=sha256_bytes(Path(request_source_path).read_bytes()),
                    )
                return store.save_classification(
                    subject_kind=subject_kind,
                    snapshot_hash=snapshot_hash,
                    taxonomy_version="1.1.0",
                    question_hash="q" * 64,
                    model_version="jev-1.13.0",
                    request_hash=request_hash,
                    response_hash=("r" + suffix) * 32,
                    answers=[
                        {
                            "question_id": "primary_intent",
                            "question_type": "choice",
                            "answer": {"choice": "implementation", "confidence": 0.9},
                        }
                    ],
                    source_path=source_path,
                    source_sha256=source_sha256,
                )

            save_classification(raw_snapshot, "1")
            save_classification(text_snapshot, "2")
            save_classification(event_snapshot, "3", subject_kind="event")
            drifted_hash = "d" * 64
            save_classification(
                drifted_hash, "4", request_source_path=mayor_path
            )
            source_hash = "p" * 64
            save_classification(source_hash, "7", source_path=single_path)
            ambiguous_hash = "a" * 64
            save_classification(ambiguous_hash, "5", source_path=shared_path)
            unmatched_hash = "u" * 64
            save_classification(unmatched_hash, "6")

        self._downgrade_schema6_to_v5(self.db_path)
        result = migrate_database(self.db_path)

        self.assertEqual(result.from_version, 5)
        self.assertEqual(result.to_version, 7)
        self.assertEqual(len(result.backup_paths), 2)
        self.assertEqual(result.backup_path, result.backup_paths[-1])
        self.assertEqual(self._backup_paths_v5(self.db_path), [result.backup_paths[0]])
        self.assertEqual(self._backup_paths_v6(self.db_path), [result.backup_paths[1]])
        summary = result.backfill_summary
        self.assertEqual(summary["classifications_total"], 7)
        self.assertEqual(summary["classifications_bound"], 5)
        self.assertEqual(summary["classifications_unbound"], 2)
        self.assertEqual(summary["classification_match_rate"], 0.714286)
        self.assertEqual(
            summary["classification_binding_methods"],
            {
                "snapshot": 1,
                "text_snapshot": 1,
                "event_snapshot": 1,
                "request": 1,
                "source_provenance": 1,
            },
        )
        self.assertEqual(summary["requests_total"], 1)
        self.assertEqual(summary["requests_bound"], 1)
        self.assertEqual(summary["request_match_rate"], 1.0)
        self.assertEqual(summary["pricing_seed_rows"], 3)
        self.assertEqual(summary["sessions_total"], 4)
        self.assertEqual(summary["sessions_with_role"], 0)
        self.assertEqual(summary["session_role_coverage"], 0.0)
        self.assertEqual(summary["untrusted_role_values_cleared"], 0)

        connection = sqlite3.connect(self.db_path)
        try:
            bindings = {
                row[0]: (row[1], row[2], row[3])
                for row in connection.execute(
                    "SELECT c.snapshot_hash, b.city_id, b.session_id, b.binding_method "
                    "FROM classifications c JOIN classification_sessions b "
                    "ON b.classification_id = c.classification_id"
                )
            }
            self.assertEqual(bindings[raw_snapshot], ("city-a", session_text, "snapshot"))
            self.assertEqual(bindings[text_snapshot], ("city-a", session_text, "text_snapshot"))
            self.assertEqual(bindings[event_snapshot], ("city-a", mayor, "event_snapshot"))
            self.assertEqual(bindings[drifted_hash], ("city-a", mayor, "request"))
            self.assertEqual(bindings[source_hash], ("city-a", session_text, "source_provenance"))
            self.assertNotIn(ambiguous_hash, bindings)
            self.assertNotIn(unmatched_hash, bindings)
            roles = dict(connection.execute("SELECT session_id, role FROM sessions"))
        finally:
            connection.close()
        self.assertTrue(all(role is None for role in roles.values()))

    def test_migrates_database_written_by_v4_era_store(self):
        # This fixture was created with ObservatoryStore from d812ee146^, when
        # schema version 4 was current; it is not a downgraded v5 database.
        fixture_path = os.path.join(
            os.path.dirname(__file__), "fixtures", "v4-era-schema4.sqlite"
        )
        shutil.copyfile(fixture_path, self.db_path)

        original = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(original.execute("PRAGMA user_version").fetchone()[0], 4)
            self.assertEqual(original.execute("SELECT COUNT(*) FROM events").fetchone()[0], 1)
            self.assertEqual(
                original.execute("SELECT COUNT(*) FROM imported_files").fetchone()[0], 1
            )
            self.assertIsNone(
                original.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'recommendations'"
                ).fetchone()
            )
        finally:
            original.close()

        result = migrate_database(self.db_path)

        self.assertEqual(result.from_version, 4)
        self.assertEqual(result.to_version, 7)
        self.assertEqual(set(result.preserved_rows), _V6_REQUIRED_TABLES)
        self.assertEqual(result.preserved_rows["events"], 1)
        self.assertEqual(result.preserved_rows["imported_files"], 1)
        self.assertEqual(self._table_count(self.db_path, "events"), 1)
        self.assertEqual(self._table_count(self.db_path, "imported_files"), 1)

    def test_v6_migration_backs_up_rows_and_clears_untrusted_roles(self):
        self._make_schema6_fixture()
        result = migrate_database(self.db_path)

        self.assertEqual(result.from_version, 6)
        self.assertEqual(result.to_version, 7)
        self.assertEqual(result.backup_paths, (result.backup_path,))
        self.assertEqual(self._backup_paths_v6(self.db_path), [result.backup_path])
        self.assertEqual(result.backfill_summary["untrusted_role_values_cleared"], 2)
        self.assertEqual(self._table_count(self.db_path, "events"), 2)
        self.assertEqual(self._table_count(self.db_path, "session_enrichment"), 0)

        upgraded = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(upgraded.execute("PRAGMA user_version").fetchone()[0], 7)
            role_count = upgraded.execute(
                "SELECT COUNT(*) FROM sessions WHERE role IS NOT NULL"
            ).fetchone()[0]
            self.assertEqual(role_count, 0)
        finally:
            upgraded.close()

        backup = sqlite3.connect(result.backup_path)
        try:
            self.assertEqual(backup.execute("PRAGMA user_version").fetchone()[0], 6)
            legacy_roles = backup.execute(
                "SELECT COUNT(*) FROM sessions WHERE role = 'legacy-guessed-role'"
            ).fetchone()[0]
            self.assertEqual(legacy_roles, 2)
        finally:
            backup.close()

    def test_v6_migration_refreshes_cost_view_for_legacy_vendor_prices(self):
        events_path = support.write_jsonl(
            os.path.join(self.tmp.name, "legacy-priced-events.jsonl"),
            [
                support.make_record(
                    event_id="legacy-priced-event",
                    provider="claude",
                    model="claude-sonnet-4-20250514",
                    usage={"input_tokens": 1_000_000},
                )
            ],
        )
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(events_path)
        self._downgrade_schema7_to_v6(self.db_path)

        connection = sqlite3.connect(self.db_path)
        try:
            connection.execute(
                "UPDATE model_pricing SET provider = 'Anthropic' "
                "WHERE model_id = 'claude-sonnet-4-20250514' AND provider = 'claude'"
            )
            connection.execute("DROP VIEW event_usage_cost")
            # Model an older v6 projection whose cost view did not resolve
            # adapter/vendor aliases or expose a measured cost.
            connection.execute(
                "CREATE VIEW event_usage_cost AS "
                "SELECT event_id, 0 AS cost_known, NULL AS cost_usd FROM events"
            )
            connection.commit()
        finally:
            connection.close()

        migrate_database(self.db_path)
        with ObservatoryStore(self.db_path) as store:
            row = dict(
                store.conn.execute(
                    "SELECT event_id, model_provider, cost_known, cost_usd "
                    "FROM event_usage_cost WHERE event_id = 'legacy-priced-event'"
                ).fetchone()
            )
        self.assertEqual(row["model_provider"], "claude")
        self.assertEqual(row["cost_known"], 1)
        self.assertEqual(row["cost_usd"], "3")

    def test_v6_migration_failure_rolls_back_and_keeps_v6_backup(self):
        self._make_schema6_fixture()
        connection = sqlite3.connect(self.db_path)
        try:
            connection.execute(
                """
                CREATE TRIGGER delete_event_during_v7_upgrade
                AFTER UPDATE ON schema_meta
                WHEN OLD.key = 'schema_version' AND NEW.value = '7'
                BEGIN
                    DELETE FROM events WHERE event_id = 'event-1';
                END
                """
            )
            connection.commit()
        finally:
            connection.close()

        with self.assertRaisesRegex(ObservatoryError, "row count"):
            migrate_database(self.db_path)

        connection = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(connection.execute("PRAGMA user_version").fetchone()[0], 6)
            self.assertEqual(
                connection.execute(
                    "SELECT value FROM schema_meta WHERE key = 'schema_version'"
                ).fetchone()[0],
                "6",
            )
            self.assertEqual(connection.execute("SELECT COUNT(*) FROM events").fetchone()[0], 2)
            self.assertEqual(
                connection.execute(
                    "SELECT COUNT(*) FROM sessions WHERE role = 'legacy-guessed-role'"
                ).fetchone()[0],
                2,
            )
            self.assertIsNone(
                connection.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_enrichment'"
                ).fetchone()
            )
        finally:
            connection.close()

        backups = self._backup_paths_v6(self.db_path)
        self.assertEqual(len(backups), 1)
        backup = sqlite3.connect(backups[0])
        try:
            self.assertEqual(backup.execute("PRAGMA user_version").fetchone()[0], 6)
            self.assertEqual(backup.execute("SELECT COUNT(*) FROM events").fetchone()[0], 2)
        finally:
            backup.close()

    def test_v6_validation_rejects_schema7_objects_without_backup(self):
        schema7_objects = (
            ("table", "CREATE TABLE session_enrichment (placeholder TEXT)"),
            ("view", "CREATE VIEW events_with_enrichment AS SELECT 1 AS placeholder"),
        )
        for name, statement in schema7_objects:
            with self.subTest(name=name):
                db_path = os.path.join(self.tmp.name, f"schema6-with-{name}.db")
                self._make_schema6_fixture(db_path)
                connection = sqlite3.connect(db_path)
                try:
                    connection.execute(statement)
                    connection.commit()
                finally:
                    connection.close()

                with self.assertRaisesRegex(ObservatoryError, "schema-7 objects"):
                    migrate_database(db_path)
                self.assertEqual(self._backup_paths_v6(db_path), [])

    def test_v6_migration_refuses_backup_row_count_mismatch(self):
        self._make_schema6_fixture()
        create_backup = migrations._create_backup

        def mismatched_backup_counts(database_path, version):
            backup_path, counts = create_backup(database_path, version)
            altered_counts = dict(counts)
            altered_counts["events"] += 1
            return backup_path, altered_counts

        with mock.patch.object(
            migrations, "_create_backup", side_effect=mismatched_backup_counts
        ):
            with self.assertRaisesRegex(ObservatoryError, "backup row counts do not match"):
                migrate_database(self.db_path)

        connection = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(connection.execute("PRAGMA user_version").fetchone()[0], 6)
            self.assertEqual(connection.execute("SELECT COUNT(*) FROM events").fetchone()[0], 2)
            self.assertIsNone(
                connection.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_enrichment'"
                ).fetchone()
            )
        finally:
            connection.close()
        backups = self._backup_paths_v6(self.db_path)
        self.assertEqual(len(backups), 1)
        self.assertEqual(self._counts(backups[0])["events"], 2)

    def test_normal_store_open_does_not_implicitly_migrate_v4(self):
        self._make_v4_fixture()
        with self.assertRaises(SchemaVersionError):
            ObservatoryStore(self.db_path)
        self.assertEqual(self._counts(self.db_path)["events"], 2)
        self.assertEqual(self._backup_paths(self.db_path), [])

    def test_normal_store_open_does_not_implicitly_migrate_v6(self):
        self._make_schema6_fixture()
        with self.assertRaises(SchemaVersionError):
            ObservatoryStore(self.db_path)
        self.assertEqual(self._counts(self.db_path)["events"], 2)
        self.assertEqual(self._backup_paths_v6(self.db_path), [])

    def test_rejects_unsupported_versions_without_writing_a_backup(self):
        for version in (0, 3, 999):
            with self.subTest(version=version):
                db_path = os.path.join(self.tmp.name, f"schema-{version}.db")
                self._make_v4_fixture(db_path)
                connection = sqlite3.connect(db_path)
                try:
                    connection.execute(
                        "UPDATE schema_meta SET value = ? WHERE key = 'schema_version'",
                        (str(version),),
                    )
                    connection.execute(f"PRAGMA user_version = {version}")
                    connection.commit()
                finally:
                    connection.close()

                with self.assertRaises(SchemaVersionError):
                    migrate_database(db_path)
                self.assertEqual(self._backup_paths(db_path), [])
                connection = sqlite3.connect(db_path)
                try:
                    self.assertEqual(
                        connection.execute("PRAGMA user_version").fetchone()[0], version
                    )
                finally:
                    connection.close()

    def test_rejects_schema5_markers_without_schema5_objects(self):
        self._make_v4_fixture()
        connection = sqlite3.connect(self.db_path)
        try:
            connection.execute("UPDATE schema_meta SET value = '5' WHERE key = 'schema_version'")
            connection.execute("PRAGMA user_version = 5")
            connection.commit()
        finally:
            connection.close()

        with self.assertRaises(ObservatoryError):
            migrate_database(self.db_path)
        self.assertEqual(self._backup_paths(self.db_path), [])
        self.assertEqual(self._table_count(self.db_path, "events"), 2)

    def test_refuses_inconsistent_schema_meta_without_backup(self):
        self._make_v4_fixture()
        connection = sqlite3.connect(self.db_path)
        try:
            connection.execute("UPDATE schema_meta SET value = '5' WHERE key = 'schema_version'")
            connection.commit()
        finally:
            connection.close()

        with self.assertRaises(SchemaVersionError):
            migrate_database(self.db_path)
        self.assertEqual(self._backup_paths(self.db_path), [])

    def test_failure_rolls_back_schema_and_version_but_keeps_backup(self):
        self._make_v4_fixture()
        connection = sqlite3.connect(self.db_path)
        try:
            connection.execute(
                """
                CREATE TRIGGER mutate_event_during_upgrade
                AFTER UPDATE ON schema_meta
                WHEN OLD.key = 'schema_version' AND NEW.value = '5'
                BEGIN
                    DELETE FROM events WHERE event_id = 'event-1';
                END
                """
            )
            connection.commit()
        finally:
            connection.close()

        with self.assertRaises(ObservatoryError):
            migrate_database(self.db_path)

        connection = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(connection.execute("PRAGMA user_version").fetchone()[0], 4)
            self.assertEqual(
                connection.execute(
                    "SELECT value FROM schema_meta WHERE key = 'schema_version'"
                ).fetchone()[0],
                "4",
            )
            self.assertIsNone(
                connection.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'recommendations'"
                ).fetchone()
            )
        finally:
            connection.close()
        self.assertEqual(self._counts(self.db_path)["events"], 2)
        backups = self._backup_paths(self.db_path)
        self.assertEqual(len(backups), 1)
        self.assertEqual(self._counts(backups[0])["events"], 2)

    def test_v5_migration_failure_rolls_back_and_keeps_v5_backup(self):
        self._make_v5_fixture()
        connection = sqlite3.connect(self.db_path)
        try:
            connection.execute(
                """
                CREATE TRIGGER delete_event_during_v6_upgrade
                AFTER UPDATE ON schema_meta
                WHEN OLD.key = 'schema_version' AND NEW.value = '6'
                BEGIN
                    DELETE FROM events WHERE event_id = 'event-1';
                END
                """
            )
            connection.commit()
        finally:
            connection.close()

        with self.assertRaises(ObservatoryError):
            migrate_database(self.db_path)

        connection = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(connection.execute("PRAGMA user_version").fetchone()[0], 5)
            self.assertEqual(
                connection.execute(
                    "SELECT value FROM schema_meta WHERE key = 'schema_version'"
                ).fetchone()[0],
                "5",
            )
            self.assertEqual(connection.execute("SELECT COUNT(*) FROM events").fetchone()[0], 2)
            self.assertNotIn(
                "role", {row[1] for row in connection.execute("PRAGMA table_info(sessions)")}
            )
            self.assertIsNone(
                connection.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'model_pricing'"
                ).fetchone()
            )
        finally:
            connection.close()
        backups = self._backup_paths_v5(self.db_path)
        self.assertEqual(len(backups), 1)
        self.assertEqual(self._counts(backups[0])["events"], 2)

    def test_failure_rolls_back_imported_file_deletion_during_upgrade(self):
        self._make_v4_fixture()
        self.assertEqual(self._table_count(self.db_path, "imported_files"), 1)
        connection = sqlite3.connect(self.db_path)
        try:
            connection.execute(
                """
                CREATE TRIGGER delete_imported_files_during_upgrade
                AFTER UPDATE ON schema_meta
                BEGIN
                    DELETE FROM imported_files;
                END
                """
            )
            connection.commit()
        finally:
            connection.close()

        with self.assertRaises(ObservatoryError):
            migrate_database(self.db_path)

        connection = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(connection.execute("PRAGMA user_version").fetchone()[0], 4)
            self.assertEqual(
                connection.execute(
                    "SELECT value FROM schema_meta WHERE key = 'schema_version'"
                ).fetchone()[0],
                "4",
            )
            self.assertEqual(
                connection.execute("SELECT COUNT(*) FROM imported_files").fetchone()[0], 1
            )
            self.assertIsNone(
                connection.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'recommendations'"
                ).fetchone()
            )
        finally:
            connection.close()

        backups = self._backup_paths(self.db_path)
        self.assertEqual(len(backups), 1)
        self.assertEqual(self._table_count(backups[0], "imported_files"), 1)

    def test_cli_migrate_subcommand_reports_backup_and_versions(self):
        self._make_v4_fixture()
        env = dict(os.environ)
        env["PYTHONPATH"] = support.PACKAGE_ROOT + os.pathsep + env.get("PYTHONPATH", "")
        env["PYTHONDONTWRITEBYTECODE"] = "1"
        completed = subprocess.run(
            [sys.executable, "-m", "agent_observatory", "migrate", "--db", self.db_path],
            cwd=support.PACKAGE_ROOT,
            env=env,
            capture_output=True,
            text=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        payload = json.loads(completed.stdout)
        self.assertEqual(payload["from_version"], 4)
        self.assertEqual(payload["to_version"], 7)
        self.assertEqual(len(payload["backups"]), 3)
        self.assertTrue(os.path.isfile(payload["backup"]))
        self.assertEqual(payload["preserved_rows"]["events"], 2)
        self.assertEqual(payload["backfill_summary"]["classifications_total"], 1)

    def test_cli_migrate_is_noop_when_database_is_already_schema7(self):
        self._make_v4_fixture()
        migrate_database(self.db_path)
        with open(self.db_path, "rb") as database:
            migrated_bytes = database.read()
        backups_before = self._backup_paths(self.db_path)

        result = migrate_database(self.db_path)
        self.assertEqual(result.from_version, 7)
        self.assertEqual(result.to_version, 7)
        self.assertEqual(result.already_at_version, 7)
        self.assertIsNone(result.backup_path)
        with open(self.db_path, "rb") as database:
            self.assertEqual(database.read(), migrated_bytes)
        self.assertEqual(self._backup_paths(self.db_path), backups_before)

        env = dict(os.environ)
        env["PYTHONPATH"] = support.PACKAGE_ROOT + os.pathsep + env.get("PYTHONPATH", "")
        env["PYTHONDONTWRITEBYTECODE"] = "1"
        completed = subprocess.run(
            [sys.executable, "-m", "agent_observatory", "migrate", "--db", self.db_path],
            cwd=support.PACKAGE_ROOT,
            env=env,
            capture_output=True,
            text=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertEqual(completed.stdout.strip(), "already at schema 7")
        self.assertEqual(completed.stderr, "")
        with open(self.db_path, "rb") as database:
            self.assertEqual(database.read(), migrated_bytes)
        self.assertEqual(self._backup_paths(self.db_path), backups_before)

    def test_missing_database_is_not_created(self):
        missing_path = os.path.join(self.tmp.name, "missing.db")
        with self.assertRaises(FileNotFoundError):
            migrate_database(missing_path)
        self.assertFalse(os.path.exists(missing_path))


if __name__ == "__main__":
    unittest.main()
