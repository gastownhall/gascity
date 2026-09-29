"""End-to-end CLI tests using synthetic fixtures and a temporary SQLite file."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

PACKAGE_ROOT = support.PACKAGE_ROOT


def run_cli(args, cwd=PACKAGE_ROOT):
    env = dict(os.environ)
    env["PYTHONPATH"] = PACKAGE_ROOT + os.pathsep + env.get("PYTHONPATH", "")
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    return subprocess.run(
        [sys.executable, "-m", "agent_observatory", *args],
        cwd=cwd,
        env=env,
        capture_output=True,
        text=True,
    )


class CliTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db = os.path.join(self.tmp.name, "projection.db")
        self.fixture = support.write_jsonl(
            os.path.join(self.tmp.name, "synthetic.jsonl"),
            [
                support.make_record(
                    event_id="e1", timestamp="2026-09-21T10:00:00Z",
                    kind="tool_call", command="go test ./...", tool_call_id="tc1",
                ),
                support.make_record(
                    event_id="e2", timestamp="2026-09-21T10:00:01Z",
                    kind="tool_result", tool_call_id="tc1", exit_code=0,
                ),
                support.make_record(
                    event_id="e3", timestamp="2026-09-21T10:00:02Z",
                    kind="tool_call", command="rg needle", tool_name="rg",
                ),
            ],
        )

    def test_import_report_and_replay_preserves_counts(self):
        first = run_cli(["import-jsonl", "--db", self.db, self.fixture])
        self.assertEqual(first.returncode, 0, first.stderr)
        first_summary = json.loads(first.stdout)["imports"][0]
        self.assertEqual(first_summary["inserted"], 3)

        replay = run_cli(["import-jsonl", "--db", self.db, self.fixture])
        self.assertEqual(replay.returncode, 0, replay.stderr)
        replay_summary = json.loads(replay.stdout)["imports"][0]
        self.assertTrue(replay_summary["skipped_identical_file"])
        self.assertEqual(replay_summary["events"], 3)

        report = run_cli(["report", "--db", self.db])
        self.assertEqual(report.returncode, 0, report.stderr)
        payload = json.loads(report.stdout)
        self.assertEqual(payload["coverage"]["events"], 3)
        self.assertEqual(payload["coverage"]["sessions"], 1)
        self.assertEqual(payload["test"]["invocations"], 1)
        self.assertEqual(payload["test"]["results"], {"passed": 1, "failed": 0, "unknown": 0})

    def test_build_request_and_import_response_round_trip(self):
        self.assertEqual(run_cli(["import-jsonl", "--db", self.db, self.fixture]).returncode, 0)
        state_path = os.path.join(self.tmp.name, "state.json")
        with open(state_path, "w", encoding="utf-8") as handle:
            json.dump({"summary": "ran the test suite"}, handle)

        built = run_cli(
            [
                "build-request",
                "--db", self.db,
                "--state", state_path,
                "--subject-kind", "session",
                "--session", "city-a|host-a|codex|session-1",
            ]
        )
        self.assertEqual(built.returncode, 0, built.stderr)
        request_body = json.loads(built.stdout)
        self.assertEqual(request_body["model"], "jev-1.13.0")
        summary = json.loads(built.stderr.strip().splitlines()[-1])
        request_hash = summary["request_hash"]

        response = support.valid_response(request_body)
        response_path = os.path.join(self.tmp.name, "response.json")
        with open(response_path, "w", encoding="utf-8") as handle:
            json.dump(response, handle)

        imported = run_cli(
            ["import-response", "--db", self.db, "--response", response_path, "--request-hash", request_hash]
        )
        self.assertEqual(imported.returncode, 0, imported.stderr)
        imported_payload = json.loads(imported.stdout)
        self.assertFalse(imported_payload["deduplicated"])

        replayed = run_cli(
            ["import-response", "--db", self.db, "--response", response_path, "--request-hash", request_hash]
        )
        self.assertEqual(replayed.returncode, 0, replayed.stderr)
        self.assertTrue(json.loads(replayed.stdout)["deduplicated"])

    def test_build_request_accepts_collision_free_json_session_identity(self):
        fixture = support.write_jsonl(
            os.path.join(self.tmp.name, "pipes.jsonl"),
            [support.make_record(event_id="e1", city_id="c|h", host_id="x")],
        )
        db = os.path.join(self.tmp.name, "pipes.db")
        self.assertEqual(run_cli(["import-jsonl", "--db", db, fixture]).returncode, 0)
        state_path = os.path.join(self.tmp.name, "state.json")
        with open(state_path, "w", encoding="utf-8") as handle:
            json.dump({"summary": "pipe identity"}, handle)
        built = run_cli(
            [
                "build-request",
                "--db", db,
                "--state", state_path,
                "--subject-kind", "session",
                "--session", '["c|h","x","codex","session-1"]',
            ]
        )
        self.assertEqual(built.returncode, 0, built.stderr)
        self.assertEqual(json.loads(built.stdout)["model"], "jev-1.13.0")

    def test_negative_seed_price_is_reported_as_cli_error_with_exit_one(self):
        seed_path = os.path.join(self.tmp.name, "negative-prices.json")
        with open(seed_path, "w", encoding="utf-8") as handle:
            json.dump(
                {
                    "schema_version": "1.0",
                    "models": [
                        {
                            "model_id": "negative-model",
                            "provider": "provider-a",
                            "input_usd_per_million": -0.01,
                            "output_usd_per_million": 0,
                            "cache_read_usd_per_million": None,
                            "cache_write_usd_per_million": None,
                            "effective_from": "2026-09-21T10:00:00Z",
                            "source": "https://example.com/pricing",
                        }
                    ],
                },
                handle,
            )

        result = run_cli(
            ["seed-pricing", "--db", self.db, "--input", seed_path]
        )
        self.assertEqual(result.returncode, 1)
        self.assertTrue(result.stderr.startswith("error:"), result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        self.assertIn("input_usd_per_million", result.stderr)
        self.assertIn("negative prices are invalid", result.stderr)

    def test_conflicting_price_seed_is_reported_as_cli_error_with_exit_one(self):
        seed_path = os.path.join(self.tmp.name, "conflicting-prices.json")
        with open(seed_path, "w", encoding="utf-8") as handle:
            json.dump(
                {
                    "schema_version": "1.0",
                    "models": [
                        {
                            "model_id": "gpt-4.1",
                            "provider": "OpenAI",
                            "input_usd_per_million": 3,
                            "output_usd_per_million": 8,
                            "cache_read_usd_per_million": 0.5,
                            "cache_write_usd_per_million": None,
                            "effective_from": "2025-04-14T00:00:00Z",
                            "source": "https://openai.com/api/pricing/",
                        }
                    ],
                },
                handle,
            )

        result = run_cli(
            ["seed-pricing", "--db", self.db, "--input", seed_path]
        )
        self.assertEqual(result.returncode, 1)
        self.assertTrue(result.stderr.startswith("error:"), result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        self.assertIn("conflicting model pricing", result.stderr)

    def test_malformed_file_fails_with_line_context(self):
        bad = os.path.join(self.tmp.name, "bad.jsonl")
        with open(bad, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(support.make_record(event_id="e1")) + "\n")
            handle.write("{not json}\n")
        result = run_cli(["import-jsonl", "--db", self.db, bad])
        self.assertEqual(result.returncode, 1)
        self.assertIn("bad.jsonl:2", result.stderr)

    def test_missing_state_file_reports_error_without_traceback(self):
        missing = os.path.join(self.tmp.name, "does-not-exist-state.json")
        result = run_cli(["build-request", "--state", missing])
        self.assertEqual(result.returncode, 1)
        self.assertNotIn("Traceback", result.stderr)
        self.assertTrue(result.stderr.strip().startswith("error:"), result.stderr)
        self.assertIn(missing, result.stderr)

    def test_missing_response_file_reports_error_without_traceback(self):
        missing = os.path.join(self.tmp.name, "does-not-exist-response.json")
        result = run_cli(
            ["import-response", "--db", self.db, "--response", missing, "--request-hash", "0" * 64]
        )
        self.assertEqual(result.returncode, 1)
        self.assertNotIn("Traceback", result.stderr)
        self.assertIn(missing, result.stderr)


SILVER_CANDIDATES = os.path.join(PACKAGE_ROOT, "tests", "fixtures", "silver", "candidates.csv")
SILVER_ANSWERS = os.path.join(PACKAGE_ROOT, "tests", "fixtures", "silver", "recorded_judge_answers.json")


class SilverCliTrustTests(unittest.TestCase):
    """silver-build/evaluate fail closed on a below-floor judge kappa."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db = os.path.join(self.tmp.name, "silver.db")
        records = []
        for index, (session_id, provider) in enumerate(
            [
                ("s-1", "codex"),
                ("s-2", "codex"),
                ("s-3", "codex"),
                ("s-4", "claude"),
                ("s-5", "claude"),
                ("s-6", "codex"),
            ],
            start=1,
        ):
            records.append(
                support.make_record(
                    event_id=f"e-{index}",
                    session_id=session_id,
                    provider=provider,
                    kind="message",
                    text=f"Please implement change {session_id}",
                )
            )
        fixture = support.write_jsonl(os.path.join(self.tmp.name, "silver.jsonl"), records)
        imported = run_cli(["import-jsonl", "--db", self.db, fixture])
        self.assertEqual(imported.returncode, 0, imported.stderr)
        # Replay recorded judge answers so the build never touches the network;
        # copy the fixture because the checkpoint client can rewrite its file.
        self.checkpoint = os.path.join(self.tmp.name, "checkpoint.json")
        shutil.copyfile(SILVER_ANSWERS, self.checkpoint)

    def _build(self, *extra):
        gold = os.path.join(self.tmp.name, "gold.json")
        report = os.path.join(self.tmp.name, "report.json")
        result = run_cli(
            [
                "silver-build",
                "--candidates", SILVER_CANDIDATES,
                "--db", self.db,
                "--sample-size", "2",
                "--seed", "silver-v1",
                "--checkpoint", self.checkpoint,
                "--out-gold", gold,
                "--out-report", report,
                *extra,
            ]
        )
        return result, gold, report

    def test_untrusted_build_refuses_out_gold_and_exits_nonzero(self):
        result, gold, report = self._build()
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("not trustworthy", result.stderr)
        self.assertIn("allow-untrusted", result.stderr)
        self.assertFalse(os.path.exists(gold))
        # The report is still written so the refusal is durable evidence.
        self.assertTrue(os.path.exists(report))
        with open(report, encoding="utf-8") as handle:
            payload = json.load(handle)
        self.assertFalse(payload["agreement"]["trustworthy"])
        self.assertLess(payload["agreement"]["cohen_kappa"], payload["agreement"]["kappa_trust_floor"])

    def test_allow_untrusted_writes_the_marked_gold_set(self):
        result, gold, _ = self._build("--allow-untrusted")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(os.path.exists(gold))
        with open(gold, encoding="utf-8") as handle:
            document = json.load(handle)
        self.assertTrue(document["silver"])
        self.assertFalse(document["silver_trustworthy"])

    def test_evaluate_refuses_an_untrusted_gold_set_unless_opted_in(self):
        built, gold, _ = self._build("--allow-untrusted")
        self.assertEqual(built.returncode, 0, built.stderr)
        predictions = os.path.join(self.tmp.name, "predictions.json")
        with open(predictions, "w", encoding="utf-8") as handle:
            json.dump(
                {
                    "predictor": "jev",
                    "model": "jev-1.13.0",
                    "predictions": [
                        {"episode_id": "ep-plan-4", "primary_intent": "planning_spec"}
                    ],
                },
                handle,
            )
        refused = run_cli(
            [
                "silver-evaluate",
                "--gold", gold,
                "--predictions", predictions,
                "--out", os.path.join(self.tmp.name, "evaluation.json"),
            ]
        )
        self.assertEqual(refused.returncode, 1, refused.stderr)
        self.assertIn("allow-untrusted", refused.stderr)
        allowed = run_cli(
            [
                "silver-evaluate",
                "--gold", gold,
                "--predictions", predictions,
                "--allow-untrusted",
                "--out", os.path.join(self.tmp.name, "evaluation-allowed.json"),
            ]
        )
        self.assertEqual(allowed.returncode, 0, allowed.stderr)
        with open(os.path.join(self.tmp.name, "evaluation-allowed.json"), encoding="utf-8") as handle:
            evaluation = json.load(handle)
        self.assertFalse(evaluation["agreement"]["trustworthy"])


if __name__ == "__main__":
    unittest.main()