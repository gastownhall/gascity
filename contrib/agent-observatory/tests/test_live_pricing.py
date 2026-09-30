"""Live tariffs must preserve unknown models and UTC tariff boundaries."""
import os
import tempfile
import unittest
try:
    from . import support
except ImportError:
    import support
from agent_observatory.store import ObservatoryStore


class LivePricingTest(unittest.TestCase):
    def test_v8_upgrade_and_idempotence(self):
        import sqlite3
        from agent_observatory.migrations import migrate_database, V6_CORE_SCHEMA_STATEMENTS
        with tempfile.TemporaryDirectory() as tmp:
            db = os.path.join(tmp, 'db')
            path = support.write_jsonl(os.path.join(tmp, 'usage.jsonl'), [
                support.make_record(model='gpt-6-astra', usage={'input_tokens': 1000000})])
            with ObservatoryStore(db) as store:
                store.import_jsonl(path)
                store.conn.execute("DELETE FROM model_pricing WHERE effective_from LIKE '2026-09-01%'")
                store.conn.execute('DROP VIEW event_usage_cost')
                store.conn.execute(V6_CORE_SCHEMA_STATEMENTS[-1])
                store.conn.execute("UPDATE schema_meta SET value='8' WHERE key='schema_version'")
                store.conn.execute('PRAGMA user_version=8')
                store.conn.commit()
                self.assertEqual(store.conn.execute('SELECT cost_known FROM event_usage_cost').fetchone()[0], 0)
            result = migrate_database(db)
            self.assertEqual((result.from_version, result.to_version), (8, 9))
            with sqlite3.connect(result.backup_path) as backup:
                self.assertEqual(backup.execute('PRAGMA user_version').fetchone()[0], 8)
            with ObservatoryStore(db) as store:
                self.assertEqual(store.conn.execute('SELECT cost_usd FROM event_usage_cost').fetchone()[0], '10')
            self.assertEqual(migrate_database(db).already_at_version, 9)

    def test_windows_and_unknown(self):
        with tempfile.TemporaryDirectory() as tmp:
            records = []
            cases = [('peak', '2026-09-28T01:00:00Z', '0.3'),
                     ('end', '2026-09-28T04:00:00Z', '0.15'),
                     ('second', '2026-09-28T06:00:00Z', '0.3'),
                     ('second-end', '2026-09-28T10:00:00Z', '0.15'),
                     ('weekend', '2026-09-27T02:00:00Z', '0.15')]
            for event, timestamp, _ in cases:
                records.append(support.make_record(event_id=event, provider='dsh',
                    model='deepseek/deepseek-flash', timestamp=timestamp,
                    usage={'input_tokens': 1000000}))
            records.append(support.make_record(event_id='unknown', provider='dsh',
                model='private-alias', usage={'input_tokens': 1000000}))
            path = support.write_jsonl(os.path.join(tmp, 'usage.jsonl'), records)
            with ObservatoryStore(os.path.join(tmp, 'db')) as store:
                store.import_jsonl(path)
                rows = {r['event_id']: r for r in store.conn.execute('SELECT * FROM event_usage_cost')}
                for event, _, cost in cases:
                    self.assertEqual(rows[event]['cost_known'], 1)
                    self.assertEqual(rows[event]['cost_usd'], cost)
                self.assertEqual(rows['unknown']['cost_known'], 0)
                self.assertIsNone(rows['unknown']['cost_usd'])
                for model in ['gpt-6-astra', 'gpt-6.1-sol', 'gpt-6-luna', 'gpt-5.6-sol']:
                    self.assertIsNotNone(store.conn.execute('SELECT 1 FROM model_pricing WHERE model_id=?', (model,)).fetchone())
