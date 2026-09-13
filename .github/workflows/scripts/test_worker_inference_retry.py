import copy
import sys
import unittest
from pathlib import Path


SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import worker_inference_retry as retry_script


class WorkerInferenceRetryTests(unittest.TestCase):
    def make_report(self, suite: str, results: list[dict], status: str | None = None) -> dict:
        report = {
            "schema_version": "gc.worker.conformance.v1",
            "run_id": f"{suite}-codex-tmux-cli",
            "suite": suite,
            "metadata": {
                "suite": suite,
                "profile_filter": "codex/tmux-cli",
            },
            "summary": {
                "status": status or retry_script.summary_status(
                    retry_script.Counter(result["status"] for result in results)
                ),
                "total": len(results),
                "passed": sum(1 for result in results if result["status"] == "pass"),
                "failed": sum(1 for result in results if result["status"] == "fail"),
                "unsupported": sum(1 for result in results if result["status"] == "unsupported"),
                "environment_errors": sum(
                    1 for result in results if result["status"] == "environment_error"
                ),
                "provider_incidents": sum(
                    1 for result in results if result["status"] == "provider_incident"
                ),
                "flaky_live": sum(1 for result in results if result["status"] == "flaky_live"),
                "not_certifiable_live": sum(
                    1 for result in results if result["status"] == "not_certifiable_live"
                ),
                "profiles": 1,
                "requirements": len(results),
                "failing_profiles": [],
                "failing_requirements": [
                    result["requirement"] for result in results if result["status"] == "fail"
                ],
                "top_evidence": [],
            },
            "results": copy.deepcopy(results),
        }
        return report

    def test_build_retry_plan_delays_rate_limit_provider_incident(self) -> None:
        report = self.make_report(
            "worker-inference",
            [
                {
                    "profile": "codex/tmux-cli",
                    "requirement": "WI-TASK-001",
                    "status": "provider_incident",
                    "detail": "worker is blocked on a provider rate-limit dialog",
                    "evidence": {
                        "blocked_kind": "rate_limit",
                        "pane_tail": "Approaching rate limits",
                    },
                }
            ],
        )

        plan = retry_script.build_retry_plan({"worker-inference-codex.json": report}, delayed_delay=17)

        self.assertIsNotNone(plan)
        self.assertEqual(plan["strategy"], "delayed")
        self.assertEqual(plan["delay_seconds"], 17)

    def test_build_retry_plan_rejects_nonretryable_auth_environment_error(self) -> None:
        report = self.make_report(
            "worker-inference",
            [
                {
                    "profile": "claude/tmux-cli",
                    "requirement": "WI-SPAWN-001",
                    "status": "environment_error",
                    "detail": "Please run /login",
                    "evidence": {"blocked_kind": "authentication"},
                }
            ],
        )

        plan = retry_script.build_retry_plan({"worker-inference-claude.json": report})

        self.assertIsNone(plan)

    def test_merge_retry_reports_marks_retry_pass_as_flaky_live(self) -> None:
        initial_report = self.make_report(
            "worker-inference",
            [
                {
                    "profile": "codex/tmux-cli",
                    "requirement": "WI-SPAWN-001",
                    "status": "pass",
                    "detail": "spawned",
                    "evidence": {"session_name": "probe"},
                },
                {
                    "profile": "codex/tmux-cli",
                    "requirement": "WI-TASK-001",
                    "status": "provider_incident",
                    "detail": "worker is blocked on a provider rate-limit dialog",
                    "evidence": {
                        "blocked_kind": "rate_limit",
                        "pane_tail": "Approaching rate limits",
                    },
                },
            ],
        )
        retry_report = self.make_report(
            "worker-inference",
            [
                {
                    "profile": "codex/tmux-cli",
                    "requirement": "WI-SPAWN-001",
                    "status": "pass",
                    "detail": "spawned",
                    "evidence": {"session_name": "probe"},
                },
                {
                    "profile": "codex/tmux-cli",
                    "requirement": "WI-TASK-001",
                    "status": "pass",
                    "detail": "completed",
                    "evidence": {"output_path": "/tmp/out.txt"},
                },
                {
                    "profile": "codex/tmux-cli",
                    "requirement": "WI-TRANSCRIPT-001",
                    "status": "pass",
                    "detail": "transcript discovered",
                    "evidence": {"transcript_path": "/tmp/t.jsonl"},
                },
            ],
        )
        plan = {
            "strategy": "delayed",
            "delay_seconds": 0,
            "reasons": ["codex/tmux-cli WI-TASK-001 delayed"],
        }

        merged = retry_script.merge_retry_reports(
            {"worker-inference-codex.json": initial_report},
            {"worker-inference-codex.json": retry_report},
            plan,
            1,
            0,
        )
        report = merged["worker-inference-codex.json"]
        self.assertEqual(report["summary"]["status"], "flaky_live")
        self.assertEqual(report["metadata"]["retry_strategy"], "delayed")

        results = {
            (result["profile"], result["requirement"]): result for result in report["results"]
        }
        flaky = results[("codex/tmux-cli", "WI-TASK-001")]
        self.assertEqual(flaky["status"], "flaky_live")
        self.assertIn("retry-pass after initial provider_incident", flaky["detail"])
        self.assertEqual(flaky["evidence"]["retry_initial_status"], "provider_incident")
        self.assertEqual(flaky["evidence"]["retry_strategy"], "delayed")
        self.assertIn("retry_initial_blocked_kind", flaky["evidence"])

    def test_exit_code_for_reports_treats_flaky_live_as_success(self) -> None:
        report = self.make_report(
            "worker-inference",
            [
                {
                    "profile": "codex/tmux-cli",
                    "requirement": "WI-TASK-001",
                    "status": "flaky_live",
                    "detail": "retry-pass after initial provider_incident",
                    "evidence": {"retry_strategy": "delayed"},
                }
            ],
            status="flaky_live",
        )

        code = retry_script.exit_code_for_reports({"worker-inference-codex.json": report}, 1)

        self.assertEqual(code, 0)


if __name__ == "__main__":
    unittest.main()
