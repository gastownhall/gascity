"""Tests for checked-in token prices and the event usage cost view."""

from __future__ import annotations

import os
import tempfile
import unittest
from decimal import Decimal

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory.errors import ObservatoryError, PricingError
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
                provider="OpenAI",
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
                provider="OpenAI",
                model="gpt-4.1",
                usage={"input_tokens": 0, "output_tokens": 0, "cache_write_tokens": 1},
            ),
            support.make_record(
                event_id="zero-cache-write",
                provider="OpenAI",
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
        self.assertEqual(rows["priced"]["cost_usd"], "19.5")
        self.assertEqual(rows["priced"]["model_provider"], "OpenAI")
        self.assertTrue(rows["priced"]["pricing_source"].startswith("https://"))
        self.assertFalse(rows["unpriced"]["cost_known"])
        self.assertIsNone(rows["unpriced"]["cost_usd"])
        self.assertFalse(rows["cache-write-unknown"]["cost_known"])
        self.assertIsNone(rows["cache-write-unknown"]["cost_usd"])
        self.assertTrue(rows["zero-cache-write"]["cost_known"])
        self.assertEqual(rows["zero-cache-write"]["cost_usd"], "0")

    def test_cost_prices_are_exact_and_provider_scoped(self):
        exact_price = Decimal("0.123456789012345678901234567890123456789")
        records = [
            support.make_record(
                event_id="provider-a",
                provider="provider-a",
                model="shared-model",
                usage={"input_tokens": 1_000_000},
            ),
            support.make_record(
                event_id="provider-b",
                provider="provider-b",
                model="shared-model",
                usage={"input_tokens": 1_000_000},
            ),
            support.make_record(
                event_id="provider-c",
                provider="provider-c",
                model="shared-model",
                usage={"input_tokens": 1_000_000},
            ),
        ]
        path = support.write_jsonl(os.path.join(self.tmp.name, "provider-usage.jsonl"), records)
        effective_from = "2026-09-21T10:00:00Z"
        source = "https://example.com/pricing"
        prices = [
            {
                "model_id": "shared-model",
                "provider": "provider-a",
                "input_usd_per_million": exact_price,
                "output_usd_per_million": Decimal("0"),
                "cache_read_usd_per_million": None,
                "cache_write_usd_per_million": None,
                "effective_from": effective_from,
                "source": source,
            },
            {
                "model_id": "shared-model",
                "provider": "provider-b",
                "input_usd_per_million": Decimal("2.5"),
                "output_usd_per_million": Decimal("0"),
                "cache_read_usd_per_million": None,
                "cache_write_usd_per_million": None,
                "effective_from": effective_from,
                "source": source,
            },
            {
                "model_id": "shared-model",
                "provider": None,
                "input_usd_per_million": Decimal("99"),
                "output_usd_per_million": Decimal("0"),
                "cache_read_usd_per_million": None,
                "cache_write_usd_per_million": None,
                "effective_from": effective_from,
                "source": source,
            },
        ]
        with ObservatoryStore(self.db_path) as store:
            store.import_jsonl(path)
            self.assertEqual(store.save_model_pricing(prices), 3)
            conflicting_default = dict(
                prices[2], input_usd_per_million=Decimal("100")
            )
            with self.assertRaises(ObservatoryError):
                store.save_model_pricing([conflicting_default])
            rows = {
                row["event_provider"]: dict(row)
                for row in store.conn.execute("SELECT * FROM event_usage_cost")
            }
            stored_type = store.conn.execute(
                "SELECT typeof(input_usd_per_million) FROM model_pricing "
                "WHERE model_id = 'shared-model' AND provider = 'provider-a'"
            ).fetchone()[0]

        self.assertEqual(stored_type, "text")
        self.assertEqual(rows["provider-a"]["cost_usd"], str(exact_price))
        self.assertEqual(rows["provider-a"]["model_provider"], "provider-a")
        self.assertEqual(rows["provider-b"]["cost_usd"], "2.5")
        self.assertEqual(rows["provider-b"]["model_provider"], "provider-b")
        self.assertFalse(rows["provider-c"]["cost_known"])
        self.assertIsNone(rows["provider-c"]["cost_usd"])

    def test_conflicts_raise_observatory_error_and_negative_prices_are_rejected(self):
        row = {
            "model_id": "conflict-model",
            "provider": "provider-a",
            "input_usd_per_million": Decimal("1.25"),
            "output_usd_per_million": Decimal("0"),
            "cache_read_usd_per_million": None,
            "cache_write_usd_per_million": None,
            "effective_from": "2026-09-21T10:00:00Z",
            "source": "https://example.com/pricing",
        }
        with ObservatoryStore(self.db_path) as store:
            self.assertEqual(store.save_model_pricing([row]), 1)
            conflicting = dict(row, input_usd_per_million=Decimal("1.5"))
            with self.assertRaises(ObservatoryError) as caught:
                store.save_model_pricing([conflicting])
            self.assertIn("conflicting model pricing", str(caught.exception))

            negative = dict(row, model_id="negative-model", input_usd_per_million=Decimal("-0.01"))
            with self.assertRaises(PricingError) as caught:
                store.save_model_pricing([negative])
            self.assertIn("input_usd_per_million", str(caught.exception))
            self.assertIn("negative prices are invalid", str(caught.exception))
            self.assertEqual(
                store.conn.execute(
                    "SELECT COUNT(*) FROM model_pricing WHERE model_id = 'negative-model'"
                ).fetchone()[0],
                0,
            )


if __name__ == "__main__":
    unittest.main()
