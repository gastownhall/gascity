"""Trusted GC session metadata enrichment tests (synthetic identities only)."""

from __future__ import annotations

import contextlib
import io
import json
import os
import subprocess
import tempfile
import unittest
from unittest import mock

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory.changes import normalize_change
from agent_observatory.cli import main
from agent_observatory.exposure import (
    CommitGraph,
    attach_session_fingerprints,
    evaluate_change,
    session_evidence_from_store,
)
from agent_observatory.gc_enrichment import enrich_gc_sessions
from agent_observatory.store import ObservatoryStore


class GCEnrichmentTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db = os.path.join(self.tmp.name, "obs.db")
        self.events_path = support.write_jsonl(
            os.path.join(self.tmp.name, "events.jsonl"),
            [
                support.make_record(
                    city_id="city-t",
                    host_id="host-t",
                    provider="codex",
                    session_id="synthetic-provider-session",
                    event_id="synthetic-event",
                    kind="message",
                    text="Synthetic event content",
                )
            ],
        )
        with ObservatoryStore(self.db) as store:
            store.import_jsonl(self.events_path)
            store.import_registry(
                {
                    "session_fingerprints": [
                        {
                            "session": [
                                "city-t",
                                "host-t",
                                "codex",
                                "synthetic-provider-session",
                            ],
                            "type": "commit_sha",
                            "value": "a" * 40,
                            "observed_at": "2026-09-28T12:00:00Z",
                            "evidence": "synthetic fixture",
                        }
                    ]
                }
            )

    def _write_gc_export(self, sessions):
        path = os.path.join(self.tmp.name, "gc-sessions.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump({"schema_version": "1", "sessions": sessions}, handle)
        return path

    def _historical_transcript(self, cwds):
        path = os.path.join(self.tmp.name, "historical.jsonl")
        support.write_jsonl(path, [
            {"type": "session_meta", "payload": {
                "id": "synthetic-provider-session", "cwd": cwd}}
            for cwd in cwds
        ])
        with ObservatoryStore(self.db) as store:
            store.conn.execute("UPDATE events SET source_path = ?", (path,))
            store.conn.commit()

    def test_removed_worktree_prefix_is_anchored_and_outermost(self):
        cases = [
            ("/home/nonexistent-review-user/src/city-worktrees/deleted/src/gascity", "city"),
            ("/home/nonexistent-review-user/src/gascity-worktrees/deleted/projects/Gateway-LLM",
             "hoomji/gascity"),
            ("/tmp/unrelated-review-root/src/gascity/deleted", None),
        ]
        for cwd, expected in cases:
            with self.subTest(cwd=cwd):
                self._historical_transcript([cwd])
                with ObservatoryStore(self.db) as store:
                    store.conn.execute("DELETE FROM session_enrichment")
                    store.conn.commit()
                    enrich_gc_sessions(store, self._write_gc_export([]),
                                       city_id="city-t", host_id="host-t")
                    row = store.conn.execute("SELECT repo FROM session_enrichment").fetchone()
                    self.assertEqual(row[0] if row else None, expected)

    def test_removed_worktree_cwd_binds_without_inferred_role_and_is_idempotent(self):
        self._historical_transcript(["/home/example/src/gascity-worktrees/fleet-removed"])
        export = self._write_gc_export([])
        with ObservatoryStore(self.db) as store:
            first = enrich_gc_sessions(store, export, city_id="city-t", host_id="host-t")
            row = store.conn.execute("SELECT * FROM session_enrichment").fetchone()
            self.assertIsNotNone(row)
            self.assertEqual(row["repo"], "hoomji/gascity")
            self.assertEqual(row["repo_source"], "transcript_cwd_prefix")
            self.assertIsNone(row["template"])
            self.assertEqual(first.repo_bindings_written, 1)
            self.assertEqual(first.role_bindings_written, 0)
            second = enrich_gc_sessions(store, export, city_id="city-t", host_id="host-t")
            self.assertEqual(second.bindings_written, 0)
            self.assertEqual(second.repo_bindings_written, 0)
            self.assertIsNone(store.conn.execute("SELECT role FROM sessions").fetchone()[0])

    def test_historical_cwd_never_overwrites_explicit_binding(self):
        self._historical_transcript(["/home/example/src/gascity-worktrees/fleet-removed"])
        export = self._write_gc_export([{
            "provider": "codex", "session_key": "synthetic-provider-session",
            "template": "example/worker", "repo": "Example/Explicit"}])
        with ObservatoryStore(self.db) as store:
            enrich_gc_sessions(store, export, city_id="city-t", host_id="host-t")
            enrich_gc_sessions(store, self._write_gc_export([]), city_id="city-t", host_id="host-t")
            row = store.conn.execute("SELECT repo, repo_source, template FROM session_enrichment").fetchone()
            self.assertEqual(tuple(row), ("example/explicit", "explicit", "example/worker"))

    def test_ambiguous_historical_cwds_do_not_bind(self):
        self._historical_transcript([
            "/home/example/src/gascity-worktrees/fleet-removed",
            "/home/example/projects/Gateway-LLM/deleted"])
        with ObservatoryStore(self.db) as store:
            result = enrich_gc_sessions(store, self._write_gc_export([]), city_id="city-t", host_id="host-t")
            self.assertEqual(result.repo_ambiguous, 1)
            self.assertEqual(store.conn.execute("SELECT COUNT(*) FROM session_enrichment").fetchone()[0], 0)

    def test_deleted_collector_spool_resolves_native_checkpoint(self):
        self._historical_transcript(["/home/example/src/gascity-worktrees/fleet-removed"])
        with ObservatoryStore(self.db) as store:
            store.conn.execute("CREATE TABLE collector_sources(source_id TEXT, provider TEXT, path TEXT)")
            store.conn.execute("INSERT INTO collector_sources VALUES ('stable-source', 'codex', ?)",
                               (os.path.join(self.tmp.name, "historical.jsonl"),))
            store.conn.execute("UPDATE events SET source_path = '/removed/spool/stable-source.jsonl'")
            store.conn.commit()
            result = enrich_gc_sessions(store, self._write_gc_export([]), city_id="city-t", host_id="host-t")
            self.assertEqual(result.repo_bindings_written, 1)
            second = enrich_gc_sessions(store, self._write_gc_export([]), city_id="city-t", host_id="host-t")
            self.assertEqual(second.bindings_written, 0)

    def test_unknown_prefix_and_foreign_codex_context_do_not_bind(self):
        self._historical_transcript(["/home/example/src/unrelated-worktrees/fleet-removed"])
        with ObservatoryStore(self.db) as store:
            result = enrich_gc_sessions(store, self._write_gc_export([]), city_id="city-t", host_id="host-t")
            self.assertEqual(result.bindings_written, 0)
        path = os.path.join(self.tmp.name, "historical.jsonl")
        support.write_jsonl(path, [
            {"type": "session_meta", "payload": {"id": "foreign", "cwd": "/home/example/src/gascity"}},
            {"type": "turn_context", "payload": {"cwd": "/home/example/src/gascity"}},
        ])
        with ObservatoryStore(self.db) as store:
            result = enrich_gc_sessions(store, self._write_gc_export([]), city_id="city-t", host_id="host-t")
            self.assertEqual(result.bindings_written, 0)

    def test_claude_cwd_and_repo_only_exposure_consumer(self):
        path = os.path.join(self.tmp.name, "claude.jsonl")
        support.write_jsonl(path, [{"type": "user", "sessionId": "claude-session", "cwd":
                                   "/home/example/projects/Gateway-LLM/deleted"}])
        event = support.write_jsonl(os.path.join(self.tmp.name, "claude-event.jsonl"), [
            support.make_record(city_id="city-t", host_id="host-t", provider="claude",
                                session_id="claude-session", event_id="claude-event")])
        with ObservatoryStore(self.db) as store:
            store.import_jsonl(event)
            store.conn.execute("UPDATE events SET source_path = ? WHERE provider = 'claude'", (path,))
            store.conn.commit()
            result = enrich_gc_sessions(store, self._write_gc_export([]), city_id="city-t", host_id="host-t")
            self.assertEqual(result.repo_bindings_written, 1)
            row = store.conn.execute("SELECT repo, template FROM session_enrichment WHERE provider = 'claude'").fetchone()
            self.assertEqual(tuple(row), ("uniblock-dev/gateway-llm", None))
            evidence = session_evidence_from_store(store)
            self.assertTrue(evidence)

    def test_exact_gc_template_and_worktree_remote_enrich_without_event_rewrite(self):
        work_dir = os.path.join(self.tmp.name, "synthetic-worktree")
        os.mkdir(work_dir)
        gc_export = self._write_gc_export(
            [
                {
                    "id": "synthetic-gc-session-id",
                    "session_name": "synthetic-runtime-name",
                    "session_key": "synthetic-provider-session",
                    "provider": "codex",
                    "template": "example-rig/worker",
                    "worker_dir": work_dir,
                    "title": "Synthetic metadata field ignored by importer",
                }
            ]
        )
        remote = "https://example.test/Example/Project.git"
        git_result = subprocess.CompletedProcess(
            args=["git"], returncode=0, stdout=remote + "\n", stderr=""
        )

        with ObservatoryStore(self.db) as store:
            event_before = dict(
                store.conn.execute(
                    "SELECT payload_hash, repo, commit_sha FROM events WHERE event_id = 'synthetic-event'"
                ).fetchone()
            )
            with mock.patch("agent_observatory.gc_enrichment.subprocess.run", return_value=git_result):
                result = enrich_gc_sessions(
                    store, gc_export, city_id="city-t", host_id="host-t"
                )
            event_after = dict(
                store.conn.execute(
                    "SELECT payload_hash, repo, commit_sha FROM events WHERE event_id = 'synthetic-event'"
                ).fetchone()
            )
            enriched_event = dict(
                store.conn.execute(
                    "SELECT effective_repo, effective_role, gc_repo_source "
                    "FROM events_with_enrichment WHERE event_id = 'synthetic-event'"
                ).fetchone()
            )
            session_role = store.conn.execute(
                "SELECT role FROM sessions WHERE session_id = 'synthetic-provider-session'"
            ).fetchone()[0]
            fingerprints = store.load_session_fingerprints()
            evidence = attach_session_fingerprints(
                session_evidence_from_store(store), fingerprints
            )

        self.assertEqual(result.sessions_matched, 1)
        self.assertEqual(result.bindings_written, 1)
        self.assertEqual(result.role_bindings_written, 1)
        self.assertEqual(result.repo_bindings_written, 1)
        self.assertEqual(event_after, event_before)
        self.assertEqual(
            enriched_event,
            {
                "effective_repo": "example/project",
                "effective_role": "example-rig/worker",
                "gc_repo_source": "worker_dir",
            },
        )
        self.assertEqual(session_role, "example-rig/worker")
        self.assertEqual(fingerprints[0]["repo"], "example/project")
        self.assertEqual(evidence[0]["repo"], "example/project")
        # The command's counts-only result cannot leak IDs, local paths, titles,
        # or the remote URL that was normalized in-process.
        serialized = json.dumps(result.to_dict())
        for secret in (
            "synthetic-provider-session",
            "synthetic-gc-session-id",
            work_dir,
            "example.test",
            "Synthetic metadata field",
        ):
            self.assertNotIn(secret, serialized)

    def test_fingerprint_only_session_receives_repo_binding_for_exposure(self):
        db = os.path.join(self.tmp.name, "fingerprints-only.db")
        session = ["city-f", "host-f", "codex", "synthetic-fingerprint-session"]
        gc_export = self._write_gc_export(
            [
                {
                    "provider": "codex",
                    "session_key": "synthetic-fingerprint-session",
                    "template": "example-rig/worker",
                    "repo": "example.test/Example/Project.git",
                }
            ]
        )
        with ObservatoryStore(db) as store:
            store.import_registry(
                {
                    "session_fingerprints": [
                        {
                            "session": session,
                            "type": "commit_sha",
                            "value": "b" * 40,
                            "observed_at": "2026-09-28T12:00:00Z",
                        }
                    ]
                }
            )
            result = enrich_gc_sessions(
                store, gc_export, city_id="city-f", host_id="host-f"
            )
            evidence = attach_session_fingerprints(
                session_evidence_from_store(store), store.load_session_fingerprints()
            )
        self.assertEqual(result.sessions_matched, 1)
        self.assertEqual(len(evidence), 1)
        self.assertEqual(evidence[0]["repo"], "example/project")
        self.assertEqual(evidence[0]["commit_shas"], ["b" * 40])

    def test_event_repo_precedes_conflicting_enrichment_with_or_without_fingerprint(self):
        db = os.path.join(self.tmp.name, "repo-precedence.db")
        session = ["city-r", "host-r", "codex", "synthetic-repo-session"]
        commit_sha = "c" * 40
        events = support.write_jsonl(
            os.path.join(self.tmp.name, "repo-precedence.jsonl"),
            [
                support.make_record(
                    city_id=session[0],
                    host_id=session[1],
                    provider=session[2],
                    session_id=session[3],
                    event_id="synthetic-repo-event",
                    timestamp="2026-09-28T12:00:00Z",
                    repo="event/project",
                    commit_sha=commit_sha,
                )
            ],
        )
        gc_export = self._write_gc_export(
            [
                {
                    "provider": "codex",
                    "session_key": session[3],
                    "template": "example-rig/worker",
                    "repo": "gc/project",
                }
            ]
        )
        change = normalize_change(
            {
                "repo": "event/project",
                "kind": "pr",
                "pr": 1,
                "merge_sha": commit_sha,
                "merged_at": "2026-09-28T11:00:00Z",
                "changed_paths": ["README.md"],
            }
        )
        graph = CommitGraph({commit_sha: []})
        activations = [
            {
                "change_id": change["change_id"],
                "activation_id": "synthetic-merge-activation",
                "mechanism": "merge",
                "target": None,
                "activated_at": "2026-09-28T11:00:00Z",
                "deactivated_at": None,
                "fingerprint": None,
                "pending": False,
            }
        ]

        with ObservatoryStore(db) as store:
            store.import_jsonl(events)
            store.import_registry(
                {
                    "session_fingerprints": [
                        {
                            "session": session,
                            "type": "commit_sha",
                            "value": commit_sha,
                            "observed_at": "2026-09-28T12:00:00Z",
                            "evidence": "synthetic fixture",
                        }
                    ]
                }
            )
            enrich_gc_sessions(store, gc_export, city_id=session[0], host_id=session[1])
            without_fingerprint = session_evidence_from_store(store)
            fingerprints = store.load_session_fingerprints()
            with_fingerprint = attach_session_fingerprints(
                session_evidence_from_store(store), fingerprints
            )

        self.assertEqual(fingerprints[0]["repo"], "gc/project")
        self.assertEqual(without_fingerprint[0]["repo"], "event/project")
        self.assertEqual(with_fingerprint[0]["repo"], "event/project")
        without_verdict = evaluate_change(
            change, activations, graph=graph, sessions=without_fingerprint
        )["status"]
        with_verdict = evaluate_change(
            change, activations, graph=graph, sessions=with_fingerprint
        )["status"]
        self.assertEqual(without_verdict, "exposed")
        self.assertEqual(with_verdict, without_verdict)

    def test_rerunning_enrichment_over_identical_rows_writes_nothing(self):
        gc_export = self._write_gc_export(
            [
                {
                    "provider": "codex",
                    "session_key": "synthetic-provider-session",
                    "template": "example-rig/worker",
                    "repo": "example/project",
                }
            ]
        )
        with ObservatoryStore(self.db) as store:
            first = enrich_gc_sessions(
                store, gc_export, city_id="city-t", host_id="host-t"
            )
            row_before = tuple(
                store.conn.execute(
                    "SELECT template, repo, repo_source, source_sha256, updated_at "
                    "FROM session_enrichment WHERE session_id = 'synthetic-provider-session'"
                ).fetchone()
            )
            changes_before = store.conn.total_changes
            second = enrich_gc_sessions(
                store, gc_export, city_id="city-t", host_id="host-t"
            )
            row_after = tuple(
                store.conn.execute(
                    "SELECT template, repo, repo_source, source_sha256, updated_at "
                    "FROM session_enrichment WHERE session_id = 'synthetic-provider-session'"
                ).fetchone()
            )

            self.assertEqual(first.bindings_written, 1)
            self.assertEqual(first.role_bindings_written, 1)
            self.assertEqual(first.repo_bindings_written, 1)
            self.assertEqual(second.bindings_written, 0)
            self.assertEqual(second.role_bindings_written, 0)
            self.assertEqual(second.repo_bindings_written, 0)
            self.assertEqual(store.conn.total_changes, changes_before)
            self.assertEqual(row_after, row_before)

    def test_one_persisted_repo_conflict_is_not_double_counted_as_ambiguous(self):
        first_export = self._write_gc_export(
            [
                {
                    "provider": "codex",
                    "session_key": "synthetic-provider-session",
                    "template": "example-rig/worker",
                    "repo": "first/project",
                }
            ]
        )
        with ObservatoryStore(self.db) as store:
            enrich_gc_sessions(
                store, first_export, city_id="city-t", host_id="host-t"
            )
            conflicting_export = self._write_gc_export(
                [
                    {
                        "provider": "codex",
                        "session_key": "synthetic-provider-session",
                        "template": "example-rig/worker",
                        "repo": "second/project",
                    }
                ]
            )
            result = enrich_gc_sessions(
                store, conflicting_export, city_id="city-t", host_id="host-t"
            )
            stored_repo = store.conn.execute(
                "SELECT repo FROM session_enrichment "
                "WHERE session_id = 'synthetic-provider-session'"
            ).fetchone()[0]

        self.assertEqual(result.rows_usable, 1)
        self.assertEqual(result.conflicts, 1)
        self.assertEqual(result.repo_ambiguous, 0)
        self.assertEqual(result.bindings_written, 0)
        self.assertEqual(stored_repo, "first/project")

    def test_gc_id_and_session_name_never_substitute_for_provider_session_key(self):
        gc_export = self._write_gc_export(
            [
                {
                    "id": "synthetic-provider-session",
                    "session_name": "synthetic-provider-session",
                    "provider": "codex",
                    "template": "must-not-bind",
                }
            ]
        )
        with ObservatoryStore(self.db) as store:
            result = enrich_gc_sessions(store, gc_export, city_id="city-t", host_id="host-t")
            enrichment_count = store.conn.execute(
                "SELECT COUNT(*) FROM session_enrichment"
            ).fetchone()[0]
            role = store.conn.execute(
                "SELECT role FROM sessions WHERE session_id = 'synthetic-provider-session'"
            ).fetchone()[0]
        self.assertEqual(result.metadata_skipped, 1)
        self.assertEqual(result.sessions_matched, 0)
        self.assertEqual(enrichment_count, 0)
        self.assertIsNone(role)

    def test_conflicting_gc_templates_for_one_provider_key_remain_unbound(self):
        gc_export = self._write_gc_export(
            [
                {
                    "provider": "codex",
                    "session_key": "synthetic-provider-session",
                    "template": "example-rig/worker-a",
                },
                {
                    "provider": "codex",
                    "session_key": "synthetic-provider-session",
                    "template": "example-rig/worker-b",
                },
            ]
        )
        with ObservatoryStore(self.db) as store:
            result = enrich_gc_sessions(store, gc_export, city_id="city-t", host_id="host-t")
            enrichment_count = store.conn.execute(
                "SELECT COUNT(*) FROM session_enrichment"
            ).fetchone()[0]
            role = store.conn.execute(
                "SELECT role FROM sessions WHERE session_id = 'synthetic-provider-session'"
            ).fetchone()[0]
        self.assertEqual(result.conflicts, 1)
        self.assertEqual(result.sessions_matched, 0)
        self.assertEqual(enrichment_count, 0)
        self.assertIsNone(role)

    def test_cli_accepts_counts_only_metadata_import(self):
        gc_export = self._write_gc_export(
            [
                {
                    "provider": "codex",
                    "session_key": "synthetic-provider-session",
                    "template": "example-rig/worker",
                    "repo": "https://example.test/Example/Project.git",
                }
            ]
        )
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = main(
                [
                    "enrich-gc-sessions",
                    "--db",
                    self.db,
                    "--input",
                    gc_export,
                    "--city",
                    "city-t",
                    "--host",
                    "host-t",
                ]
            )
        self.assertEqual(code, 0)
        payload = json.loads(output.getvalue())
        self.assertEqual(payload["sessions_matched"], 1)
        self.assertNotIn("synthetic-provider-session", output.getvalue())
        self.assertNotIn(gc_export, output.getvalue())


if __name__ == "__main__":
    unittest.main()
