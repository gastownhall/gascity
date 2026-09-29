"""Tests for checked-in token prices and the event usage cost view."""

from __future__ import annotations

import os
import tempfile
import unittest

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory.pricing import load_pricing_seed, seed_model_pricing
from agent_observatory.store import ObservatoryStore


class PricingTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db_path = os.path.join(self.tmp.name, "projection.db")

    def test_checked_in_seed_is_cited_and_idempotent(self):
        prices = load_pricing_seed()
        self.assertGreaterEqual(len(prices), 3)
        self.assertTrue(all(row["source"].startswith("https://") for row in prices))
        with ObservatoryStore(self.db_path) as store:
            self.assertEqual(
                store.conn.execute("SELECT COUNT(*) FROM model_pricing").fetchone()[0],
                len(prices),
            )
            self.assertEqual(seed_model_pricing(store), 0)
            self.assertEqual(seed_model_pricing(store), 0)

    def test_cost_view_applies_effective_public_price_and_preserves_unknowns(self):
        records = [
            support.make_record(
                event_id="priced",
                model="gpt-4.1",
                usage={
                    "input_tokens": 1_000_000,
                    "output_tokens": 2_000_000,
                    "cache_read_tokens": 3_000_000,
                },
            ),
            support.make_record(
                event_id="unpriced",
                model="private-alias",
                usage={"input_tokens": 100, "output_tokens": 50},
            ),
            support.make_record(
                event_id="cache-write-unknown",
                model="gpt-4.1",
                usage={"input_tokens": 0, "output_tokens": 0, "cache_write_tokens": 1},
            ),
            support.make_record(
                event_id="zero-cache-write",
                model="gpt-4.1",
                usage={"input_tokens": 0, "output_tokens": 0, "cache_write_tokens": 0},
            ),
        ]
        path = support.write_jsonl(os.path.join(self.tmp.name, "usage.jsonl"), records)
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            seed_model_pricing(store)
            rows = {
                row["event_id"]: dict(row)
                for row in store.conn.execute("SELECT * FROM event_usage_cost")
            }
        self.assertTrue(rows["priced"]["cost_known"])
        self.assertAlmostEqual(rows["priced"]["cost_usd"], 19.5)
        self.assertEqual(rows["priced"]["model_provider"], "OpenAI")
        self.assertTrue(rows["priced"]["pricing_source"].startswith("https://"))
        self.assertFalse(rows["unpriced"]["cost_known"])
        self.assertIsNone(rows["unpriced"]["cost_usd"])
        self.assertFalse(rows["cache-write-unknown"]["cost_known"])
        self.assertIsNone(rows["cache-write-unknown"]["cost_usd"])
        self.assertTrue(rows["zero-cache-write"]["cost_known"])
        self.assertEqual(rows["zero-cache-write"]["cost_usd"], 0.0)


if __name__ == "__main__":
    unittest.main()
