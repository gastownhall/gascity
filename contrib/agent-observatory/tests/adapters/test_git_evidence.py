"""Repository and commit evidence extraction across file transcript adapters."""

from __future__ import annotations

import contextlib
import io
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import adapter_support as support  # noqa: E402

from agent_observatory.adapters import read_source  # noqa: E402
from agent_observatory.adapters.git_evidence import normalize_repo_identity  # noqa: E402
from agent_observatory.cli import main  # noqa: E402
from agent_observatory.store import ObservatoryStore  # noqa: E402


class GitEvidenceAdapterTests(unittest.TestCase):
    def _read(self, provider: str, fixture_name: str):
        return read_source(
            support.fixture(provider, fixture_name),
            provider=provider,
            context=support.CONTEXT,
        )

    def test_each_transcript_adapter_collects_commit_push_and_fleet_evidence(self):
        cases = (
            ("claude", "claude-repo-evidence"),
            ("codex", "codex-repo-evidence"),
            ("dsh", "dsh-repo-evidence"),
        )
        expected_shas = {
            "1111111",
            "1111111111111111111111111111111111111111",
            "2222222222222222222222222222222222222222",
        }
        for provider, session_id in cases:
            with self.subTest(provider=provider):
                result = self._read(provider, "repo-evidence.jsonl")
                self.assertTrue(result.records)
                self.assertEqual({record["repo"] for record in result.records}, {"hoomji/gascity"})
                fingerprints = result.session_fingerprints
                self.assertTrue(fingerprints)
                self.assertEqual({item["type"] for item in fingerprints}, {"commit_sha"})
                self.assertEqual({item["value"] for item in fingerprints}, expected_shas)
                self.assertTrue(
                    all(
                        item["session"] == [support.CITY, support.HOST, provider, session_id]
                        for item in fingerprints
                    )
                )
                self.assertTrue(all(item["observed_at"] for item in fingerprints))
                evidence = {item["evidence"] for item in fingerprints}
                self.assertTrue(any("git_commit_output" in kind for kind in evidence))
                self.assertTrue(any("git_push_head:fleet_worktree=testdispatch" in kind for kind in evidence))
                self.assertTrue(any("git_rev_parse_head:fleet_worktree=testdispatch" in kind for kind in evidence))

                pushed_results = [
                    record for record in result.records
                    if record["kind"] == "tool_result" and record.get("tool_call_id") == "push-call"
                ]
                self.assertEqual(len(pushed_results), 1)
                self.assertEqual(
                    pushed_results[0]["commit_sha"],
                    "2222222222222222222222222222222222222222",
                )

    def test_each_transcript_adapter_explains_absent_repo_and_commit_evidence(self):
        cases = (
            ("claude", "claude-no-repo-evidence"),
            ("codex", "codex-no-repo-evidence"),
            ("dsh", "dsh-no-repo-evidence"),
        )
        for provider, session_id in cases:
            with self.subTest(provider=provider):
                result = self._read(provider, "no-repo-evidence.jsonl")
                self.assertTrue(result.records)
                self.assertTrue(all(record["repo"] is None for record in result.records))
                self.assertTrue(all(record["commit_sha"] is None for record in result.records))
                self.assertEqual(result.session_fingerprints, [])
                notes = " ".join(result.session_evidence_notes[session_id])
                self.assertIn("repo:none", notes)
                self.assertIn("commit_sha:none", notes)

    def test_export_persists_session_fingerprints(self):
        temp = support.make_temp_dir()
        self.addCleanup(temp.cleanup)
        db_path = os.path.join(temp.name, "obs.db")
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            code = main(
                [
                    "export",
                    "--provider",
                    "claude",
                    "--input",
                    support.fixture("claude", "repo-evidence.jsonl"),
                    "--city",
                    support.CITY,
                    "--host",
                    support.HOST,
                    "--db",
                    db_path,
                ]
            )
        self.assertEqual(code, 0)
        with ObservatoryStore(db_path) as store:
            fingerprints = store.load_session_fingerprints()
        self.assertTrue(fingerprints)
        self.assertEqual({item["type"] for item in fingerprints}, {"commit_sha"})

    def test_remote_urls_normalize_to_owner_and_repository(self):
        self.assertEqual(
            normalize_repo_identity("git@github.com:hoomji/gascity.git"),
            "hoomji/gascity",
        )
        self.assertEqual(
            normalize_repo_identity("https://github.com/hoomji/gascity.git"),
            "hoomji/gascity",
        )
        self.assertIsNone(normalize_repo_identity("/worktrees/fleet-example"))


if __name__ == "__main__":
    unittest.main()
