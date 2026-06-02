import unittest
from pathlib import Path

import yaml

import ci_suite_coverage as cov

CI_YML = Path(__file__).resolve().parents[1] / "ci.yml"

# Core substrates whose breakage ripples across every subsystem. Beads is the
# universal persistence substrate, events the universal observation substrate,
# config the universal activation mechanism (see AGENTS.md); build/dependency/CI
# files affect every job. The `shared` filter must cover all of these.
EXPECTED_SHARED_PATHS = {
    "go.mod",
    "go.sum",
    "Makefile",
    ".github/workflows/**",
    "internal/beads/**",
    "internal/events/**",
    "internal/config/**",
}

# Outputs of the `changes` job that gate a downstream job. Each must fold the
# `shared` filter into its value so a cross-cutting change runs the full suite.
GATED_OUTPUTS = {
    "mail",
    "docker",
    "k8s",
    "packs",
    "worker",
    "worker_phase2",
    "cmd_gc_process",
    "integration",
}


def _load_changes_job():
    workflow = yaml.safe_load(CI_YML.read_text(encoding="utf-8"))
    return workflow["jobs"]["changes"]


def _filter_globs():
    """Return {filter_name: [globs]} parsed from the dorny filter block."""
    changes = _load_changes_job()
    for step in changes["steps"]:
        if step.get("id") == "filter":
            return yaml.safe_load(step["with"]["filters"])
    raise AssertionError("filter step not found in changes job")


class ClassifyModeTests(unittest.TestCase):
    def test_shared_match_is_full(self) -> None:
        self.assertEqual(cov.classify_mode(True), cov.FULL)

    def test_no_shared_match_is_filtered(self) -> None:
        self.assertEqual(cov.classify_mode(False), cov.FILTERED)


class PathsMatchTests(unittest.TestCase):
    def test_directory_glob_matches_nested_file(self) -> None:
        self.assertTrue(cov.paths_match(["internal/beads/store.go"], ["internal/beads/**"]))

    def test_directory_glob_does_not_match_sibling(self) -> None:
        self.assertFalse(cov.paths_match(["internal/beadsx/store.go"], ["internal/beads/**"]))

    def test_suffix_glob_matches_any_go_file(self) -> None:
        self.assertTrue(cov.paths_match(["cmd/gc/main.go"], ["**/*.go"]))

    def test_literal_path_matches_exactly(self) -> None:
        self.assertTrue(cov.paths_match(["go.mod"], ["go.mod"]))
        self.assertFalse(cov.paths_match(["go.sum"], ["go.mod"]))

    def test_trailing_wildcard_matches_within_segment(self) -> None:
        # `cmd/gc/session_*` and `contrib/session-scripts/gc-session-k8s*`
        self.assertTrue(cov.paths_match(["cmd/gc/session_pool.go"], ["cmd/gc/session_*"]))
        self.assertTrue(
            cov.paths_match(
                ["contrib/session-scripts/gc-session-k8s-runner"],
                ["contrib/session-scripts/gc-session-k8s*"],
            )
        )

    def test_trailing_wildcard_does_not_cross_slash(self) -> None:
        # `*` must not match a path separator, mirroring picomatch/dorny.
        self.assertFalse(cov.paths_match(["cmd/gc/session_sub/extra.go"], ["cmd/gc/session_*"]))

    def test_mid_path_wildcard_with_suffix(self) -> None:
        # `cmd/gc/template_resolve*.go`
        self.assertTrue(
            cov.paths_match(
                ["cmd/gc/template_resolve_t3bridge.go"],
                ["cmd/gc/template_resolve*.go"],
            )
        )
        self.assertFalse(
            cov.paths_match(
                ["cmd/gc/template_resolve_t3bridge.txt"], ["cmd/gc/template_resolve*.go"]
            )
        )

    def test_embedded_globstar(self) -> None:
        # `test/**worker**` matches any test path containing "worker".
        self.assertTrue(
            cov.paths_match(["test/integration/session_worker_test.go"], ["test/**worker**"])
        )
        self.assertFalse(cov.paths_match(["test/integration/mail_test.go"], ["test/**worker**"]))

    def test_root_file_matches_leading_globstar_suffix(self) -> None:
        # `**/*.go` must match a repo-root file, not only nested ones.
        self.assertTrue(cov.paths_match(["main.go"], ["**/*.go"]))

    def test_simulator_handles_every_glob_shape_in_ci_yml(self) -> None:
        """Regression guard: a representative path for each real ci.yml glob
        must match its glob. Catches a filter glob whose shape the simulator
        silently fails to match (the under-fire failure mode this module
        exists to detect)."""
        # One sample path constructed to match each glob shape present in the
        # ci.yml filters. Globstar/literal shapes are exercised above; this
        # pins the trailing/mid-path single-star shapes against the live globs.
        samples = {
            "cmd/gc/template_resolve*.go": "cmd/gc/template_resolve_t3bridge.go",
            "cmd/gc/session_*": "cmd/gc/session_pool.go",
            "contrib/session-scripts/gc-session-k8s*": (
                "contrib/session-scripts/gc-session-k8s-runner"
            ),
            "test/**worker**": "test/integration/session_worker_test.go",
        }
        all_globs = {glob for globs in _filter_globs().values() for glob in globs}
        for glob, sample in samples.items():
            self.assertIn(glob, all_globs, f"sample glob no longer present in ci.yml: {glob}")
            self.assertTrue(
                cov.paths_match([sample], [glob]),
                f"simulator fails to match {sample!r} against live ci.yml glob {glob!r}",
            )


class AggregateTests(unittest.TestCase):
    def test_percentages(self) -> None:
        result = cov.aggregate([cov.FULL, cov.FILTERED, cov.FILTERED, cov.FULL])
        self.assertEqual(result["total"], 4)
        self.assertEqual(result["full"], 2)
        self.assertEqual(result["filtered"], 2)
        self.assertEqual(result["full_pct"], 50.0)
        self.assertEqual(result["filtered_pct"], 50.0)

    def test_empty_is_zero_not_division_error(self) -> None:
        result = cov.aggregate([])
        self.assertEqual(result["total"], 0)
        self.assertEqual(result["full_pct"], 0.0)

    def test_unknown_tokens_counted_separately(self) -> None:
        result = cov.aggregate([cov.FULL, "weird"])
        self.assertEqual(result["unknown"], 1)


class WiringTests(unittest.TestCase):
    """Assert the option-A union wiring is present and correct in ci.yml."""

    def test_shared_filter_covers_core_substrates(self) -> None:
        filters = _filter_globs()
        self.assertIn("shared", filters, "changes job must define a `shared` filter")
        shared = set(filters["shared"])
        missing = EXPECTED_SHARED_PATHS - shared
        self.assertFalse(missing, f"shared filter missing core paths: {sorted(missing)}")

    def test_gated_outputs_fold_in_shared(self) -> None:
        outputs = _load_changes_job()["outputs"]
        for name in GATED_OUTPUTS:
            self.assertIn(name, outputs, f"missing changes output: {name}")
            expr = outputs[name]
            self.assertIn(
                "shared",
                expr,
                f"output `{name}` must fold in the shared filter so cross-cutting "
                f"changes run the full suite; got: {expr!r}",
            )

    def test_changes_job_exposes_shared_and_suite_mode(self) -> None:
        outputs = _load_changes_job()["outputs"]
        self.assertIn("shared", outputs, "raw `shared` output drives the coverage metric")
        self.assertIn("suite_mode", outputs, "`suite_mode` output records the metric per run")


class AcceptanceScenarioTests(unittest.TestCase):
    """Acceptance scenario: cross-cutting change forces the full suite.

    A PR that only touches cmd/gc/foo.go AND modifies a shared type used by
    integration tests must run the integration job, not skip it.
    """

    def test_cmd_gc_plus_shared_core_change_runs_full_suite(self) -> None:
        filters = _filter_globs()
        changed = ["cmd/gc/foo.go", "internal/beads/widget.go"]

        shared_fires = cov.paths_match(changed, filters["shared"])
        self.assertTrue(shared_fires, "a core-substrate edit must trigger the shared filter")

        mode = cov.classify_mode(shared_fires)
        self.assertEqual(mode, cov.FULL, "cross-cutting change must classify as a full-suite run")

        # The integration output folds shared in, so the integration-shards job
        # gate (`needs.changes.outputs.integration == 'true'`) is satisfied even
        # if the integration filter had not matched on its own.
        integration_expr = _load_changes_job()["outputs"]["integration"]
        self.assertIn("shared", integration_expr)


if __name__ == "__main__":
    unittest.main()
