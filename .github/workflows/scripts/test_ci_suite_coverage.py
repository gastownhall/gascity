import unittest

import ci_suite_coverage as cov


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

    def test_matcher_handles_supported_single_star_shapes(self) -> None:
        samples = {
            "cmd/gc/template_resolve*.go": "cmd/gc/template_resolve_t3bridge.go",
            "cmd/gc/session_*": "cmd/gc/session_pool.go",
            "contrib/session-scripts/gc-session-k8s*": (
                "contrib/session-scripts/gc-session-k8s-runner"
            ),
            "test/**worker**": "test/integration/session_worker_test.go",
        }
        for glob, sample in samples.items():
            self.assertTrue(
                cov.paths_match([sample], [glob]),
                f"matcher fails to match {sample!r} against {glob!r}",
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


if __name__ == "__main__":
    unittest.main()
