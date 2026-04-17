import tempfile
import unittest
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from lib import manifest  # noqa: E402


class ManifestRoundTripTest(unittest.TestCase):
    def test_legacy_url_and_path_read_as_source_and_write_canonical_source(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            city_toml = Path(tmp) / "city.toml"
            city_toml.write_text(
                """
[imports.gastown]
url = "https://github.com/example/gastown"
version = "^1.2"

[imports.helper]
path = "../helper"
""".lstrip()
            )

            m = manifest.read(city_toml)

            self.assertEqual(m.imports["gastown"].source, "https://github.com/example/gastown")
            self.assertEqual(m.imports["gastown"].url, "https://github.com/example/gastown")
            self.assertIsNone(m.imports["gastown"].path)
            self.assertEqual(m.imports["gastown"].version, "^1.2")

            self.assertEqual(m.imports["helper"].source, "../helper")
            self.assertIsNone(m.imports["helper"].url)
            self.assertEqual(m.imports["helper"].path, "../helper")

            manifest.write(m, city_toml)
            written = city_toml.read_text()

            self.assertIn('source = "https://github.com/example/gastown"', written)
            self.assertIn('source = "../helper"', written)
            self.assertNotIn('url = "', written)
            self.assertNotIn('path = "', written)


if __name__ == "__main__":
    unittest.main()
