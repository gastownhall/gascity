import json
import unittest

import main_health_escalate as escalate_script


class FakeRun:
    """Stand-in for subprocess.run that serves canned results by argv prefix."""

    def __init__(self):
        self.calls = []
        self._results = []

    def queue(self, argv_prefix, returncode=0, stdout=""):
        self._results.append((argv_prefix, returncode, stdout))

    def __call__(self, argv, **kwargs):
        self.calls.append(argv)
        for prefix, returncode, stdout in self._results:
            if argv[: len(prefix)] == prefix:
                if kwargs.get("check") and returncode != 0:
                    raise escalate_script.subprocess.CalledProcessError(returncode, argv)
                return escalate_script.subprocess.CompletedProcess(
                    argv, returncode, stdout=stdout, stderr=""
                )
        raise AssertionError(f"FakeRun: no queued result for {argv!r}")


class ParsePrNumberTests(unittest.TestCase):
    def test_extracts_pr_number_from_merge_commit_message(self):
        message = "Merge pull request #5039 from gastownhall/fix/ga-usd3k-step-completed"
        self.assertEqual(escalate_script.parse_pr_number(message), "5039")

    def test_returns_empty_string_when_not_a_merge_commit(self):
        message = "fix(runtime): add missing hash/fnv import to city_runtime.go"
        self.assertEqual(escalate_script.parse_pr_number(message), "")


class BuildIssueTitleTests(unittest.TestCase):
    def test_includes_short_sha(self):
        title = escalate_script.build_issue_title("cc036a76e6b3f0a1b2c3d4e5f60718293a4b5c6")
        self.assertIn("cc036a76e6b3", title)

    def test_does_not_include_full_sha(self):
        full_sha = "cc036a76e6b3f0a1b2c3d4e5f60718293a4b5c6"
        title = escalate_script.build_issue_title(full_sha)
        self.assertNotIn(full_sha, title)


class BuildIssueBodyTests(unittest.TestCase):
    def test_includes_sha_author_pr_and_run_url(self):
        body = escalate_script.build_issue_body(
            sha="cc036a76e",
            author="Jane Dev <jane@example.com>",
            pr_number="5039",
            run_url="https://github.com/gastownhall/gc-management/actions/runs/123",
        )
        self.assertIn("cc036a76e", body)
        self.assertIn("Jane Dev <jane@example.com>", body)
        self.assertIn("#5039", body)
        self.assertIn("https://github.com/gastownhall/gc-management/actions/runs/123", body)

    def test_omits_pr_line_when_no_pr_associated(self):
        body = escalate_script.build_issue_body(
            sha="cc036a76e", author="Jane Dev", pr_number="", run_url="https://example/runs/1"
        )
        self.assertNotIn("PR:", body)

    def test_includes_dedup_marker_for_this_sha(self):
        body = escalate_script.build_issue_body(
            sha="cc036a76e", author="Jane Dev", pr_number="", run_url="https://example/runs/1"
        )
        self.assertIn(f"{escalate_script.ESCALATION_MARKER}:cc036a76e", body)

    def test_states_escalate_only_no_revert(self):
        body = escalate_script.build_issue_body(
            sha="cc036a76e", author="Jane Dev", pr_number="", run_url="https://example/runs/1"
        )
        self.assertIn("nothing has been reverted", body.lower())


class FindExistingIssueTests(unittest.TestCase):
    def test_returns_url_when_gh_reports_a_match(self):
        fake = FakeRun()
        fake.queue(
            ["gh", "issue", "list"],
            stdout=json.dumps([{"url": "https://github.com/o/r/issues/42"}]),
        )
        result = escalate_script.find_existing_issue("cc036a76e", run=fake)
        self.assertEqual(result, "https://github.com/o/r/issues/42")

    def test_returns_none_when_gh_reports_no_matches(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], stdout="[]")
        result = escalate_script.find_existing_issue("cc036a76e", run=fake)
        self.assertIsNone(result)

    def test_returns_none_when_gh_command_fails(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], returncode=1, stdout="")
        result = escalate_script.find_existing_issue("cc036a76e", run=fake)
        self.assertIsNone(result)

    def test_search_scopes_to_this_shas_marker(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], stdout="[]")
        escalate_script.find_existing_issue("cc036a76e", run=fake)
        (call,) = fake.calls
        search_value = call[call.index("--search") + 1]
        self.assertIn(f"{escalate_script.ESCALATION_MARKER}:cc036a76e", search_value)


class CreateIssueTests(unittest.TestCase):
    def test_returns_url_printed_by_gh(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "create"], stdout="https://github.com/o/r/issues/43\n")
        url = escalate_script.create_issue("title", "body", run=fake)
        self.assertEqual(url, "https://github.com/o/r/issues/43")

    def test_passes_p0_label(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "create"], stdout="https://github.com/o/r/issues/43")
        escalate_script.create_issue("title", "body", run=fake)
        (call,) = fake.calls
        self.assertIn(escalate_script.P0_LABEL, call)


class EscalateTests(unittest.TestCase):
    def test_creates_new_issue_when_none_exists(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], stdout="[]")
        fake.queue(["git", "log", "-1", "--format=%an <%ae>"], stdout="Jane Dev <jane@example.com>")
        fake.queue(["git", "log", "-1", "--format=%B"], stdout="Merge pull request #5039 from x/y")
        fake.queue(["gh", "issue", "create"], stdout="https://github.com/o/r/issues/44")

        url = escalate_script.escalate(sha="cc036a76e", run_url="https://example/runs/9", run=fake)

        self.assertEqual(url, "https://github.com/o/r/issues/44")
        create_calls = [c for c in fake.calls if c[:2] == ["gh", "issue"] and "create" in c]
        self.assertEqual(len(create_calls), 1)

    def test_does_not_create_a_duplicate_when_an_issue_already_exists(self):
        fake = FakeRun()
        fake.queue(
            ["gh", "issue", "list"],
            stdout=json.dumps([{"url": "https://github.com/o/r/issues/40"}]),
        )

        url = escalate_script.escalate(sha="cc036a76e", run_url="https://example/runs/9", run=fake)

        self.assertEqual(url, "https://github.com/o/r/issues/40")
        create_calls = [c for c in fake.calls if c[:2] == ["gh", "issue"] and "create" in c]
        self.assertEqual(create_calls, [])


class GatherContextTests(unittest.TestCase):
    def test_builds_run_url_from_github_env_vars(self):
        env = {
            "GITHUB_SHA": "cc036a76e",
            "GITHUB_SERVER_URL": "https://github.com",
            "GITHUB_REPOSITORY": "gastownhall/gc-management",
            "GITHUB_RUN_ID": "123456",
        }
        sha, run_url = escalate_script.gather_context(env)
        self.assertEqual(sha, "cc036a76e")
        self.assertEqual(
            run_url, "https://github.com/gastownhall/gc-management/actions/runs/123456"
        )

    def test_raises_when_sha_missing(self):
        env = {
            "GITHUB_SERVER_URL": "https://github.com",
            "GITHUB_REPOSITORY": "gastownhall/gc-management",
            "GITHUB_RUN_ID": "123456",
        }
        with self.assertRaises(escalate_script.MissingContextError):
            escalate_script.gather_context(env)

    def test_raises_when_run_url_components_missing(self):
        env = {"GITHUB_SHA": "cc036a76e"}
        with self.assertRaises(escalate_script.MissingContextError):
            escalate_script.gather_context(env)


class MainTests(unittest.TestCase):
    def test_returns_1_and_prints_error_when_context_missing(self):
        exit_code = escalate_script.main(env={}, run=FakeRun())
        self.assertEqual(exit_code, 1)

    def test_happy_path_creates_issue_and_returns_0(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], stdout="[]")
        fake.queue(["git", "log", "-1", "--format=%an <%ae>"], stdout="Jane Dev <jane@example.com>")
        fake.queue(["git", "log", "-1", "--format=%B"], stdout="fix: unrelated")
        fake.queue(["gh", "issue", "create"], stdout="https://github.com/o/r/issues/45")

        env = {
            "GITHUB_SHA": "cc036a76e",
            "GITHUB_SERVER_URL": "https://github.com",
            "GITHUB_REPOSITORY": "gastownhall/gc-management",
            "GITHUB_RUN_ID": "123456",
        }
        exit_code = escalate_script.main(env=env, run=fake)
        self.assertEqual(exit_code, 0)


if __name__ == "__main__":
    unittest.main()
