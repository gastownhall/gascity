"""Tests for the bounded Bazel target resolver and BEP correlation helper."""

import json
import pathlib
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

    def parse(self, events):
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", delete=False) as f:
            for event in events:
                f.write(json.dumps(event) + "\n")
            path = f.name
        self.addCleanup(pathlib.Path(path).unlink, missing_ok=True)
        return parse_bep_jsonl(path, self.requested)

    def test_dedupes_sorts_and_intersects_configured_and_completed(self):
        result = self.parse([
            {"id": {"targetConfigured": {"label": "//internal/config:config_storage_endpoint_test"}}, "configured": {"targetConfigured": {"label": "//internal/config:config_storage_endpoint_test"}}},
            {"id": {"targetConfigured": {"label": "//internal/config:config_envname_test"}}, "configured": {}},
            {"id": {"targetConfigured": {"label": "//other:ignored"}}, "configured": {}},
            {"id": {"targetCompleted": {"label": "//internal/config:config_envname_test"}}, "completed": {"success": True}},
            {"id": {"targetCompleted": {"label": "//internal/config:config_envname_test"}}, "completed": {"success": True}},
            {"id": {"targetCompleted": {"label": "//internal/config:config_storage_endpoint_test"}}, "completed": {"success": True}},
        ])
        self.assertEqual(result.configured, (
            "//internal/config:config_envname_test",
            "//internal/config:config_storage_endpoint_test",
        ))
        self.assertEqual(result.completed, result.configured)
        self.assertEqual(result.configured_count, 2)
        self.assertEqual(result.completed_count, 2)
        self.assertIsNone(result.error)

    def test_nested_fallback_labels_are_accepted(self):
        result = self.parse([
            {"id": {"targetConfigured": {}}, "configured": {"targetConfigured": {"label": "//internal/config:config_diagnostic_locations_test"}}},
            {"id": {"targetCompleted": {}}, "completed": {"targetCompleted": {"label": "//internal/config:config_diagnostic_locations_test"}}},
        ])
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
        result = parse_bep_jsonl(fixture, self.requested)
        self.assertEqual(result.configured_count, 1)
        self.assertEqual(result.completed_count, 1)
        self.assertEqual(result.configured, ("//internal/config:config_envname_test",))
        self.assertEqual(result.completed, result.configured)
        self.assertIsNone(result.error)

    def test_fixture_contains_no_credential_material(self):
        fixture = pathlib.Path(__file__).with_name("testdata") / "bazel" / "real_bazel_9_2.bep.jsonl"
        text = fixture.read_text(encoding="utf-8")
        self.assertNotRegex(text, r"(?i)(api[_-]?key|auth[_-]?token|password|secret|mn_live_|sk-[A-Za-z0-9])")


if __name__ == "__main__":
    unittest.main()
