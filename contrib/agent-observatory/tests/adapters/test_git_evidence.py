"""Repository and commit evidence extraction across file transcript adapters."""

from __future__ import annotations

import contextlib
import io
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import adapter_support as support  # noqa: E402

from agent_observatory.adapters import read_source  # noqa: E402
from agent_observatory.adapters.git_evidence import _fleet_id, normalize_repo_identity  # noqa: E402
from agent_observatory.cli import main  # noqa: E402
from agent_observatory.store import ObservatoryStore  # noqa: E402


class GitEvidenceAdapterTests(unittest.TestCase):
    def _read(self, provider: str, fixture_name: str):
        return read_source(
            support.fixture(provider, fixture_name),
            provider=provider,
            context=support.CONTEXT,
        )

    def _read_claude_calls(self, calls, *, cwd="/home/fixture/project"):
        session_id = "git-evidence-synthetic"
        temp = support.make_temp_dir()
        self.addCleanup(temp.cleanup)
        source = os.path.join(temp.name, "synthetic.jsonl")
        records = [
            {
                "type": "user",
                "uuid": "synthetic-start",
                "sessionId": session_id,
                "timestamp": "2026-09-28T12:00:00.000Z",
                "cwd": cwd,
                "message": {
                    "role": "user",
                    "content": [{"type": "text", "text": "Synthetic git evidence"}],
                },
            }
        ]
        for sequence, (call_id, command, output) in enumerate(calls, start=1):
            call_second = sequence * 2 - 1
            result_second = sequence * 2
            records.extend(
                [
                    {
                        "type": "assistant",
                        "uuid": f"synthetic-call-{sequence}",
                        "sessionId": session_id,
                        "timestamp": f"2026-09-28T12:00:{call_second:02d}.000Z",
                        "message": {
                            "content": [
                                {
                                    "type": "tool_use",
                                    "id": call_id,
                                    "name": "Bash",
                                    "input": {"command": command},
                                }
                            ]
                        },
                    },
                    {
                        "type": "user",
                        "uuid": f"synthetic-result-{sequence}",
                        "sessionId": session_id,
                        "timestamp": f"2026-09-28T12:00:{result_second:02d}.000Z",
                        "message": {
                            "role": "user",
                            "content": [
                                {"type": "tool_result", "tool_use_id": call_id, "content": output}
                            ],
                        },
                    },
                ]
            )
        with open(source, "w", encoding="utf-8") as handle:
            handle.write("".join(json.dumps(record) + "\n" for record in records))
        return read_source(source, provider="claude", context=support.CONTEXT)

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
                self.assertTrue(any("git_push_head:fleet_worktree=0123456789abcdefabcd" in kind for kind in evidence))
                self.assertTrue(any("git_rev_parse_head:fleet_worktree=0123456789abcdefabcd" in kind for kind in evidence))

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

    def test_signed_remote_urls_drop_userinfo_query_and_fragment(self):
        signed_urls = (
            "git:ghp_FAKE@github.com:hoomji/gascity.git?token=FAKE_TOKEN#fake-fragment",
            "https://git:ghp_FAKE@github.com/hoomji/gascity.git?token=FAKE_TOKEN#fake-fragment",
        )
        for url in signed_urls:
            with self.subTest(scheme=url.split(":", 1)[0]):
                repo = normalize_repo_identity(url)
                self.assertEqual(repo, "hoomji/gascity")
                self.assertNotIn("token=", repo)
                self.assertNotIn("ghp_", repo)

    def test_dotless_scp_hosts_normalize_without_host_or_userinfo(self):
        self.assertEqual(normalize_repo_identity("git@github:owner/name.git"), "owner/name")
        self.assertEqual(normalize_repo_identity("github:owner/name.git"), "owner/name")
        self.assertIsNone(normalize_repo_identity("git@github:owner"))

    def test_only_dispatch_shaped_fleet_ids_are_used_as_evidence(self):
        dispatch_id = "0123456789abcdefabcd"
        self.assertEqual(_fleet_id(f"/worktrees/fleet-{dispatch_id}/repo"), dispatch_id)
        self.assertIsNone(_fleet_id("/worktrees/fleet-not-a-real-dispatch/repo"))
        self.assertIsNone(_fleet_id(f"/worktrees/fleet-{'a' * 21}/repo"))

        result = self._read_claude_calls(
            [("head", "git rev-parse HEAD", "abcdef0")],
            cwd="/worktrees/fleet-not-a-real-dispatch/repo",
        )
        self.assertEqual(result.session_fingerprints[0]["evidence"], "git_rev_parse_head")
        self.assertNotIn("not-a-real-dispatch", repr(result.session_fingerprints))

    def test_plain_and_porcelain_worktree_lists_parse_per_block(self):
        dispatch_id = "0123456789abcdefabcd"
        result = self._read_claude_calls(
            [
                (
                    "plain-worktrees",
                    "git worktree list",
                    "/home/fixture/project/main  1111111 [main]\n"
                    f"/home/fixture/project/worktrees/fleet-{dispatch_id}/task  2222222 [task]\n",
                ),
                (
                    "porcelain-worktrees",
                    "git worktree list --porcelain",
                    "worktree /home/fixture/project/main\nHEAD 3333333\nbranch refs/heads/main\n\n"
                    f"worktree /home/fixture/project/worktrees/fleet-{dispatch_id}/task\n"
                    "HEAD 4444444\nbranch refs/heads/task\n\n",
                ),
            ],
            cwd=f"/home/fixture/project/worktrees/fleet-{dispatch_id}/session",
        )
        evidence = {item["value"]: item["evidence"] for item in result.session_fingerprints}
        self.assertEqual(evidence["1111111"], "git_worktree_head")
        self.assertEqual(
            evidence["2222222"], f"git_worktree_head:fleet_worktree={dispatch_id}"
        )
        self.assertEqual(evidence["3333333"], "git_worktree_head")
        self.assertEqual(
            evidence["4444444"], f"git_worktree_head:fleet_worktree={dispatch_id}"
        )

    def test_plain_worktree_list_parses_single_worktree_output(self):
        # Captured from `git worktree list` in a single-worktree repository.
        result = self._read_claude_calls(
            [("single-worktree", "git worktree list", "/tmp/r/repo d99b100 [master]\n")]
        )
        evidence = {item["value"]: item["evidence"] for item in result.session_fingerprints}
        self.assertEqual(evidence, {"d99b100": "git_worktree_head"})

    def test_plain_worktree_list_parses_longest_path_output(self):
        # Real longest-path row from `git worktree list`, with a single-space
        # delimiter before its abbreviated HEAD.
        output = (
            "/home/coolhenrylinux/src/gascity-worktrees/fix-tracking-retention-bounded"
            " 32b367f83 [fix/order-tracking-retention-bounded]\n"
        )
        result = self._read_claude_calls(
            [("longest-path", "git worktree list", output)]
        )
        evidence = {item["value"]: item["evidence"] for item in result.session_fingerprints}
        self.assertEqual(evidence, {"32b367f83": "git_worktree_head"})

    def test_push_without_its_own_fleet_id_does_not_inherit_and_notes_ambiguous_head(self):
        dispatch_id = "0123456789abcdefabcd"
        output = (
            "To https://github.com/hoomji/gascity.git\n"
            "1111111..2222222 main -> main\n"
            "2222222..3333333 side -> side\n"
        )
        result = self._read_claude_calls(
            [("push", "git push origin HEAD", output)],
            cwd=f"/home/fixture/project/worktrees/fleet-{dispatch_id}/session",
        )
        evidence = {item["value"]: item["evidence"] for item in result.session_fingerprints}
        self.assertEqual(
            set(evidence["2222222"].split(";")), {"git_push_head", "git_push_previous"}
        )
        self.assertEqual(evidence["3333333"], "git_push_head")
        self.assertNotIn(dispatch_id, repr(evidence))
        pushed_result = next(
            record
            for record in result.records
            if record["kind"] == "tool_result" and record.get("tool_call_id") == "push"
        )
        self.assertIsNone(pushed_result["commit_sha"])
        notes = " ".join(result.session_evidence_notes["git-evidence-synthetic"])
        self.assertIn("commit_sha:none (multi-ref push", notes)

    def test_default_git_log_commit_lines_are_fingerprinted(self):
        sha = "5555555555555555555555555555555555555555"
        result = self._read_claude_calls(
            [("log", "git log", f"commit {sha} (HEAD -> main)\nAuthor: Synthetic\n")]
        )
        fingerprint = next(item for item in result.session_fingerprints if item["value"] == sha)
        self.assertIn("git_log", fingerprint["evidence"])

    def test_commit_output_branch_names_may_contain_right_brackets(self):
        sha = "6666666"
        result = self._read_claude_calls(
            [("commit", "git commit -m synthetic", f"[topic/with]bracket {sha}] synthetic commit\n")]
        )
        self.assertIn(sha, {item["value"] for item in result.session_fingerprints})

    def test_commit_output_does_not_take_sha_from_commit_subject(self):
        result = self._read_claude_calls(
            [
                (
                    "commit",
                    "git commit -m synthetic",
                    "[main 1111111] check 2222222] tail\n",
                )
            ]
        )
        self.assertEqual(
            {item["value"] for item in result.session_fingerprints}, {"1111111"}
        )


if __name__ == "__main__":
    unittest.main()
