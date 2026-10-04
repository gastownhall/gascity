import contextlib
import io
import json
import unittest

import main_health_escalate as escalate_script


class FakeRun:
    """Stand-in for subprocess.run that serves canned results by argv prefix."""

    def __init__(self):
        self.calls = []
        self._results = []

    def queue(self, argv_prefix, returncode=0, stdout="", stderr=""):
        self._results.append((argv_prefix, returncode, stdout, stderr))

    def __call__(self, argv, **kwargs):
        self.calls.append(argv)
        for prefix, returncode, stdout, stderr in self._results:
            if argv[: len(prefix)] == prefix:
                if kwargs.get("check") and returncode != 0:
                    raise escalate_script.subprocess.CalledProcessError(returncode, argv)
                return escalate_script.subprocess.CompletedProcess(
                    argv, returncode, stdout=stdout, stderr=stderr
                )
        raise AssertionError(f"FakeRun: no queued result for {argv!r}")


def watchdog_issue_body(sha):
    """The body the watchdog itself files for a broken-main SHA."""
    return escalate_script.build_issue_body(
        sha=sha, author="Jane Dev", pr_number="", run_url="https://example/runs/1"
    )


def listed_issue(number, body):
    """One entry of `gh issue list --json url,body` output."""
    return {"url": f"https://github.com/o/r/issues/{number}", "body": body}


def calls_to(fake, *argv_prefix):
    """The recorded gh/git invocations whose argv starts with argv_prefix."""
    return [c for c in fake.calls if c[: len(argv_prefix)] == list(argv_prefix)]


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


class BuildCommentBodyTests(unittest.TestCase):
    def test_includes_sha_author_pr_and_run_url(self):
        body = escalate_script.build_comment_body(
            sha="bbbb2222",
            author="Sam Dev <sam@example.com>",
            pr_number="5051",
            run_url="https://github.com/gastownhall/gc-management/actions/runs/124",
        )
        self.assertIn("bbbb2222", body)
        self.assertIn("Sam Dev <sam@example.com>", body)
        self.assertIn("#5051", body)
        self.assertIn("https://github.com/gastownhall/gc-management/actions/runs/124", body)

    def test_omits_pr_line_when_no_pr_associated(self):
        body = escalate_script.build_comment_body(
            sha="bbbb2222", author="Sam Dev", pr_number="", run_url="https://example/runs/2"
        )
        self.assertNotIn("PR:", body)


class FindOpenEscalationIssueTests(unittest.TestCase):
    def test_returns_url_of_the_open_issue_the_watchdog_filed(self):
        # Round trip: the finder must recognise exactly what build_issue_body writes.
        fake = FakeRun()
        fake.queue(
            ["gh", "issue", "list"],
            stdout=json.dumps([listed_issue(42, watchdog_issue_body("aaaa1111"))]),
        )
        result = escalate_script.find_open_escalation_issue(run=fake)
        self.assertEqual(result, "https://github.com/o/r/issues/42")

    def test_skips_open_p0_issues_the_watchdog_did_not_file(self):
        fake = FakeRun()
        fake.queue(
            ["gh", "issue", "list"],
            stdout=json.dumps(
                [
                    listed_issue(7, "Scheduler stalls under load."),
                    listed_issue(42, watchdog_issue_body("aaaa1111")),
                ]
            ),
        )
        result = escalate_script.find_open_escalation_issue(run=fake)
        self.assertEqual(result, "https://github.com/o/r/issues/42")

    def test_returns_none_when_no_open_issue_is_the_watchdogs(self):
        fake = FakeRun()
        fake.queue(
            ["gh", "issue", "list"],
            stdout=json.dumps([listed_issue(7, "Scheduler stalls under load.")]),
        )
        self.assertIsNone(escalate_script.find_open_escalation_issue(run=fake))

    def test_returns_none_when_there_are_no_open_p0_issues(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], stdout="[]")
        self.assertIsNone(escalate_script.find_open_escalation_issue(run=fake))

    def test_lists_only_open_issues_carrying_the_p0_label(self):
        # An issue a human already closed must not suppress a new escalation, and the
        # label scope keeps the lookup off the repo's ordinary issue backlog.
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], stdout="[]")
        escalate_script.find_open_escalation_issue(run=fake)
        (call,) = fake.calls
        self.assertEqual(call[call.index("--state") + 1], "open")
        self.assertEqual(call[call.index("--label") + 1], escalate_script.P0_LABEL)
        json_fields = call[call.index("--json") + 1].split(",")
        self.assertIn("url", json_fields)
        self.assertIn("body", json_fields)

    def test_warns_on_stderr_and_returns_none_when_gh_fails(self):
        # Alerting must not be lost to a lookup failure, but the failure must not be
        # silent either: it lands in the workflow log so a possible duplicate is explicable.
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], returncode=1, stderr="HTTP 502: bad gateway")
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            result = escalate_script.find_open_escalation_issue(run=fake)
        self.assertIsNone(result)
        self.assertIn("could not list open escalation issues", stderr.getvalue())
        self.assertIn("HTTP 502: bad gateway", stderr.getvalue())

    def test_warns_on_stderr_and_returns_none_when_gh_output_is_not_json(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], stdout="<html>rate limited</html>")
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            result = escalate_script.find_open_escalation_issue(run=fake)
        self.assertIsNone(result)
        self.assertIn("could not list open escalation issues", stderr.getvalue())


class CommentOnIssueTests(unittest.TestCase):
    def test_comments_on_the_issue_by_url(self):
        issue_url = "https://github.com/o/r/issues/42"
        fake = FakeRun()
        fake.queue(["gh", "issue", "comment"])
        escalate_script.comment_on_issue(issue_url, "still broken", run=fake)
        self.assertEqual(
            fake.calls, [["gh", "issue", "comment", issue_url, "--body", "still broken"]]
        )

    def test_raises_when_gh_fails(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "comment"], returncode=1)
        with self.assertRaises(escalate_script.subprocess.CalledProcessError):
            escalate_script.comment_on_issue("https://github.com/o/r/issues/42", "body", run=fake)


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

    def test_p0_label_matches_repo_priority_label_convention(self):
        # gastownhall/gascity's real label taxonomy is priority/p0..priority/p3
        # (no bare "P0" label exists) -- gh issue create --label errors on an
        # unrecognized label, so the constant must match the repo convention
        # or every escalation issue-create call fails outright.
        self.assertEqual(escalate_script.P0_LABEL, "priority/p0")


class EscalateTests(unittest.TestCase):
    def test_creates_new_issue_when_none_exists(self):
        fake = FakeRun()
        fake.queue(["gh", "issue", "list"], stdout="[]")
        fake.queue(["git", "log", "-1", "--format=%an <%ae>"], stdout="Jane Dev <jane@example.com>")
        fake.queue(["git", "log", "-1", "--format=%B"], stdout="Merge pull request #5039 from x/y")
        fake.queue(["gh", "issue", "create"], stdout="https://github.com/o/r/issues/44")

        url = escalate_script.escalate(sha="cc036a76e", run_url="https://example/runs/9", run=fake)

        self.assertEqual(url, "https://github.com/o/r/issues/44")
        self.assertEqual(len(calls_to(fake, "gh", "issue", "create")), 1)
        self.assertEqual(calls_to(fake, "gh", "issue", "comment"), [])

    def test_later_failing_push_comments_on_the_open_issue_instead_of_filing_another(self):
        # main is still broken, now at a different commit than the one the open issue
        # was filed for: that is the same failure, so it gets a comment, not a new P0.
        open_issue = "https://github.com/o/r/issues/40"
        fake = FakeRun()
        fake.queue(
            ["gh", "issue", "list"],
            stdout=json.dumps([{"url": open_issue, "body": watchdog_issue_body("aaaa1111")}]),
        )
        fake.queue(["git", "log", "-1", "--format=%an <%ae>"], stdout="Sam Dev <sam@example.com>")
        fake.queue(["git", "log", "-1", "--format=%B"], stdout="Merge pull request #5051 from x/y")
        fake.queue(["gh", "issue", "comment"])

        url = escalate_script.escalate(sha="bbbb2222", run_url="https://example/runs/9", run=fake)

        self.assertEqual(url, open_issue)
        self.assertEqual(calls_to(fake, "gh", "issue", "create"), [])
        (comment,) = calls_to(fake, "gh", "issue", "comment")
        self.assertEqual(comment[3], open_issue)
        body = comment[comment.index("--body") + 1]
        self.assertIn("bbbb2222", body)
        self.assertIn("#5051", body)
        self.assertIn("https://example/runs/9", body)

    def test_comment_failure_is_not_papered_over_by_filing_a_duplicate(self):
        fake = FakeRun()
        fake.queue(
            ["gh", "issue", "list"],
            stdout=json.dumps([listed_issue(40, watchdog_issue_body("aaaa1111"))]),
        )
        fake.queue(["git", "log", "-1", "--format=%an <%ae>"], stdout="Sam Dev <sam@example.com>")
        fake.queue(["git", "log", "-1", "--format=%B"], stdout="fix: unrelated")
        fake.queue(["gh", "issue", "comment"], returncode=1)

        with self.assertRaises(escalate_script.subprocess.CalledProcessError):
            escalate_script.escalate(sha="bbbb2222", run_url="https://example/runs/9", run=fake)

        self.assertEqual(calls_to(fake, "gh", "issue", "create"), [])


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
