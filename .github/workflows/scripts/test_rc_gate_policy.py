from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / "rc-gate.yml"


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
        self.assertIn("max-parallel: 1", acceptance_c)

        integration = _job_block(workflow, "ubuntu_integration_shards")
        self.assertIn("needs: ubuntu_acceptance_c", integration)
        self.assertIn("max-parallel: 4", integration)

        tutorial = _job_block(workflow, "ubuntu_tutorial")
        self.assertIn("needs: ubuntu_integration_shards", tutorial)
        self.assertIn("max-parallel: 1", tutorial)


if __name__ == "__main__":
    unittest.main()
