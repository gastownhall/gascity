import unittest

import critical_path_evidence as cpe


def _needs(job, result):
    return {job: {"result": result}}


class CriticalPathJobsTests(unittest.TestCase):
    def test_maps_every_critical_path_category_to_its_gate_job(self) -> None:
        self.assertEqual(
            cpe.CRITICAL_PATH_JOBS,
            {
                "cmd_gc_process": "cmd-gc-process",
                "integration": "integration-shards",
                "worker": "worker-core-summary",
                "worker_phase2": "worker-core-phase2-summary",
                "packs": "pack-gate",
                "docker": "docker-session",
                "k8s": "k8s-session",
                "openclaw_bridge": "openclaw-bridge",
            },
        )


class EvaluateTests(unittest.TestCase):
    def test_matched_success_is_ok(self) -> None:
        rows = cpe.evaluate({"cmd_gc_process": "true"}, _needs("cmd-gc-process", "success"))
        row = next(r for r in rows if r["category"] == "cmd_gc_process")
        self.assertTrue(row["ok"])
        self.assertEqual(cpe.failures(rows), [])

    def test_matched_skipped_fails(self) -> None:
        rows = cpe.evaluate({"cmd_gc_process": "true"}, _needs("cmd-gc-process", "skipped"))
        row = next(r for r in rows if r["category"] == "cmd_gc_process")
        self.assertFalse(row["ok"])
        self.assertIn("cmd_gc_process", cpe.failures(rows)[0])

    def test_matched_absent_fails(self) -> None:
        rows = cpe.evaluate({"cmd_gc_process": "true"}, {})
        row = next(r for r in rows if r["category"] == "cmd_gc_process")
        self.assertFalse(row["ok"])
        self.assertEqual(row["result"], "absent")

    def test_matched_failure_fails(self) -> None:
        rows = cpe.evaluate({"cmd_gc_process": "true"}, _needs("cmd-gc-process", "failure"))
        self.assertFalse(next(r for r in rows if r["category"] == "cmd_gc_process")["ok"])

    def test_matched_cancelled_fails(self) -> None:
        rows = cpe.evaluate({"cmd_gc_process": "true"}, _needs("cmd-gc-process", "cancelled"))
        self.assertFalse(next(r for r in rows if r["category"] == "cmd_gc_process")["ok"])

    def test_unmatched_skipped_is_ok(self) -> None:
        rows = cpe.evaluate({"cmd_gc_process": "false"}, _needs("cmd-gc-process", "skipped"))
        self.assertTrue(next(r for r in rows if r["category"] == "cmd_gc_process")["ok"])

    def test_unmatched_success_is_ok(self) -> None:
        # A job that ran successfully despite an unmatched filter (e.g. a
        # reusable/manual full-suite run) must not be penalized.
        rows = cpe.evaluate({"cmd_gc_process": "false"}, _needs("cmd-gc-process", "success"))
        self.assertTrue(next(r for r in rows if r["category"] == "cmd_gc_process")["ok"])

    def test_unmatched_absent_is_ok(self) -> None:
        rows = cpe.evaluate({"cmd_gc_process": "false"}, {})
        self.assertTrue(next(r for r in rows if r["category"] == "cmd_gc_process")["ok"])

    def test_every_mapped_category_is_evaluated_independently(self) -> None:
        changes = {category: "true" for category in cpe.CRITICAL_PATH_JOBS}
        rows = cpe.evaluate(changes, {})
        self.assertEqual(len(rows), len(cpe.CRITICAL_PATH_JOBS))
        self.assertEqual(len(cpe.failures(rows)), len(cpe.CRITICAL_PATH_JOBS))
        self.assertEqual({row["category"] for row in rows}, set(cpe.CRITICAL_PATH_JOBS))

    def test_missing_changes_key_defaults_to_unmatched(self) -> None:
        # A category is only absent from `changes` if the workflow wiring
        # forgot to pass it through -- treat that as unmatched (fail open on
        # the input) so a wiring gap shows up as a Go policy-wiring failure,
        # not a crash here.
        rows = cpe.evaluate({}, {})
        self.assertEqual(cpe.failures(rows), [])

    def test_rows_are_sorted_by_category(self) -> None:
        rows = cpe.evaluate({}, {})
        self.assertEqual([r["category"] for r in rows], sorted(cpe.CRITICAL_PATH_JOBS))


class FailuresMessageTests(unittest.TestCase):
    def test_failure_message_names_job_and_result(self) -> None:
        rows = cpe.evaluate({"k8s": "true"}, _needs("k8s-session", "cancelled"))
        message = cpe.failures(rows)[0]
        self.assertIn("k8s-session", message)
        self.assertIn("cancelled", message)

    def test_multiple_failures_are_all_reported(self) -> None:
        changes = {"k8s": "true", "docker": "true", "packs": "false"}
        needs = {
            "k8s-session": {"result": "cancelled"},
            "docker-session": {"result": "skipped"},
            "pack-gate": {"result": "skipped"},
        }
        rows = cpe.evaluate(changes, needs)
        messages = cpe.failures(rows)
        self.assertEqual(len(messages), 2)
        joined = " ".join(messages)
        self.assertIn("k8s-session", joined)
        self.assertIn("docker-session", joined)
        self.assertNotIn("pack-gate", joined)


if __name__ == "__main__":
    unittest.main()
