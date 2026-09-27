"""Regression tests for the managed Bazel source filegroups."""

import tempfile
import unittest
from pathlib import Path

from repo_tree import BLOCK_MARKER, refresh_pkg_block


class RefreshPkgBlockTest(unittest.TestCase):
    def test_gazelle_formatted_block_is_restored_idempotently(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "BUILD.bazel"
            path.write_text(
                'filegroup(name = "other", srcs = [])\n\n'
                f'{BLOCK_MARKER}\n\n'
                'filegroup(\n'
                '    name = "bazel_repo_srcs",\n'
                '    srcs = glob(\n'
                '        ["**"],\n'
                '        allow_empty = True,\n'
                '        exclude = ["BUILD.bazel"],\n'
                '    ),\n'
                '    visibility = ["//visibility:public"],\n'
                ')\n'
            )

            refresh_pkg_block(str(path))
            first = path.read_text()
            self.assertEqual(first.count(BLOCK_MARKER), 1)
            self.assertIn('srcs = glob(\n        ["**"],', first)

            refresh_pkg_block(str(path))
            self.assertEqual(path.read_text(), first)


if __name__ == "__main__":
    unittest.main()
