"""Tests for the bounded Bazel target resolver and BEP correlation helper."""

import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

from bazel_target_resolver import (
    CONFIG_LABELS,
    parse_bep_jsonl,
    resolve_name_status_z,
)


class ResolveNameStatusZTest(unittest.TestCase):
    def resolve(self, *records):
        return resolve_name_status_z(b"\0".join(part.encode() for part in records) + b"\0")

    def test_maps_each_hermetic_source_and_test(self):
        selection = self.resolve(
            "M", "internal/config/envname.go",
            "M", "internal/config/diagnostic_locations_test.go",
            "M", "internal/config/storage_endpoint.go",
        )
        self.assertEqual(selection.labels, CONFIG_LABELS)
        self.assertFalse(selection.conservative)
        self.assertEqual(selection.reason, "mapped")
        self.assertIsNone(selection.error)

    def test_rename_and_delete_of_config_path_fail_closed(self):
        renamed = self.resolve("R100", "internal/config/envname.go", "docs/envname.go")
        deleted = self.resolve("D", "internal/config/storage_binding_validation.go")
        for selection in (renamed, deleted):
            self.assertEqual(selection.labels, CONFIG_LABELS)
            self.assertTrue(selection.conservative)
            self.assertEqual(selection.reason, "config-unmapped")

    def test_spaces_tabs_and_tab_delimited_record_are_preserved(self):
        selection = self.resolve("M\tinternal/config/a file\tname.go")
        self.assertEqual(selection.labels, CONFIG_LABELS)
        self.assertTrue(selection.conservative)
        self.assertEqual(selection.reason, "config-unmapped")

    def test_quoted_and_newline_tab_records(self):
        quoted = resolve_name_status_z(b'M\t"internal/config/envname.go"\n')
        renamed = resolve_name_status_z(b"R100\told name\tinternal/config/envname.go\n")
        copied = resolve_name_status_z(b"C100\tinternal/config/envname.go\tinternal/config/copy.go\n")
        self.assertEqual(quoted.labels, ("//internal/config:config_envname_test",))
        self.assertTrue(renamed.conservative)
        self.assertTrue(copied.conservative)

    def test_shared_graph_files_select_all(self):
        for path in ("MODULE.bazel.lock", ".bazelversion", "go.mod", "internal/clock/BUILD.bazel"):
            selection = self.resolve("M", path)
            self.assertEqual(selection.labels, CONFIG_LABELS)
            self.assertTrue(selection.conservative)
            self.assertEqual(selection.reason, "shared-build-graph")

    def test_unrelated_path_selects_zero(self):
        selection = self.resolve("M", "docs/guide.md")
        self.assertEqual(selection.labels, ())
        self.assertFalse(selection.conservative)
        self.assertEqual(selection.reason, "unrelated")

    def test_empty_and_malformed_input_fail_closed(self):
        for raw in (b"", b"M\0", b"R100\0old-only\0"):
            selection = resolve_name_status_z(raw)
            self.assertEqual(selection.labels, CONFIG_LABELS)
            self.assertTrue(selection.conservative)
            self.assertEqual(selection.reason, "unavailable")
            self.assertIsNotNone(selection.error)


class ParseBEPTest(unittest.TestCase):
    requested = CONFIG_LABELS

    def parse(self, events, requested=None):
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", delete=False) as f:
            for event in events:
                f.write(json.dumps(event) + "\n")
            path = f.name
        self.addCleanup(pathlib.Path(path).unlink, missing_ok=True)
        return parse_bep_jsonl(path, self.requested if requested is None else requested)

    def test_dedupes_id_and_payload_but_rejects_duplicate_events(self):
        result = self.parse([
            {"id": {"targetConfigured": {"label": "//internal/config:config_storage_endpoint_test"}}, "configured": {"targetConfigured": {"label": "//internal/config:config_storage_endpoint_test"}}},
            {"id": {"targetConfigured": {"label": "//internal/config:config_envname_test"}}, "configured": {}},
            {"id": {"targetCompleted": {"label": "//internal/config:config_envname_test"}}, "completed": {"targetCompleted": {"label": "//internal/config:config_envname_test"}, "success": True}},
            {"id": {"targetCompleted": {"label": "//internal/config:config_envname_test"}}, "completed": {"success": True}},
            {"id": {"targetCompleted": {"label": "//internal/config:config_storage_endpoint_test"}}, "completed": {"success": True}},
        ], ("//internal/config:config_envname_test", "//internal/config:config_storage_endpoint_test"))
        self.assertIsNotNone(result.error)
        self.assertEqual(result.error, "duplicate requested target events")
        self.assertIsNone(result.completed_count)

    def test_requested_pattern_zero_one_three_and_mismatch(self):
        zero = self.parse([{"id": {"pattern": {"patterns": []}}, "pattern": {"patterns": []}}])
        self.assertIsNotNone(zero.error)
        one = self.parse([
            {"id": {"pattern": {"pattern": "//internal/config:config_envname_test"}}, "pattern": {"pattern": "//internal/config:config_envname_test"}},
            {"id": {"targetConfigured": {"label": "//internal/config:config_envname_test"}}, "configured": {}},
            {"id": {"targetCompleted": {"label": "//internal/config:config_envname_test"}}, "completed": {}},
        ], ("//internal/config:config_envname_test",))
        self.assertEqual(one.configured_count, 1)
        three_events = [{"id": {"pattern": {"pattern": list(self.requested)}}, "pattern": {"patterns": list(self.requested)}}]
        for label in self.requested:
            three_events.extend([
                {"id": {"targetConfigured": {"label": label}}, "configured": {}},
                {"id": {"targetCompleted": {"label": label}}, "completed": {}},
            ])
        three = self.parse(three_events)
        self.assertEqual(three.configured_count, 3)
        mismatch = self.parse([
            {"id": {"pattern": {"pattern": "//other:target"}}, "pattern": {}},
        ])
        self.assertIsNotNone(mismatch.error)

    def test_nested_fallback_labels_are_accepted(self):
        result = self.parse([
            {"id": {"targetConfigured": {}}, "configured": {"targetConfigured": {"label": "//internal/config:config_diagnostic_locations_test"}}},
            {"id": {"targetCompleted": {}}, "completed": {"targetCompleted": {"label": "//internal/config:config_diagnostic_locations_test"}}},
        ], ("//internal/config:config_diagnostic_locations_test",))
        self.assertEqual(result.configured_count, 1)
        self.assertEqual(result.completed_count, 1)
        self.assertIsNone(result.error)

    def test_missing_mismatched_and_truncated_events_are_unknown(self):
        for events in (
            [],
            [{"id": {"targetConfigured": {"label": "//other:nope"}}, "configured": {}}],
            [{"id": {"targetConfigured": {"label": "//internal/config:config_envname_test"}}, "configured": {}}],
        ):
            result = self.parse(events)
            self.assertIsNotNone(result.error)
            self.assertIsNone(result.configured_count)
            self.assertIsNone(result.completed_count)

    def test_real_bazel_9_2_fixture(self):
        fixture = pathlib.Path(__file__).with_name("testdata") / "bazel" / "real_bazel_9_2.bep.jsonl"
        result = parse_bep_jsonl(fixture, ("//internal/config:config_envname_test",))
        self.assertEqual(result.configured_count, 1)
        self.assertEqual(result.completed_count, 1)
        self.assertEqual(result.configured, ("//internal/config:config_envname_test",))
        self.assertEqual(result.completed, result.configured)
        self.assertIsNone(result.error)

    def test_fixture_contains_no_credential_material(self):
        fixture = pathlib.Path(__file__).with_name("testdata") / "bazel" / "real_bazel_9_2.bep.jsonl"
        text = fixture.read_text(encoding="utf-8")
        self.assertNotRegex(text, r"(?i)(api[_-]?key|auth[_-]?token|password|secret|mn_live_|sk-[A-Za-z0-9])")


class CLIResilienceTest(unittest.TestCase):
    def test_unreadable_resolve_input_is_structured_and_nonzero(self):
        script = pathlib.Path(__file__).with_name("bazel_target_resolver.py")
        missing = pathlib.Path(tempfile.gettempdir()) / "gascity-resolver-no-such-input"
        for output_format in ("json", "tsv"):
            result = subprocess.run(
                [sys.executable, str(script), "resolve", str(missing), "--format", output_format],
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stderr, "")
            if output_format == "json":
                payload = json.loads(result.stdout)
                self.assertEqual(tuple(payload["labels"]), CONFIG_LABELS)
                self.assertTrue(payload["conservative"])
                self.assertEqual(payload["reason"], "unavailable")
                self.assertTrue(payload["error"])
            else:
                labels, conservative, reason, error = result.stdout.rstrip("\n").split("\t", 3)
                self.assertEqual(tuple(labels.split(",")), CONFIG_LABELS)
                self.assertEqual(conservative, "true")
                self.assertEqual(reason, "unavailable")
                self.assertTrue(error)


if __name__ == "__main__":
    unittest.main()
