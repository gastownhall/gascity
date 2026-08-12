import pathlib
import unittest


WORKFLOW = pathlib.Path(__file__).parents[1] / "remove-needs-triage.yml"


class RemoveNeedsTriagePolicyTests(unittest.TestCase):
    def test_olivia_label_events_do_not_run_the_automatic_removal_job(self) -> None:
        lines = WORKFLOW.read_text(encoding="utf-8").splitlines()

        self.assertIn(
            "    if: github.event.sender.login != 'gascityinc-olivia[bot]'",
            lines,
        )


if __name__ == "__main__":
    unittest.main()
