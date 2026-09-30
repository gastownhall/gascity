import sqlite3
import unittest
from unittest.mock import patch

from agent_observatory.rebind_historical import rebind


class RebindTests(unittest.TestCase):
    def setUp(self):
        self.conn = sqlite3.connect(':memory:')
        self.addCleanup(self.conn.close)
        self.conn.executescript('''
            CREATE TABLE session_enrichment (
                city_id TEXT, host_id TEXT, provider TEXT, session_id TEXT,
                template TEXT, repo TEXT, repo_source TEXT, source_sha256 TEXT,
                updated_at TEXT);
            CREATE TABLE events (city_id TEXT, host_id TEXT, provider TEXT,
                                 session_id TEXT, source_path TEXT);
        ''')
        for sid, source in [('fallback', 'transcript_cwd_prefix'), ('explicit', 'explicit')]:
            self.conn.execute('INSERT INTO session_enrichment VALUES (?,?,?,?,?,?,?,?,?)',
                              ('city', 'host', 'claude', sid, 'role', 'old/repo', source, 'old-digest', 'old-time'))
            self.conn.execute('INSERT INTO events VALUES (?,?,?,?,?)',
                              ('city', 'host', 'claude', sid, '/transcript.jsonl'))
        self.conn.commit()

    @patch('agent_observatory.rebind_historical._transcript_repositories',
           return_value={('new/repo', 'transcript_cwd_prefix')})
    def test_rewrite_explicit_untouched_and_idempotent(self, resolver):
        explicit = tuple(self.conn.execute("SELECT * FROM session_enrichment WHERE session_id='explicit'").fetchone())
        self.assertEqual(rebind(self.conn)['changed'], 1)
        self.assertEqual(self.conn.execute("SELECT repo FROM session_enrichment WHERE session_id='fallback'").fetchone()[0], 'old/repo')
        self.assertEqual(rebind(self.conn, apply=True)['changed'], 1)
        self.assertEqual(self.conn.execute("SELECT repo FROM session_enrichment WHERE session_id='fallback'").fetchone()[0], 'new/repo')
        self.assertEqual(tuple(self.conn.execute("SELECT * FROM session_enrichment WHERE session_id='explicit'").fetchone()), explicit)
        self.assertEqual(rebind(self.conn, apply=True)['changed'], 0)

    @patch('agent_observatory.rebind_historical._transcript_repositories', return_value=set())
    def test_unknown_clears_binding_and_rerun_is_noop(self, resolver):
        self.assertEqual(rebind(self.conn, apply=True)['changed'], 1)
        row = self.conn.execute("SELECT repo, repo_source, template FROM session_enrichment WHERE session_id='fallback'").fetchone()
        self.assertEqual(tuple(row), (None, None, 'role'))
        self.assertEqual(rebind(self.conn)['changed'], 0)

    @patch('agent_observatory.rebind_historical._transcript_repositories',
           return_value={('a/repo', 'transcript_cwd'), ('b/repo', 'transcript_cwd_prefix')})
    def test_conflicting_evidence_is_unknown(self, resolver):
        self.assertIsNone(rebind(self.conn)['changes'][0]['new'])

    def test_unavailable_keeps_binding(self):
        before = tuple(self.conn.execute("SELECT * FROM session_enrichment WHERE session_id='fallback'").fetchone())
        for apply in (False, True, True):
            result = rebind(self.conn, apply=apply)
            self.assertEqual(result['changed'], 0)
            self.assertEqual(result['skipped_unavailable'], 1)
            self.assertEqual(tuple(self.conn.execute("SELECT * FROM session_enrichment WHERE session_id='fallback'").fetchone()), before)

    def test_readable_no_match_clears(self):
        import tempfile
        with tempfile.NamedTemporaryFile(mode='w', suffix='.jsonl') as source:
            source.write('{}\n')
            source.flush()
            self.conn.execute('UPDATE events SET source_path=?', (source.name,))
            self.conn.commit()
            result = rebind(self.conn, apply=True)
        self.assertEqual(result['changed'], 1)
        self.assertEqual(result['skipped_unavailable'], 0)

    @patch('agent_observatory.rebind_historical._transcript_repositories',
           return_value={('old/repo', 'transcript_cwd_prefix')})
    def test_correct_binding_unchanged(self, resolver):
        before = tuple(self.conn.execute("SELECT * FROM session_enrichment WHERE session_id='fallback'").fetchone())
        self.assertEqual(rebind(self.conn, apply=True)['changed'], 0)
        self.assertEqual(tuple(self.conn.execute("SELECT * FROM session_enrichment WHERE session_id='fallback'").fetchone()), before)

    @patch('agent_observatory.rebind_historical._transcript_repositories', side_effect=RuntimeError('failure'))
    def test_failure_rolls_back(self, resolver):
        with self.assertRaises(RuntimeError):
            rebind(self.conn, apply=True)
        self.assertFalse(self.conn.in_transaction)
        self.assertEqual(self.conn.execute("SELECT repo FROM session_enrichment WHERE session_id='fallback'").fetchone()[0], 'old/repo')


if __name__ == '__main__':
    unittest.main()
