from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / "dispatch-mac-regression.yml"


class DispatchMacRegressionPolicyTests(unittest.TestCase):
    def test_mac_regression_job_requires_trusted_author(self) -> None:
        workflow = WORKFLOW.read_text()

        self.assertIn(
            'trusted = association in {"OWNER", "MEMBER", "COLLABORATOR"} or author.lower() in allowlist',
            workflow,
        )
        self.assertIn("needs.checkout-base.outputs.trusted == 'true'", workflow)
        self.assertIn(".github/blacksmith-allowlist.txt", workflow)

    def test_trust_output_is_wired_from_checkout_base_to_mac_regression(self) -> None:
        workflow = WORKFLOW.read_text()

        self.assertIn("outputs:\n      trusted: ${{ steps.trust-check.outputs.trusted }}", workflow)
        self.assertIn("id: trust-check", workflow)

    def test_trust_gate_is_attached_to_the_mac_regression_job(self) -> None:
        workflow = WORKFLOW.read_text()

        self.assertIn(
            "needs: checkout-base\n    if: needs.checkout-base.outputs.trusted == 'true'",
            workflow,
            "the trust-check output must gate the mac-regression job itself, "
            "immediately after its needs: checkout-base line",
        )


if __name__ == "__main__":
    unittest.main()
