import tempfile
import unittest
from pathlib import Path

import runner_policy


class RunnerPolicyTests(unittest.TestCase):
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
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("approved pull requests", reason)
        self.assertEqual(runners["runner_32vcpu"], "ubuntu-latest")

    def test_unlisted_pull_request_author_uses_github(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "external-contributor",
            {"julianknutsen"},
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("not on the Blacksmith allowlist", reason)
        self.assertEqual(runners["runner_macos"], "macos-15")


if __name__ == "__main__":
    unittest.main()
