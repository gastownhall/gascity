import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import worker_report_rollup as rollup
import worker_report_summary as summary_script


class WorkerReportArtifactTests(unittest.TestCase):
    def write_report(self, directory: Path, name: str, payload: dict) -> None:
        directory.mkdir(parents=True, exist_ok=True)
        with open(directory / name, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, indent=2)
            handle.write("\n")

    def report_payload(self, results: list[dict], suite: str = "phase2") -> dict:
        total = len(results)
        passed = sum(1 for result in results if result["status"] == "pass")
        failed = sum(1 for result in results if result["status"] == "fail")
        unsupported = sum(1 for result in results if result["status"] == "unsupported")
        environment_errors = sum(
            1 for result in results if result["status"] == "environment_error"
        )
        provider_incidents = sum(
            1 for result in results if result["status"] == "provider_incident"
        )
        flaky_live = sum(1 for result in results if result["status"] == "flaky_live")
        not_certifiable_live = sum(
            1 for result in results if result["status"] == "not_certifiable_live"
        )
        failing_requirements = [
            result["requirement"] for result in results if result["status"] != "pass"
        ]
        top_evidence = []
        for result in results:
            evidence = result.get("evidence") or {}
            if result["status"] == "pass" or not evidence:
                continue
            keys = sorted(evidence)
            top_evidence.append(
                {
                    "profile": result["profile"],
                    "requirement": result["requirement"],
                    "status": result["status"],
                    "detail": result.get("detail", ""),
                    "keys": keys,
                    "excerpt": " / ".join(f"{key}={evidence[key]}" for key in keys[:3]),
                }
            )

        return {
            "schema_version": "gc.worker.conformance.v1",
            "suite": suite,
            "run_id": f"{suite}-test",
            "metadata": {"suite": suite, "profile_filter": "all-profiles"},
            "summary": {
                "status": "fail" if failed else "pass",
                "total": total,
                "passed": passed,
                "failed": failed,
                "unsupported": unsupported,
                "environment_errors": environment_errors,
                "provider_incidents": provider_incidents,
                "flaky_live": flaky_live,
                "not_certifiable_live": not_certifiable_live,
                "suite_failed": False,
                "profiles": 0,
                "requirements": 0,
                "failing_requirements": failing_requirements,
                "top_evidence": top_evidence,
            },
            "results": results,
        }

    def test_summary_prints_top_evidence_and_hooks(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            report_dir = Path(tmp) / "reports"
            summary_path = Path(tmp) / "summary.md"
            self.write_report(
                report_dir,
                "phase2-codex.json",
                self.report_payload(
                    [
                        {
                            "profile": "codex/tmux-cli",
                            "requirement": "WC-INT-001",
                            "status": "fail",
                            "detail": "missing interaction signal",
                            "evidence": {
                                "transcript_path": "/tmp/transcript.jsonl",
                                "state_path": "/tmp/state.json",
                            },
                        }
                    ]
                ),
            )
            payload_path = report_dir / "phase2-codex.json"
            payload = json.loads(payload_path.read_text(encoding="utf-8"))
            payload["summary"]["hooks"] = [
                {"name": "live_smoke", "suite": "worker-inference"},
                {"name": "e2e_smoke", "suite": "worker-e2e-smoke"},
            ]
            payload_path.write_text(
                json.dumps(payload, indent=2) + "\n",
                encoding="utf-8",
            )

            env = os.environ.copy()
            env["GITHUB_STEP_SUMMARY"] = str(summary_path)
            with patch.dict(os.environ, env, clear=True), patch.object(
                sys, "argv", ["worker_report_summary.py", str(report_dir)]
            ):
                self.assertEqual(summary_script.main(), 0)

            content = summary_path.read_text(encoding="utf-8")
            self.assertIn("top evidence", content)
            self.assertIn("planned hooks", content)
            self.assertIn("transcript_path=/tmp/transcript.jsonl", content)

    def test_rollup_builds_baseline_delta_and_hooks(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            current_dir = tmp_path / "current"
            baseline_dir = tmp_path / "baseline"
            current_report = self.report_payload(
                [
                    {
                        "profile": "claude/tmux-cli",
                        "requirement": "WC-START-001",
                        "status": "pass",
                    },
                    {
                        "profile": "codex/tmux-cli",
                        "requirement": "WC-INT-001",
                        "status": "fail",
                        "detail": "missing interaction signal",
                        "evidence": {
                            "error": "blocked",
                            "transcript_path": "/tmp/transcript.jsonl",
                            "workspace_dir": "/tmp/workspace",
                        },
                    },
                    {
                        "profile": "gemini/tmux-cli",
                        "requirement": "WC-INPUT-001",
                        "status": "unsupported",
                    },
                ]
            )
            baseline_report = self.report_payload(
                [
                    {
                        "profile": "claude/tmux-cli",
                        "requirement": "WC-START-001",
                        "status": "fail",
                        "detail": "startup mismatch",
                        "evidence": {
                            "state_path": "/tmp/state.json",
                            "launch_state": "blocked",
                        },
                    },
                    {
                        "profile": "codex/tmux-cli",
                        "requirement": "WC-INT-001",
                        "status": "unsupported",
                    },
                    {
                        "profile": "gemini/tmux-cli",
                        "requirement": "WC-INPUT-001",
                        "status": "unsupported",
                    },
                ]
            )
            self.write_report(current_dir, "phase2-current.json", current_report)
            self.write_report(baseline_dir, "phase2-baseline.json", baseline_report)

            current = rollup.build_rollup(
                [str(current_dir / "phase2-current.json")],
                str(current_dir),
                "Worker core summary",
                {},
                rollup.load_baseline_state(str(baseline_dir)),
            )

            summary = current["summary"]
            self.assertEqual(summary["status"], "fail")
            self.assertEqual(summary["hooks"], rollup.PLANNED_HOOKS)
            self.assertGreaterEqual(len(summary["top_evidence"]), 1)
            self.assertEqual(summary["top_evidence"][0]["profile"], "codex/tmux-cli")
            self.assertIn("transcript_path", summary["top_evidence"][0]["excerpt"])
            self.assertGreaterEqual(
                summary["top_evidence_keys"][0]["count"],
                1,
            )

            baseline = summary["baseline"]
            self.assertEqual(baseline["status_counts"]["fail"], 1)
            delta = summary["delta"]
            self.assertEqual(delta["total_reports"], 0)
            self.assertTrue(all(value == 0 for value in delta["status_counts"].values()))
            self.assertEqual(
                delta["newly_passing_requirements"][0]["requirement"],
                "WC-START-001",
            )
            self.assertEqual(
                delta["newly_failing_requirements"][0]["requirement"],
                "WC-INT-001",
            )
            self.assertTrue(delta["changed_support_classifications"])

    def test_rollup_counts_evidence_keys_beyond_top_evidence_limit(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            report_dir = Path(tmp) / "reports"
            results = []
            for i in range(rollup.TOP_EVIDENCE_LIMIT + 1):
                results.append(
                    {
                        "profile": "codex/tmux-cli",
                        "requirement": f"WC-INT-{i:03d}",
                        "status": "fail",
                        "detail": "missing evidence",
                        "evidence": {f"evidence_key_{i:02d}": f"value-{i}"},
                    }
                )
            self.write_report(report_dir, "phase2-codex.json", self.report_payload(results))

            payload = rollup.build_rollup(
                [str(report_dir / "phase2-codex.json")],
                str(report_dir),
                "Worker core summary",
                {},
            )

            evidence_keys = {
                item["key"]: item["count"]
                for item in payload["summary"]["top_evidence_keys"]
            }
            self.assertEqual(len(payload["summary"]["top_evidence"]), rollup.TOP_EVIDENCE_LIMIT)
            self.assertIn(
                f"evidence_key_{rollup.TOP_EVIDENCE_LIMIT:02d}",
                evidence_keys,
            )
            self.assertEqual(
                evidence_keys[f"evidence_key_{rollup.TOP_EVIDENCE_LIMIT:02d}"],
                1,
            )


if __name__ == "__main__":
    unittest.main()
