#!/usr/bin/env python3
"""Tests for release_gate_freshness.py (ga-wnziq7.1).

Every repo here is a throwaway git repository driven with the real git
binary, so the merge-tree results the check re-derives are git's own, not a
mock's.
"""

from __future__ import annotations

import contextlib
import io
import os
import subprocess
import tempfile
import unittest

import release_gate_freshness as rgf

GIT_ENV = {
    "GIT_CONFIG_NOSYSTEM": "1",
    "GIT_CONFIG_GLOBAL": "/dev/null",
    "GIT_AUTHOR_NAME": "t",
    "GIT_AUTHOR_EMAIL": "t@example.invalid",
    "GIT_COMMITTER_NAME": "t",
    "GIT_COMMITTER_EMAIL": "t@example.invalid",
}


def git(repo: str, *args: str, check: bool = True) -> str:
    proc = subprocess.run(
        ["git", "-C", repo, *args],
        env={**os.environ, **GIT_ENV},
        capture_output=True,
        text=True,
        check=False,
    )
    if check and proc.returncode != 0:
        raise AssertionError(f"git {args} failed: {proc.stderr}")
    return proc.stdout.strip()


def commit_file(repo: str, name: str, content: str, msg: str) -> str:
    path = os.path.join(repo, name)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(content)
    git(repo, "add", name)
    git(repo, "commit", "-q", "-m", msg)
    return git(repo, "rev-parse", "HEAD")


def record_text(base_ref: str, base_sha: str, head: str, tree: str, *, base_line: bool = True, row6: str | None = None) -> str:
    """A gate record shaped like the landed ones: a Base ref line and a criterion 6 table row."""
    if row6 is None:
        row6 = (
            f"| 6 | Branch diverges cleanly from main | **PASS** | "
            f"`git merge-tree --write-tree {base_ref} {head}` exited 0 and produced tree {tree}. |"
        )
    lines = ["# Release gate", ""]
    if base_line:
        lines.append(f"- Base ref: `{base_ref}@{base_sha}`")
    lines += ["", "| # | Criterion | Result | Evidence |", "|---|---|---|---|", row6, ""]
    return "\n".join(lines)


class Fixture:
    """main with one commit, plus a reviewed branch commit that touches a different file."""

    def __init__(self, test: unittest.TestCase):
        tmp = tempfile.TemporaryDirectory()
        test.addCleanup(tmp.cleanup)
        self.repo = tmp.name
        git(self.repo, "init", "-q", "-b", "main")
        self.base = commit_file(self.repo, "a.txt", "one\n", "base")
        git(self.repo, "checkout", "-q", "-b", "feature")
        self.head = commit_file(self.repo, "b.txt", "feature\n", "feature")
        git(self.repo, "checkout", "-q", "main")
        self.tree = git(self.repo, "merge-tree", "--write-tree", "main", self.head).splitlines()[0]

    def text(self, **kw) -> str:
        return record_text("main", self.base, self.head, self.tree, **kw)

    def advance_main(self, name: str, content: str) -> str:
        return commit_file(self.repo, name, content, f"main moves {name}")


class ParseRecordTest(unittest.TestCase):
    def test_extracts_base_head_and_tree(self):
        sha = "a" * 40
        tree = "b" * 40
        head = "c" * 40
        rec = rgf.parse_record(record_text("origin/main", sha, head, tree))
        self.assertEqual(rec.base_ref, "origin/main")
        self.assertEqual(rec.base_sha, sha)
        self.assertEqual(rec.head, head)
        self.assertEqual(rec.tree, tree)

    def test_accepts_messages_flag_and_numbered_row_label(self):
        row6 = f"| 6. Clean merge | PASS | `git merge-tree --write-tree --messages origin/main {'d' * 40}` produced tree {'e' * 40} |"
        rec = rgf.parse_record(record_text("origin/main", "a" * 40, "unused", "unused", row6=row6))
        self.assertEqual(rec.head, "d" * 40)
        self.assertEqual(rec.tree, "e" * 40)

    def test_missing_base_ref_is_an_error(self):
        with self.assertRaisesRegex(rgf.RecordError, "Base ref"):
            rgf.parse_record(record_text("origin/main", "a" * 40, "c" * 40, "b" * 40, base_line=False))

    def test_base_ref_without_pinned_sha_is_an_error(self):
        text = record_text("origin/main", "a" * 40, "c" * 40, "b" * 40).replace(f"@{'a' * 40}", "")
        with self.assertRaisesRegex(rgf.RecordError, "Base ref"):
            rgf.parse_record(text)

    def test_missing_criterion_6_invocation_is_an_error(self):
        row6 = "| 6 | Branch diverges cleanly from main | PASS | looked fine |"
        with self.assertRaisesRegex(rgf.RecordError, "criterion 6"):
            rgf.parse_record(record_text("origin/main", "a" * 40, "c" * 40, "b" * 40, row6=row6))

    def test_head_must_be_a_commit_not_a_symbolic_name(self):
        row6 = f"| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` produced tree {'b' * 40} |"
        with self.assertRaisesRegex(rgf.RecordError, "HEAD"):
            rgf.parse_record(record_text("origin/main", "a" * 40, "x", "x", row6=row6))

    def test_missing_tree_hash_is_an_error(self):
        row6 = f"| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main {'c' * 40}` exited 0. |"
        with self.assertRaisesRegex(rgf.RecordError, "tree"):
            rgf.parse_record(record_text("origin/main", "a" * 40, "x", "x", row6=row6))


class CheckRecordTest(unittest.TestCase):
    def test_fresh_record_passes(self):
        fx = Fixture(self)
        self.assertEqual(rgf.check_record(fx.repo, "release-gates/x.md", fx.text()), [])

    def test_base_moved_fails_and_reports_old_and_new(self):
        fx = Fixture(self)
        new_tip = fx.advance_main("c.txt", "unrelated\n")
        (msg,) = rgf.check_record(fx.repo, "release-gates/x.md", fx.text())
        self.assertIn("release-gates/x.md", msg)
        self.assertIn(fx.base, msg)
        self.assertIn(new_tip, msg)
        self.assertIn(fx.tree, msg)
        new_tree = git(fx.repo, "merge-tree", "--write-tree", "main", fx.head).splitlines()[0]
        self.assertIn(new_tree, msg)

    def test_conflict_with_moved_base_fails_as_not_clean(self):
        fx = Fixture(self)
        fx.advance_main("b.txt", "main wrote b.txt too\n")
        (msg,) = rgf.check_record(fx.repo, "release-gates/x.md", fx.text())
        self.assertIn("not clean", msg)
        self.assertIn("b.txt", msg)

    def test_base_ref_that_no_longer_resolves_fails(self):
        fx = Fixture(self)
        text = record_text("gone-branch", fx.base, fx.head, fx.tree)
        (msg,) = rgf.check_record(fx.repo, "release-gates/x.md", text)
        self.assertIn("gone-branch", msg)
        self.assertIn("resolve", msg)

    def test_cited_head_absent_from_the_repo_fails(self):
        fx = Fixture(self)
        text = record_text("main", fx.base, "f" * 40, fx.tree)
        (msg,) = rgf.check_record(fx.repo, "release-gates/x.md", text)
        self.assertIn("f" * 40, msg)

    def test_missing_base_ref_fails_with_the_record_named(self):
        fx = Fixture(self)
        (msg,) = rgf.check_record(fx.repo, "release-gates/x.md", fx.text(base_line=False))
        self.assertIn("release-gates/x.md", msg)
        self.assertIn("Base ref", msg)


class ChangedRecordsAndMainTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo = tmp.name
        git(self.repo, "init", "-q", "-b", "main")
        self.start = commit_file(self.repo, "a.txt", "one\n", "base")

    def run_main(self, base: str, head: str) -> tuple[int, str]:
        out = io.StringIO()
        cwd = os.getcwd()
        os.chdir(self.repo)
        try:
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
                rc = rgf.main(["--base-sha", base, "--head-sha", head])
        finally:
            os.chdir(cwd)
        return rc, out.getvalue()

    def test_only_added_or_modified_release_gate_markdown_counts(self):
        commit_file(self.repo, "release-gates/old.md", "old\n", "old record")
        mid = git(self.repo, "rev-parse", "HEAD")
        commit_file(self.repo, "release-gates/new.md", "new\n", "new record")
        commit_file(self.repo, "release-gates/old.md", "edited\n", "edit old")
        commit_file(self.repo, "release-gates/notes.txt", "x\n", "not markdown")
        commit_file(self.repo, "docs/other.md", "x\n", "elsewhere")
        head = git(self.repo, "rev-parse", "HEAD")
        self.assertEqual(rgf.changed_records(self.repo, mid, head), ["release-gates/new.md", "release-gates/old.md"])

    def test_deleted_records_are_not_checked(self):
        commit_file(self.repo, "release-gates/old.md", "old\n", "old record")
        mid = git(self.repo, "rev-parse", "HEAD")
        git(self.repo, "rm", "-q", "release-gates/old.md")
        git(self.repo, "commit", "-q", "-m", "drop")
        self.assertEqual(rgf.changed_records(self.repo, mid, git(self.repo, "rev-parse", "HEAD")), [])

    def test_pr_without_release_gate_changes_is_a_quiet_pass(self):
        head = commit_file(self.repo, "src/code.go", "package x\n", "code only")
        rc, out = self.run_main(self.start, head)
        self.assertEqual(rc, 0)
        self.assertIn("no release-gates", out)

    def test_main_fails_naming_the_stale_record(self):
        fx = Fixture(self)
        fx.advance_main("c.txt", "unrelated\n")
        base = git(fx.repo, "rev-parse", "main")
        git(fx.repo, "checkout", "-q", "-b", "pr", "main")
        head = commit_file(fx.repo, "release-gates/x.md", fx.text(), "gate record")
        self.repo = fx.repo
        rc, out = self.run_main(base, head)
        self.assertEqual(rc, 1)
        self.assertIn("release-gates/x.md", out)

    def test_main_passes_on_a_fresh_record(self):
        fx = Fixture(self)
        base = git(fx.repo, "rev-parse", "main")
        git(fx.repo, "checkout", "-q", "-b", "pr", "main")
        head = commit_file(fx.repo, "release-gates/x.md", fx.text(), "gate record")
        self.repo = fx.repo
        rc, out = self.run_main(base, head)
        self.assertEqual(rc, 0, out)


if __name__ == "__main__":
    unittest.main()
