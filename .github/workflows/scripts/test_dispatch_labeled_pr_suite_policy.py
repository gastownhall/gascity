from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / "dispatch-labeled-pr-suite.yml"


class DispatchLabeledPRSuitePolicyTests(unittest.TestCase):
    def test_dispatch_accepts_github_success_statuses(self) -> None:
        workflow = WORKFLOW.read_text()

        self.assertIn("if response.status not in {200, 204}:", workflow)
        self.assertIn("unexpected dispatch status", workflow)


if __name__ == "__main__":
    unittest.main()
