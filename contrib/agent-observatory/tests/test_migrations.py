"""Tests for the explicit, backed-up schema-4 to schema-5 migration."""

from __future__ import annotations

import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory.errors import ObservatoryError, SchemaVersionError
from agent_observatory.migrations import migrate_database
from agent_observatory.store import ObservatoryStore


class MigrationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db_path = os.path.join(self.tmp.name, "obs.db")

    def _make_v4_fixture(self, db_path=None):
        """Build populated v5 data, then remove the sole v5 addition for a v4 fixture."""
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

        # v5 only adds this table and index. Remove that addition and set both
        # schema markers to 4 to create a representative, populated v4 fixture.
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
        self.assertEqual(result.to_version, 5)
        self.assertEqual(result.database_path, os.path.realpath(self.db_path))
        self.assertEqual(result.preserved_rows, expected_counts)
        self.assertTrue(os.path.isfile(result.backup_path))
        self.assertEqual(self._backup_paths(self.db_path), [result.backup_path])
        self.assertEqual(self._counts(self.db_path), expected_counts)

        upgraded = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(upgraded.execute("PRAGMA user_version").fetchone()[0], 5)
            self.assertEqual(
                upgraded.execute(
                    "SELECT value FROM schema_meta WHERE key = 'schema_version'"
                ).fetchone()[0],
                "5",
            )
            self.assertEqual(
                upgraded.execute("SELECT COUNT(*) FROM recommendations").fetchone()[0], 0
            )
            self.assertEqual(
                upgraded.execute(
                    "SELECT command FROM events WHERE event_id = 'event-1'"
                ).fetchone()[0],
                "python test",
            )
        finally:
            upgraded.close()

        backup = sqlite3.connect(result.backup_path)
        try:
            self.assertEqual(backup.execute("PRAGMA user_version").fetchone()[0], 4)
            self.assertEqual(
                backup.execute(
                    "SELECT value FROM schema_meta WHERE key = 'schema_version'"
                ).fetchone()[0],
                "4",
            )
            self.assertEqual(self._counts(result.backup_path), expected_counts)
            self.assertIsNone(
                backup.execute(
                    "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'recommendations'"
                ).fetchone()
            )
        finally:
            backup.close()

    def test_normal_store_open_does_not_implicitly_migrate_v4(self):
        self._make_v4_fixture()
        with self.assertRaises(SchemaVersionError):
            ObservatoryStore(self.db_path)
        self.assertEqual(self._counts(self.db_path)["events"], 2)
        self.assertEqual(self._backup_paths(self.db_path), [])

    def test_rejects_every_non_v4_version_without_writing_a_backup(self):
        for version in (0, 3, 5, 999):
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
                    self.assertEqual(connection.execute("PRAGMA user_version").fetchone()[0], version)
                finally:
                    connection.close()

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
        self.assertEqual(payload["to_version"], 5)
        self.assertTrue(os.path.isfile(payload["backup"]))
        self.assertEqual(payload["preserved_rows"]["events"], 2)

    def test_missing_database_is_not_created(self):
        missing_path = os.path.join(self.tmp.name, "missing.db")
        with self.assertRaises(FileNotFoundError):
            migrate_database(missing_path)
        self.assertFalse(os.path.exists(missing_path))


if __name__ == "__main__":
    unittest.main()
