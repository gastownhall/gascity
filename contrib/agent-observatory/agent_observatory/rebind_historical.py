"""Repair only transcript-derived repository bindings; dry-run by default.

Run: python3 -m agent_observatory.rebind_historical --db PATH [--apply]
Back up the database with SQLite's online backup API before applying.
"""
from __future__ import annotations

import argparse
import json
import sqlite3
from collections import Counter
from pathlib import Path

from .gc_enrichment import _binding_sha256, _transcript_repositories


SOURCES = ('transcript_cwd', 'transcript_cwd_prefix')


def rebind(conn: sqlite3.Connection, *, apply: bool = False) -> dict:
    """Recompute historical evidence atomically, never touching explicit rows."""
    conn.row_factory = sqlite3.Row
    conn.execute('BEGIN IMMEDIATE' if apply else 'BEGIN')
    try:
        checkpoints = {}
        if conn.execute("SELECT 1 FROM sqlite_master WHERE name='collector_sources'").fetchone():
            checkpoints = {(r['source_id'], r['provider']): r['path'] for r in
                           conn.execute('SELECT source_id, provider, path FROM collector_sources')}
        rows = conn.execute(
            'SELECT * FROM session_enrichment WHERE repo_source IN (?, ?) AND repo IS NOT NULL',
            SOURCES).fetchall()
        counts = Counter()
        changes = []
        cache = {}
        for row in rows:
            identity = tuple(row[k] for k in ('city_id', 'host_id', 'provider', 'session_id'))
            evidence = set()
            paths = []
            for event in conn.execute(
                'SELECT DISTINCT source_path FROM events WHERE city_id=? AND host_id=? '
                'AND provider=? AND session_id=? AND source_path IS NOT NULL', identity):
                path = checkpoints.get((Path(event[0]).stem, row['provider']), event[0])
                paths.append(path)
                evidence.update(_transcript_repositories(path, row['provider'], row['session_id'], cache))
            repos = {repo for repo, _ in evidence}
            repo = next(iter(repos)) if len(repos) == 1 else None
            counts[(row['repo'], repo)] += 1
            if repo == row['repo']:
                continue
            source = ('transcript_cwd' if (repo, 'transcript_cwd') in evidence
                      else 'transcript_cwd_prefix') if repo else None
            changes.append({'identity': identity, 'old': row['repo'], 'new': repo,
                            'source_paths': paths})
            if apply:
                conn.execute(
                    'UPDATE session_enrichment SET repo=?, repo_source=?, source_sha256=?, '
                    "updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') "
                    'WHERE city_id=? AND host_id=? AND provider=? AND session_id=?',
                    (repo, source, _binding_sha256(identity, row['template'], repo, source), *identity))
        conn.execute('COMMIT' if apply else 'ROLLBACK')
        return {'apply': apply, 'scanned': len(rows), 'changed': len(changes),
                'table': [{'old': old, 'new': new, 'count': n}
                          for (old, new), n in counts.items()], 'changes': changes}
    except BaseException:
        if conn.in_transaction:
            conn.execute('ROLLBACK')
        raise


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--db', required=True)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    uri = Path(args.db).expanduser().resolve().as_uri() + ('?mode=rw' if args.apply else '?mode=ro')
    with sqlite3.connect(uri, uri=True) as conn:
        print(json.dumps(rebind(conn, apply=args.apply), indent=2))


if __name__ == '__main__':
    main()
