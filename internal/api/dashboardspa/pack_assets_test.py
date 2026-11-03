"""Pure producer checks: exact bytes, deterministic metadata and input ownership."""
import base64
import gzip
import json
from pathlib import Path
import tempfile
import unittest

from pack_assets import pack


class AssetPacking(unittest.TestCase):
    def test_roundtrip_is_sorted_exact_and_deterministic(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "assets").mkdir()
            expected = {"dist/index.html": b"<script>inline</script>\n", "dist/assets/app.js": bytes(range(256))}
            (root / "index.html").write_bytes(expected["dist/index.html"])
            (root / "assets/app.js").write_bytes(expected["dist/assets/app.js"])
            first = pack(root)
            (root / "index.html").touch()
            self.assertEqual(first, pack(root))
            self.assertEqual(first[4:8], b"\0\0\0\0")
            self.assertEqual(first[9], 255)
            document = json.loads(gzip.decompress(first))
            self.assertEqual(document["version"], 1)
            self.assertEqual([row["name"] for row in document["files"]], sorted(expected))
            self.assertEqual({row["name"]: base64.b64decode(row["data"]) for row in document["files"]}, expected)

    def test_empty_and_symlink_inputs_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaises(ValueError):
                pack(root)
            (root / "escape").symlink_to(root.parent)
            with self.assertRaises(ValueError):
                pack(root)


if __name__ == "__main__":
    unittest.main()
