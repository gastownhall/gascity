import shlex
import tempfile
import unittest
from pathlib import Path

import runner_policy


class RunnerPolicyTests(unittest.TestCase):
    def test_bazel_uses_the_test_runners_private_home(self) -> None:
        root = Path(__file__).resolve().parents[3]
        for path in (".bazelrc", ".github/workflows/bazel-test.yml"):
            with self.subTest(path=path):
                content = (root / path).read_text()
                self.assertNotIn(
                    "--test_env=HOME",
                    content,
                    "Bazel must supply HOME from each action's TEST_TMPDIR; "
                    "a client-side directory may not exist in the sandbox",
                )

    def test_bazel_gives_script_fixtures_a_private_var_tmp(self) -> None:
        root = Path(__file__).resolve().parents[3]
        options = [
            shlex.split(line, comments=True)
            for line in (root / ".bazelrc").read_text().splitlines()
        ]
        self.assertIn(["test", "--sandbox_tmpfs_path=/var/tmp"], options)
        self.assertNotIn(["test", "--sandbox_writable_path=/var/tmp"], options)

    def test_bazel_ownership_suites_use_real_host_uids(self) -> None:
        root = Path(__file__).resolve().parents[3]
        for package in ("gchome", "productmetrics"):
            with self.subTest(package=package):
                build = (root / "internal" / package / "BUILD.bazel").read_text()
                test_rule = build.split("go_test(", 1)[1].split("\n)", 1)[0]
                self.assertIn('"no-sandbox"', test_rule)
                self.assertIn('"no-remote-exec"', test_rule)

    def test_bazel_creates_sandbox_writable_tmpdir_before_test(self) -> None:
        root = Path(__file__).resolve().parents[3]
        workflow = (root / ".github/workflows/bazel-test.yml").read_text()
        setup = workflow.split('bazel "${ARGS[@]}" test', 1)
        self.assertEqual(len(setup), 2, "Bazel test command must be present")
        created = set()
        for line in setup[0].splitlines():
            if line.strip().startswith("mkdir -p "):
                created.update(shlex.split(line.split("&&", 1)[0].strip())[2:])
        self.assertIn(
            "/tmp/bt",
            created,
            "--sandbox_writable_path requires /tmp/bt to exist before actions start",
        )

    def test_load_allowlist_ignores_comments_and_case_normalizes(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "allowlist.txt"
            path.write_text(
                "julianknutsen\n"
                "  Csells  # maintainer\n"
                "\n"
                "# comment\n",
                encoding="utf-8",
            )

            self.assertEqual(runner_policy.load_allowlist(path), {"julianknutsen", "csells"})

    def test_pull_request_from_allowlisted_author_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "Quad341",
            {"quad341"},
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("allowlist", reason)
        self.assertEqual(runners["runner_32vcpu"], "blacksmith-32vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-12vcpu-macos-15")

    def test_push_uses_github_even_for_allowlisted_author(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "push",
            "julianknutsen",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("approved pull requests", reason)
        self.assertEqual(runners["runner_32vcpu"], "ubuntu-latest")

    def test_forced_workflow_call_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "workflow_call",
            "",
            set(),
            force_blacksmith=True,
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("forced", reason)
        self.assertEqual(runners["runner_16vcpu"], "blacksmith-16vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-12vcpu-macos-15")

    def test_unlisted_pull_request_author_uses_github(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "external-contributor",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("not on the Blacksmith allowlist", reason)
        self.assertEqual(runners["runner_macos"], "macos-15")


if __name__ == "__main__":
    unittest.main()
