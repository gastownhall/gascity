"""Check canonical uncompressed manifests across pinned build/local compressors."""
import argparse
import gzip
from pathlib import Path
import unittest


class PackedDistSync(unittest.TestCase):
    def test_committed_and_generated_payloads_match(self):
        documents = []
        for directory in (ARGS.committed, ARGS.generated):
            data = (directory / "dist.bundle.json.gz").read_bytes()
            self.assertEqual(data[4:8], b"\0\0\0\0", "gzip timestamp is not deterministic")
            self.assertEqual(data[9], 255, "gzip OS byte is not platform-neutral")
            documents.append(gzip.decompress(data))
        self.assertEqual(documents[0], documents[1], "packed dashboard is stale; run make dashboard-build")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--committed", type=Path, required=True)
    parser.add_argument("--generated", type=Path, required=True)
    ARGS, rest = parser.parse_known_args()
    unittest.main(argv=[__file__] + rest)
