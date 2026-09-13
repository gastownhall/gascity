from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / "rc-gate.yml"
MAC_WORKFLOW = Path(__file__).resolve().parents[1] / "mac-regression.yml"


def _job_block(workflow: str, job_name: str) -> str:
    marker = f"  {job_name}:\n"
    start = workflow.index(marker)
    lines = workflow[start:].splitlines(keepends=True)
    block = [lines[0]]
    for line in lines[1:]:
        if line.startswith("  ") and not line.startswith("    ") and line.strip().endswith(":"):
            break
        block.append(line)
    return "".join(block)


class RCGatePolicyTests(unittest.TestCase):
    def test_real_inference_jobs_are_throttled_after_ci_parity(self) -> None:
        workflow = WORKFLOW.read_text()

        acceptance_a = _job_block(workflow, "ubuntu_acceptance_a")
        self.assertIn("needs: ci_parity", acceptance_a)
        self.assertIn("max-parallel: 2", acceptance_a)

        acceptance_c = _job_block(workflow, "ubuntu_acceptance_c")
        self.assertIn("needs: ubuntu_acceptance_a", acceptance_c)
        self.assertIn("max-parallel: 2", acceptance_c)

        integration = _job_block(workflow, "ubuntu_integration_shards")
        self.assertIn("needs: ubuntu_acceptance_c", integration)
        self.assertIn("max-parallel: 8", integration)
        self.assertIn("shard_name: review-formulas-basic-2-of-2", integration)
        self.assertIn("timeout_minutes: 35", integration)

        tutorial = _job_block(workflow, "ubuntu_tutorial")
        self.assertIn("needs: ubuntu_integration_shards", tutorial)
        self.assertIn("max-parallel: 2", tutorial)

    def test_rc_gate_runs_full_mac_regression_workflow(self) -> None:
        workflow = WORKFLOW.read_text()

        self.assertNotIn("macos_fast_tests:", workflow)

        mac_regression = _job_block(workflow, "mac_regression")
        self.assertIn("uses: ./.github/workflows/mac-regression.yml", mac_regression)
        self.assertIn("suite: full", mac_regression)
        self.assertIn("force_blacksmith: true", mac_regression)

        summary = _job_block(workflow, "rc_summary")
        self.assertIn("- mac_regression", summary)

    def test_mac_regression_is_reusable_by_rc_gate(self) -> None:
        workflow = MAC_WORKFLOW.read_text()

        self.assertIn("workflow_call:", workflow)
        self.assertIn("force_blacksmith:", workflow)
        self.assertIn("FORCE_BLACKSMITH: ${{ inputs.force_blacksmith }}", workflow)


if __name__ == "__main__":
    unittest.main()
