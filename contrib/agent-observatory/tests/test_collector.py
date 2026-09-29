"""M4 collector: checkpoints, replay, rotation, debounce, budgets, kill switch, queue."""

from __future__ import annotations

import contextlib
import io
import json
import os
import shutil
import subprocess
import tempfile
import time
import unittest
from unittest import mock

try:
    from . import support  # noqa: F401  (puts the package root on sys.path)
except ImportError:  # pragma: no cover
    import support  # noqa: F401

from agent_observatory.cli import main
from agent_observatory.collector import (
    DEFAULT_MAX_SOURCE_BYTES,
    KILL_SWITCH_ENV,
    CollectorConfig,
    _collector_lock,
    _drain_lock,
    backfill_missing_usage,
    collect_once,
    collect_watch,
    collector_status,
    drain_queue,
    enqueue_sessions,
    ensure_collector_schema,
    metadata_state,
    set_kill_switch,
    text_state,
)
from agent_observatory.errors import ObservatoryError
from agent_observatory.inventory import SourceRoot
from agent_observatory.jev import REQUEST_BYTE_CAP
from agent_observatory.store import ObservatoryStore
from agent_observatory.taxonomy import load_taxonomy
from agent_observatory.transport import Budget, ClassifyResult, TransportConfig, TransportError

HERE = os.path.dirname(os.path.abspath(__file__))
CLAUDE_FIXTURE = os.path.join(HERE, "fixtures", "adapters", "claude", "sample.jsonl")
CODEX_FIXTURE = os.path.join(HERE, "fixtures", "adapters", "codex", "sample.jsonl")

EXTRA_LINE = (
    '{"type":"assistant","uuid":"u-a4","sessionId":"claude-sess-1","session_id":"claude-parent-0",'
    '"timestamp":"2026-09-21T10:00:05.000Z","message":{"id":"msg-3","role":"assistant",'
    '"model":"claude-test-1","content":[{"type":"text","text":"done"}],'
    '"usage":{"input_tokens":1,"output_tokens":1}}}\n'
)
NO_ENV: dict[str, str] = {}


def _later(seconds: float = 3600.0):
    return lambda: time.time() + seconds


def _compress_zstd(source: str, target: str) -> None:
    """Compress *source* to *target* with the ``zstd`` binary, or skip."""

    binary = shutil.which("zstd")
    if binary is None:
        raise unittest.SkipTest("zstd binary is not available")
    os.makedirs(os.path.dirname(target), exist_ok=True)
    with open(source, "rb") as reader, open(target, "wb") as writer:
        process = subprocess.run(
            [binary, "-q", "-c"], stdin=reader, stdout=writer, stderr=subprocess.PIPE, check=False
        )
    if process.returncode != 0:
        raise RuntimeError(process.stderr.decode("utf-8", "replace"))


class CollectorTestCase(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp(prefix="collector-test-")
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.root = os.path.join(self.tmp, "home")
        self.claude_dir = os.path.join(self.root, ".claude", "projects", "proj")
        os.makedirs(self.claude_dir)
        self.db = os.path.join(self.tmp, "obs.db")
        self.kill = os.path.join(self.tmp, "disabled")

    def claude_source(self, name: str = "sess.jsonl") -> str:
        path = os.path.join(self.claude_dir, name)
        shutil.copyfile(CLAUDE_FIXTURE, path)
        return path

    def config(self, **overrides) -> CollectorConfig:
        values = dict(
            roots=(SourceRoot(path=self.root),),
            city_id="city-t",
            host_id="host-t",
            debounce_seconds=30.0,
            kill_switch_path=self.kill,
        )
        values.update(overrides)
        return CollectorConfig(**values)

    def collect(self, store, clock=None, **overrides):
        return collect_once(store, self.config(**overrides), clock=clock or _later(), environ=NO_ENV)

    def statuses(self, store) -> dict[str, tuple[str, str]]:
        rows = store.conn.execute("SELECT path, status, reason FROM collector_sources").fetchall()
        return {os.path.basename(row["path"]): (row["status"], row["reason"]) for row in rows}


class CollectTests(CollectorTestCase):
    def test_every_scoped_source_has_status_and_reason(self):
        self.claude_source()
        opencode = os.path.join(self.root, ".opencode")
        os.makedirs(opencode)
        with open(os.path.join(opencode, "opencode.db"), "wb") as handle:
            handle.write(b"x")
        with open(os.path.join(self.claude_dir, "sess.lock"), "w") as handle:
            handle.write("")
        locked = os.path.join(self.root, "locked")
        os.makedirs(locked)
        os.chmod(locked, 0)
        self.addCleanup(os.chmod, locked, 0o755)
        with ObservatoryStore(self.db) as store:
            run = self.collect(store)
            rows = store.conn.execute("SELECT status, reason FROM collector_sources").fetchall()
        self.assertEqual(run.status, "ok")
        self.assertEqual(run.ignored_files, {".lock": 1})
        statuses = sorted(row["status"] for row in rows)
        self.assertIn("imported", statuses)
        self.assertIn("unsupported", statuses)
        if os.geteuid() != 0:
            self.assertIn("unreadable", statuses)
        for row in rows:
            self.assertTrue(row["reason"])

    def test_replay_is_idempotent_and_unchanged(self):
        self.claude_source()
        with ObservatoryStore(self.db) as store:
            first = self.collect(store)
            events = store.event_count()
            second = self.collect(store)
            self.assertEqual(store.event_count(), events)
        self.assertGreater(first.events_inserted, 0)
        self.assertEqual(second.by_status, {"unchanged": 1})
        self.assertEqual(second.events_inserted, 0)

    def test_append_keeps_generation_and_adds_only_new_events(self):
        path = self.claude_source()
        with ObservatoryStore(self.db) as store:
            self.collect(store)
            before = store.event_count()
            with open(path, "a", encoding="utf-8") as handle:
                handle.write(EXTRA_LINE)
            os.utime(path, (time.time() + 5, time.time() + 5))
            run = self.collect(store)
            row = store.conn.execute("SELECT generation, change FROM collector_sources").fetchone()
            self.assertEqual(store.event_count(), before + 1)
        self.assertEqual(run.by_change, {"appended": 1})
        self.assertEqual(run.events_inserted, 1)
        self.assertEqual((row["generation"], row["change"]), (1, "appended"))

    def test_rewrite_increments_generation(self):
        path = self.claude_source()
        with ObservatoryStore(self.db) as store:
            self.collect(store)
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(EXTRA_LINE)
            os.utime(path, (time.time() + 5, time.time() + 5))
            run = self.collect(store)
            row = store.conn.execute("SELECT generation, change FROM collector_sources").fetchone()
        self.assertEqual(run.by_change, {"rewritten": 1})
        self.assertEqual((row["generation"], row["change"]), (2, "rewritten"))

    def test_lost_checkpoint_replays_as_duplicates(self):
        """A crash after import but before the checkpoint write loses nothing."""
        self.claude_source()
        with ObservatoryStore(self.db) as store:
            self.collect(store)
            events = store.event_count()
            store.conn.execute("DELETE FROM collector_sources")
            store.conn.execute("DELETE FROM imported_files")
            run = self.collect(store)
            self.assertEqual(store.event_count(), events)
        self.assertEqual(run.events_inserted, 0)
        self.assertEqual(run.events_duplicate, events)

    def test_failed_import_leaves_no_checkpoint_and_retries(self):
        self.claude_source()
        with ObservatoryStore(self.db) as store:
            original = store.import_jsonl

            def crash(path):
                raise OSError("disk full")

            store.import_jsonl = crash
            run = self.collect(store)
            self.assertEqual(run.by_status, {"error": 1})
            self.assertEqual(store.event_count(), 0)
            store.import_jsonl = original
            run = self.collect(store)
        self.assertEqual(run.by_status, {"imported": 1})
        self.assertGreater(run.events_inserted, 0)

    def test_rotation_keeps_counts_and_marks_old_path_missing(self):
        path = self.claude_source()
        with ObservatoryStore(self.db) as store:
            self.collect(store)
            events = store.event_count()
            os.rename(path, os.path.join(self.claude_dir, "sess-rotated.jsonl"))
            run = self.collect(store)
            statuses = self.statuses(store)
            self.assertEqual(store.event_count(), events)
        self.assertEqual(statuses["sess.jsonl"][0], "missing")
        self.assertEqual(statuses["sess-rotated.jsonl"][0], "imported")
        self.assertEqual(run.events_inserted, 0)

    def test_debounce_defers_recently_modified_files(self):
        self.claude_source()
        with ObservatoryStore(self.db) as store:
            run = self.collect(store, clock=time.time)
            self.assertEqual(run.by_status, {"debounced": 1})
            self.assertEqual(store.event_count(), 0)
            run = self.collect(store)
        self.assertEqual(run.by_status, {"imported": 1})

    def test_never_quiet_file_is_read_after_max_debounce(self):
        path = self.claude_source()
        start = time.time()
        with ObservatoryStore(self.db) as store:
            os.utime(path, (start, start))
            run = self.collect(store, clock=lambda: start + 1, max_debounce_seconds=60)
            self.assertEqual(run.by_status, {"debounced": 1})
            os.utime(path, (start + 100, start + 100))
            run = self.collect(store, clock=lambda: start + 101, max_debounce_seconds=60)
        self.assertEqual(run.by_status, {"imported": 1})

    def test_debounce_clock_starts_when_the_change_was_first_seen(self):
        # A file quiet for longer than max_debounce, then written once, must be
        # debounced: the starvation clock runs from the first sighting of the
        # pending change, not from the last import.
        path = self.claude_source()
        start = 1_000_000.0
        with ObservatoryStore(self.db) as store:
            os.utime(path, (start, start))
            run = self.collect(store, clock=lambda: start, debounce_seconds=0)
            self.assertEqual(run.by_status, {"imported": 1})
            events = store.event_count()
            os.utime(path, (start + 10_000, start + 10_000))
            run = self.collect(
                store, clock=lambda: start + 10_000, debounce_seconds=30, max_debounce_seconds=600
            )
            self.assertEqual(store.event_count(), events)
            reason = self.statuses(store)["sess.jsonl"][1]
        self.assertEqual(run.by_status, {"debounced": 1})
        self.assertIn("waits for", reason)

    def test_pending_clock_resets_when_the_source_is_replaced(self):
        # Finding 1: a deferred source's starvation clock must not carry over to
        # a *different* change. A file that replaces an oversized one long after
        # the clock expired is a new change and must be debounced, not imported
        # mid-write the instant it appears.
        path = self.claude_source()
        start = 1_000_000.0
        with ObservatoryStore(self.db) as store:
            os.utime(path, (start, start))
            run = self.collect(store, clock=lambda: start, debounce_seconds=0, max_source_bytes=100)
            self.assertEqual(run.by_status, {"deferred": 1})
            # Replace the oversized transcript with a fresh, small one, then run
            # the next pass 5s later while the old clock is far past max_debounce.
            replaced = start + 10_000
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(EXTRA_LINE)
            os.utime(path, (replaced, replaced))
            run = self.collect(
                store,
                clock=lambda: replaced + 5,
                debounce_seconds=30,
                max_debounce_seconds=600,
            )
            status, reason = self.statuses(store)["sess.jsonl"]
        self.assertEqual(run.by_status, {"debounced": 1})
        self.assertEqual(status, "debounced")
        self.assertIn("waits for", reason)

    def test_pending_clock_continues_only_for_the_same_change(self):
        # The anchor decides continuation: growth of the same file keeps the
        # clock (so a live transcript still starves), a truncation starts over.
        from agent_observatory.collector import _pending_change_since

        path = os.path.join(self.tmp, "anchor.jsonl")
        with open(path, "wb") as handle:
            handle.write(b"x" * 100)
        anchor = os.stat(path)
        start = 1_000.0
        prev = {
            "status": "deferred",
            "pending_since": start,
            "pending_raw_size": anchor.st_size,
            "pending_mtime": anchor.st_mtime,
            "pending_dev": anchor.st_dev,
            "pending_ino": anchor.st_ino,
        }
        self.assertEqual(_pending_change_since(prev, anchor, start + 5_000), (start, True))
        with open(path, "ab") as handle:
            handle.write(b"y" * 10)
        grown = os.stat(path)
        self.assertEqual(_pending_change_since(prev, grown, start + 5_000), (start, True))
        with open(path, "wb") as handle:
            handle.write(b"z")
        shrunk = os.stat(path)
        self.assertEqual(_pending_change_since(prev, shrunk, start + 5_000), (start + 5_000, False))

    def test_source_cap_defers_but_first_source_always_progresses(self):
        self.claude_source("a.jsonl")
        self.claude_source("b.jsonl")
        with ObservatoryStore(self.db) as store:
            run = self.collect(store, max_sources_per_run=1)
            self.assertEqual(run.by_status, {"imported": 1, "deferred": 1})
            run = self.collect(store, max_sources_per_run=1)
            self.assertEqual(run.by_status, {"imported": 1, "unchanged": 1})
            run = self.collect(store, max_bytes_per_run=1)
        self.assertEqual(run.by_status, {"unchanged": 2})

    def test_failed_source_waits_for_a_change_instead_of_taking_the_slot(self):
        with open(os.path.join(self.claude_dir, "a-bad.jsonl"), "wb") as handle:
            handle.write(b"\xff\xfe not utf-8 \xff\n")
        self.claude_source("z.jsonl")
        with ObservatoryStore(self.db) as store:
            run = self.collect(store, max_sources_per_run=1)
            self.assertEqual(run.by_status.get("deferred"), 1)
            failed = set(run.by_status) - {"deferred"}
            self.assertTrue(failed <= {"error", "unreadable"}, run.by_status)
            run = self.collect(store, max_sources_per_run=1)
        self.assertEqual(run.by_status.get("imported"), 1, run.by_status)
        self.assertNotIn("deferred", run.by_status)

    def test_byte_cap_and_storage_cap(self):
        self.claude_source("a.jsonl")
        self.claude_source("b.jsonl")
        with ObservatoryStore(self.db) as store:
            run = self.collect(store, max_bytes_per_run=10)
            self.assertEqual(run.by_status, {"imported": 1, "deferred": 1})
            run = self.collect(store, max_db_bytes=1)
            reasons = [reason for status, reason in self.statuses(store).values() if status == "deferred"]
        self.assertEqual(run.by_status, {"unchanged": 1, "deferred": 1})
        self.assertIn("storage cap", reasons[0])

    def test_per_source_cap_defers_even_the_first_source(self):
        self.claude_source()
        with ObservatoryStore(self.db) as store:
            run = self.collect(store, max_source_bytes=100)
            reason = self.statuses(store)["sess.jsonl"][1]
            self.assertEqual(store.event_count(), 0)
        self.assertEqual(run.by_status, {"deferred": 1})
        self.assertIn("per-source cap", reason)

    def test_max_source_bytes_default_is_a_real_cap(self):
        self.assertIsNotNone(self.config().max_source_bytes)
        self.assertEqual(self.config().max_source_bytes, DEFAULT_MAX_SOURCE_BYTES)

    def test_compressed_source_deferred_when_decompressed_exceeds_cap(self):
        # The compressed st_size is tiny, but the .zstd stream expands well past
        # the cap. The bounded reader must defer it without materializing it.
        session_dir = os.path.join(self.root, ".dsh", "sessions", "--tmp--", "session-big")
        os.makedirs(session_dir)
        plain = os.path.join(self.tmp, "big.jsonl")
        with open(plain, "w", encoding="utf-8") as handle:
            handle.write(
                '{"type":"user/message","seq":1,"time":1790000000,"data":{"content":'
                '[{"type":"text","text":"' + "x" * 200_000 + '"}]}}\n'
            )
        target = os.path.join(session_dir, "session.v3.jsonl.zstd")
        _compress_zstd(plain, target)
        self.assertLess(os.path.getsize(target), 5_000)
        with ObservatoryStore(self.db) as store:
            run = self.collect(store, max_source_bytes=1_000)
            reason = self.statuses(store)["session.v3.jsonl.zstd"][1]
            self.assertEqual(store.event_count(), 0)
        self.assertEqual(run.by_status, {"deferred": 1})
        self.assertIn("per-source cap", reason)

    @unittest.skipUnless(shutil.which("zstd"), "zstd binary is not available")
    def test_unchanged_oversized_stream_is_not_re_decompressed(self):
        # Finding 2: once a .zstd stream is known to expand past the cap, an
        # unchanged stat must not be read and re-decompressed on every pass.
        session_dir = os.path.join(self.root, ".dsh", "sessions", "--tmp--", "session-big")
        os.makedirs(session_dir)
        plain = os.path.join(self.tmp, "big.jsonl")
        with open(plain, "w", encoding="utf-8") as handle:
            handle.write(
                '{"type":"user/message","seq":1,"time":1790000000,"data":{"content":'
                '[{"type":"text","text":"' + "x" * 200_000 + '"}]}}\n'
            )
        target = os.path.join(session_dir, "session.v3.jsonl.zstd")
        _compress_zstd(plain, target)
        cap = os.path.getsize(target) + 500
        with ObservatoryStore(self.db) as store:
            from agent_observatory import collector

            real = collector.load_source_data
            calls = []

            def counting(*args, **kwargs):
                calls.append(args)
                return real(*args, **kwargs)

            with mock.patch.object(collector, "load_source_data", counting):
                first = self.collect(store, max_source_bytes=cap, debounce_seconds=0)
                second = self.collect(store, max_source_bytes=cap)
                third = self.collect(store, max_source_bytes=cap)
        self.assertEqual(first.by_status, {"deferred": 1})
        self.assertEqual(second.by_status, {"deferred": 1})
        self.assertEqual(third.by_status, {"deferred": 1})
        self.assertEqual(len(calls), 1, "unchanged oversized stream was re-read")

    @unittest.skipUnless(shutil.which("zstd"), "zstd binary is not available")
    def test_cap_deferral_refunds_the_per_run_slot(self):
        # Finding 3: a stream that expands past the cap imports nothing and must
        # not consume a per-run slot, so a healthy source sorted behind it still
        # imports in the same pass instead of being wedged forever.
        big_dir = os.path.join(self.root, ".dsh", "sessions", "aaa-big")
        ok_dir = os.path.join(self.root, ".dsh", "sessions", "zzz-ok")
        os.makedirs(big_dir)
        os.makedirs(ok_dir)
        plain = os.path.join(self.tmp, "big.jsonl")
        with open(plain, "w", encoding="utf-8") as handle:
            handle.write(
                '{"type":"user/message","seq":1,"time":1790000000,"data":{"content":'
                '[{"type":"text","text":"' + "x" * 200_000 + '"}]}}\n'
            )
        _compress_zstd(plain, os.path.join(big_dir, "session.v3.jsonl.zstd"))
        _compress_zstd(
            os.path.join(HERE, "fixtures", "adapters", "dsh", "sample.jsonl"),
            os.path.join(ok_dir, "session.v3.jsonl.zstd"),
        )
        with ObservatoryStore(self.db) as store:
            run = self.collect(
                store, max_source_bytes=10_000, max_sources_per_run=1, debounce_seconds=0
            )
            healthy = store.conn.execute(
                "SELECT status FROM collector_sources WHERE path LIKE ?", ("%zzz-ok%",)
            ).fetchone()
        self.assertEqual(run.by_status, {"deferred": 1, "imported": 1}, run.by_status)
        self.assertEqual(healthy["status"], "imported")

    def test_schema_migration_tolerates_a_concurrent_duplicate_column(self):
        # Finding 4: two first-start processes can both pass the column check and
        # both ALTER; the loser must degrade to the existing column, not crash.
        with ObservatoryStore(self.db) as store:
            ensure_collector_schema(store.conn)

            class _StaleColumns:
                """Report the pre-migration column set while forwarding DDL."""

                def __init__(self, conn):
                    self._conn = conn

                def execute(self, sql, *args):
                    if sql.startswith("PRAGMA table_info"):
                        return [{"name": "realpath"}, {"name": "source_id"}]
                    return self._conn.execute(sql, *args)

            # The real columns already exist, so every additive ALTER this stale
            # column set provokes is a lost race and must be tolerated.
            ensure_collector_schema(_StaleColumns(store.conn))
            names = {row["name"] for row in store.conn.execute("PRAGMA table_info(collector_sources)")}
        self.assertIn("pending_since", names)
        self.assertIn("deferral_terminal", names)

    def test_kill_switch_file_and_env_stop_collection(self):
        self.claude_source()
        with ObservatoryStore(self.db) as store:
            set_kill_switch(self.kill, True)
            run = self.collect(store)
            self.assertEqual(run.status, "disabled")
            self.assertEqual(store.event_count(), 0)
            set_kill_switch(self.kill, False)
            run = collect_once(store, self.config(), clock=_later(), environ={KILL_SWITCH_ENV: "1"})
            self.assertEqual(run.status, "disabled")
            run = self.collect(store)
        self.assertEqual(run.status, "ok")

    def test_watch_stops_when_switch_engages(self):
        self.claude_source()
        sleeps = []
        with ObservatoryStore(self.db) as store:
            runs = collect_watch(
                store,
                self.config(),
                interval_seconds=1,
                clock=_later(),
                sleeper=lambda seconds: (sleeps.append(seconds), set_kill_switch(self.kill, True)),
                environ=NO_ENV,
            )
        self.assertEqual([run.status for run in runs], ["ok", "disabled"])
        self.assertEqual(sleeps, [1])

    def test_concurrent_collector_is_locked_out(self):
        self.claude_source()
        with ObservatoryStore(self.db) as store:
            with _collector_lock(self.db) as acquired:
                self.assertTrue(acquired)
                run = self.collect(store)
        self.assertEqual(run.status, "locked")

    def test_advisory_lock_keys_on_realpath_not_the_symlink_spelling(self):
        link = os.path.join(self.tmp, "obs-link.db")
        os.symlink(self.db, link)
        with _collector_lock(link) as first:
            self.assertTrue(first)
            with _collector_lock(self.db) as second:
                self.assertFalse(second)

    def test_config_validation(self):
        with self.assertRaises(ObservatoryError):
            self.config(roots=())
        with self.assertRaises(ObservatoryError):
            self.config(max_sources_per_run=0)
        with self.assertRaises(ObservatoryError):
            self.config(debounce_seconds=-1)
        with self.assertRaises(ObservatoryError):
            self.config(debounce_seconds=60, max_debounce_seconds=10)


def _fake_classify(outcomes):
    calls = []

    def fake(store, request, *, config):
        outcome, failure = outcomes[min(len(calls), len(outcomes) - 1)]
        calls.append(request)
        if outcome == "raise":
            raise TransportError("corrupt circuit row")
        config.budget.record_request()
        return ClassifyResult(
            outcome=outcome,
            request_hash=request.request_hash,
            snapshot_hash=request.snapshot_hash,
            subject_kind=request.subject_kind,
            taxonomy_version=request.taxonomy_version,
            question_hash=request.question_hash,
            model=request.model,
            endpoint="test",
            classification_id=7 if outcome == "classified" else None,
            failure_class=failure,
            error=None if failure is None else f"{failure} detail",
        )

    return fake, calls




class UsageBackfillTests(CollectorTestCase):
    def test_backfill_is_resumable_and_idempotent_with_source_size_cap(self):
        source = os.path.join(self.claude_dir, "usage-backfill.jsonl")
        transcript_record = {
            "type": "assistant",
            "uuid": "usage-event",
            "sessionId": "usage-backfill-session",
            "timestamp": "2026-09-28T12:00:00.000Z",
            "message": {
                "id": "usage-message",
                "role": "assistant",
                "content": [{"type": "text", "text": "Synthetic usage evidence"}],
            },
        }
        with open(source, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(transcript_record) + "\n")

        with ObservatoryStore(self.db) as store:
            self.collect(store)
            event = store.conn.execute(
                "SELECT city_id, host_id, provider, session_id, event_id FROM events"
            ).fetchone()
            identity = tuple(event)
            transcript_record["message"]["usage"] = {"input_tokens": 4, "output_tokens": 2}
            with open(source, "w", encoding="utf-8") as handle:
                handle.write(json.dumps(transcript_record) + "\n")
            store.conn.execute("DELETE FROM event_usage")
            store.conn.execute("DELETE FROM collector_sources")
            roots = (SourceRoot(path=self.root),)

            deferred = backfill_missing_usage(store, max_source_bytes=1, roots=roots)
            self.assertEqual(deferred.sources_deferred, 1)
            self.assertEqual(deferred.usage_inserted, 0)
            self.assertIsNone(store.get_usage(identity))

            first = backfill_missing_usage(
                store, max_source_bytes=DEFAULT_MAX_SOURCE_BYTES, roots=roots
            )
            self.assertEqual(first.sources_read, 1)
            self.assertEqual(first.events_matched, 1)
            self.assertEqual(first.usage_inserted, 1)
            usage = store.get_usage(identity)
            self.assertEqual(usage["input_tokens"], 4)
            self.assertEqual(usage["output_tokens"], 2)

            second = backfill_missing_usage(
                store, max_source_bytes=DEFAULT_MAX_SOURCE_BYTES, roots=roots
            )
            self.assertEqual(second.candidate_events, 0)
            self.assertEqual(second.usage_inserted, 0)

    def test_usage_backfill_survives_payload_rewrite_for_same_stable_event_key(self):
        source = os.path.join(self.claude_dir, "rewritten-usage.jsonl")
        transcript_record = {
            "type": "assistant",
            "uuid": "synthetic-usage-event",
            "sessionId": "synthetic-usage-session",
            "timestamp": "2026-09-28T12:00:00.000Z",
            "message": {
                "id": "synthetic-usage-message",
                "role": "assistant",
                "model": "claude-test-1",
                "content": [{"type": "text", "text": "Synthetic original event text"}],
            },
        }
        with open(source, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(transcript_record) + "\n")

        with ObservatoryStore(self.db) as store:
            self.collect(store)
            event_before = dict(
                store.conn.execute(
                    "SELECT payload_hash, text FROM events WHERE provider = 'claude'"
                ).fetchone()
            )
            store.conn.execute("DELETE FROM event_usage")

            # A provider rewrite changes prose while preserving the native event
            # identity and the event-defining time/kind/model fields.
            transcript_record["message"]["content"][0]["text"] = "Synthetic rewritten event text"
            transcript_record["message"]["usage"] = {"input_tokens": 9, "output_tokens": 3}
            with open(source, "w", encoding="utf-8") as handle:
                handle.write(json.dumps(transcript_record) + "\n")

            run = backfill_missing_usage(store, roots=(SourceRoot(path=self.root),))
            event_after = store.conn.execute(
                "SELECT payload_hash, text FROM events WHERE provider = 'claude'"
            ).fetchone()
            usage = store.conn.execute(
                "SELECT input_tokens, output_tokens FROM event_usage WHERE provider = 'claude'"
            ).fetchone()

        self.assertEqual(run.events_identity_matched, 1)
        self.assertEqual(run.events_matched, 1)
        self.assertEqual(run.events_stable_field_rejected, 0)
        self.assertEqual(run.usage_inserted, 1)
        self.assertEqual(dict(event_after), event_before)
        self.assertEqual(tuple(usage), (9, 3))

    def test_usage_backfill_rejects_same_key_when_model_changes(self):
        source = os.path.join(self.claude_dir, "changed-model-usage.jsonl")
        transcript_record = {
            "type": "assistant",
            "uuid": "synthetic-model-event",
            "sessionId": "synthetic-model-session",
            "timestamp": "2026-09-28T12:00:00.000Z",
            "message": {
                "id": "synthetic-model-message",
                "role": "assistant",
                "model": "claude-test-1",
                "content": [{"type": "text", "text": "Synthetic event text"}],
            },
        }
        with open(source, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(transcript_record) + "\n")

        with ObservatoryStore(self.db) as store:
            self.collect(store)
            store.conn.execute("DELETE FROM event_usage")
            transcript_record["message"]["model"] = "claude-test-2"
            transcript_record["message"]["usage"] = {"input_tokens": 9, "output_tokens": 3}
            with open(source, "w", encoding="utf-8") as handle:
                handle.write(json.dumps(transcript_record) + "\n")

            run = backfill_missing_usage(store, roots=(SourceRoot(path=self.root),))
            usage_count = store.conn.execute("SELECT COUNT(*) FROM event_usage").fetchone()[0]

        self.assertEqual(run.events_identity_matched, 1)
        self.assertEqual(run.events_matched, 0)
        self.assertEqual(run.events_stable_field_rejected, 1)
        self.assertEqual(run.usage_inserted, 0)
        self.assertEqual(usage_count, 0)

    def test_collector_persists_adapter_session_fingerprints(self):
        source = os.path.join(self.claude_dir, "repo-evidence.jsonl")
        fixture = os.path.join(HERE, "fixtures", "adapters", "claude", "repo-evidence.jsonl")
        shutil.copyfile(fixture, source)

        with ObservatoryStore(self.db) as store:
            run = self.collect(store)
            fingerprints = store.load_session_fingerprints()
            repos = {
                row["repo"] for row in store.conn.execute("SELECT DISTINCT repo FROM events")
                if row["repo"]
            }

        self.assertGreater(run.session_fingerprints_inserted, 0)
        self.assertTrue(fingerprints)
        self.assertEqual({item["value"] for item in fingerprints}, {
            "1111111",
            "1111111111111111111111111111111111111111",
            "2222222222222222222222222222222222222222",
        })
        self.assertEqual(repos, {"hoomji/gascity"})

    def test_backfill_without_root_reports_missing_checkpoint(self):
        source = os.path.join(self.claude_dir, "missing-checkpoint.jsonl")
        transcript_record = {
            "type": "user",
            "uuid": "missing-checkpoint-event",
            "sessionId": "missing-checkpoint-session",
            "timestamp": "2026-09-28T12:00:00.000Z",
            "message": {
                "role": "user",
                "content": [{"type": "text", "text": "Synthetic missing-checkpoint event"}],
            },
        }
        with open(source, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(transcript_record) + "\n")

        with ObservatoryStore(self.db) as store:
            self.collect(store)
            store.conn.execute("DELETE FROM collector_sources")
            os.unlink(source)
            run = backfill_missing_usage(store)
            self.assertEqual(run.candidate_events, 1)
            self.assertEqual(run.unmapped_sources, 1)
            self.assertEqual(run.status, "incomplete")
            self.assertIn("--root", run.reason)

        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = main(["backfill-usage", "--db", self.db])
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(output.getvalue())["status"], "incomplete")

class QueueTests(CollectorTestCase):
    def setUp(self) -> None:
        super().setUp()
        self.claude_source()
        self.store = ObservatoryStore(self.db)
        self.addCleanup(self.store.close)
        self.run = self.collect(self.store)
        self.taxonomy = load_taxonomy(None)

    def drain(self, outcomes, now=None, **kwargs):
        fake, calls = _fake_classify(outcomes)
        config = kwargs.pop("transport_config", TransportConfig(budget=Budget(max_requests=10)))
        result = drain_queue(
            self.store,
            self.taxonomy,
            transport_config=config,
            max_items=kwargs.pop("max_items", 10),
            classify_fn=fake,
            clock=now or _later(),
            environ=NO_ENV,
            **kwargs,
        )
        return result, calls

    def queue(self):
        return [dict(row) for row in self.store.conn.execute("SELECT * FROM collector_queue ORDER BY queue_id")]

    def test_collect_enqueues_changed_sessions_once(self):
        self.assertEqual(self.run.sessions_enqueued, 1)
        self.assertEqual(self.collect(self.store).sessions_enqueued, 0)
        self.assertEqual(len(self.queue()), 1)

    def test_crash_before_checkpoint_still_queues_sessions(self):
        from unittest import mock

        from agent_observatory import collector

        path = self.claude_source("second.jsonl")
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(EXTRA_LINE.replace("claude-sess-1", "claude-sess-2"))
        with mock.patch.object(collector, "enqueue_sessions", side_effect=RuntimeError("killed")):
            with self.assertRaises(RuntimeError):
                self.collect(self.store)
        run = self.collect(self.store)
        self.assertEqual(run.by_status.get("imported"), 1, run.by_status)
        self.assertGreaterEqual(run.sessions_enqueued, 1)

    def test_classified_item_is_done(self):
        result, calls = self.drain([("classified", None)])
        self.assertEqual((result.classified, len(calls)), (1, 1))
        self.assertEqual(self.queue()[0]["status"], "done")
        self.assertEqual(self.queue()[0]["classification_id"], 7)

    def test_repeated_failures_end_in_unknown_queue(self):
        result, _ = self.drain([("pending", "http_500")], max_attempts=2, retry_backoff_seconds=10)
        self.assertEqual(result.retry_scheduled, 1)
        item = self.queue()[0]
        self.assertEqual((item["status"], item["attempts"]), ("pending", 1))
        result, calls = self.drain([("pending", "http_500")], max_attempts=2)
        self.assertEqual(calls, [])  # not due yet
        result, _ = self.drain([("pending", "http_500")], now=_later(7200), max_attempts=2)
        self.assertEqual(result.unknown, 1)
        item = self.queue()[0]
        self.assertEqual((item["status"], item["last_failure_class"]), ("unknown", "http_500"))
        status = collector_status(self.store, now=time.time(), environ=NO_ENV)
        self.assertEqual(status["queue"]["by_status"]["unknown"], 1)
        self.assertEqual(status["queue"]["unknown"][0]["last_failure_class"], "http_500")

    def test_budget_and_circuit_stop_without_charging_attempts(self):
        for failure in ("budget_exhausted", "circuit_open", "credential_error", "http_401", "http_403", "http_429", "http_529"):
            result, _ = self.drain([("pending", failure)])
            self.assertEqual(result.status, "stopped")
            item = self.queue()[0]
            self.assertEqual((item["status"], item["attempts"]), ("pending", 0))

    def test_budget_ceiling_checked_before_sending(self):
        config = TransportConfig(budget=Budget(max_requests=0))
        result, calls = self.drain([("classified", None)], transport_config=config)
        self.assertEqual((result.status, calls), ("stopped", []))
        self.assertIn("request cap", result.reason)

    def test_transport_error_stops_drain(self):
        result, _ = self.drain([("raise", None)])
        self.assertEqual(result.status, "stopped")
        self.assertEqual(self.queue()[0]["last_failure_class"], "transport_error")

    def test_transport_error_charges_an_attempt_and_parks_after_max(self):
        result, _ = self.drain([("raise", None)], max_attempts=1, retry_backoff_seconds=0)
        self.assertEqual(result.status, "stopped")
        item = self.queue()[0]
        self.assertEqual(item["status"], "unknown")
        self.assertEqual(item["attempts"], 1)
        self.assertEqual(item["last_failure_class"], "transport_error")

    def test_overlapping_drain_is_locked_out(self):
        with _drain_lock(self.db) as acquired:
            self.assertTrue(acquired)
            result, calls = self.drain([("classified", None)])
        self.assertEqual(result.status, "locked")
        self.assertEqual(calls, [])
        self.assertEqual(self.queue()[0]["status"], "pending")

    def test_drain_request_byte_cap_is_charged_not_raised(self):
        from agent_observatory.jev import REQUEST_BYTE_CAP

        def oversized(store, key):
            return {"state_kind": "session_metadata", "padding": "x" * (REQUEST_BYTE_CAP + 1)}

        result, calls = self.drain(
            [("classified", None)],
            state_builder=oversized,
            max_attempts=2,
            retry_backoff_seconds=10,
        )
        self.assertEqual(calls, [])
        item = self.queue()[0]
        self.assertEqual(item["status"], "pending")
        self.assertEqual(item["attempts"], 1)
        self.assertEqual(item["last_failure_class"], "request_byte_cap")
        self.assertEqual(result.retry_scheduled, 1)

    def test_changed_session_supersedes_stale_item(self):
        path = os.path.join(self.claude_dir, "sess.jsonl")
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(EXTRA_LINE)
        os.utime(path, (time.time() + 5, time.time() + 5))
        run = self.collect(self.store)
        self.assertEqual((run.sessions_enqueued, run.queue_superseded), (1, 1))
        result, calls = self.drain([("classified", None)])
        self.assertEqual((result.classified, len(calls)), (1, 1))
        self.assertEqual([item["status"] for item in self.queue()], ["superseded", "done"])

    def test_drain_respects_kill_switch(self):
        set_kill_switch(self.kill, True)
        result, calls = self.drain([("classified", None)], kill_switch_path=self.kill)
        self.assertEqual((result.status, calls), ("disabled", []))

    def test_metadata_state_carries_no_transcript_text(self):
        key = self.store.session_keys()[0]
        state = metadata_state(self.store, key)
        encoded = json.dumps(state)
        for secret in ("Please fix the bug", "supersecretvalue", "echo hi", "First title", "Second title"):
            self.assertNotIn(secret, encoded)
        self.assertEqual(state["state_kind"], "session_metadata")
        self.assertGreater(state["events"], 0)
        _, calls = self.drain([("classified", None)])
        self.assertNotIn("Please fix the bug", json.dumps(calls[0].body))

    def test_metadata_mode_keeps_raw_snapshot_identity(self):
        key = self.store.session_keys()[0]
        raw = self.store.session_snapshot(key)
        result, calls = self.drain([("classified", None)])
        self.assertEqual(result.items[0]["state_mode"], "metadata")
        self.assertEqual(calls[0].snapshot_hash, raw)
        self.assertEqual(result.items[0]["request_snapshot_hash"], raw)

    def test_text_state_carries_redacted_transcript_text(self):
        key = self.store.session_keys()[0]
        state = text_state(self.store, key)
        self.assertEqual(state["state_kind"], "session_transcript")
        self.assertTrue(state["text_mode"]["enabled"])
        self.assertTrue(state["text_mode"]["redacted"])
        encoded = json.dumps(state)
        self.assertIn("Please fix the bug", encoded)
        self.assertIn("[REDACTED]", encoded)
        self.assertNotIn("supersecretvalue", encoded)

    def test_text_drain_sends_redacted_text_and_distinct_snapshot(self):
        key = self.store.session_keys()[0]
        raw = self.store.session_snapshot(key)
        result, calls = self.drain([("classified", None)], state_mode="text")
        self.assertEqual(result.attempted, 1)
        request = calls[0]
        body = json.dumps(request.body)
        self.assertIn("Please fix the bug", body)
        self.assertNotIn("supersecretvalue", body)
        self.assertIn("[REDACTED]", body)
        self.assertEqual(request.body["state"]["state_kind"], "session_transcript")
        # Text mode is a distinct data scope: it must not share the metadata
        # subject snapshot (which would collide in ``classifications``).
        self.assertNotEqual(request.snapshot_hash, raw)
        self.assertEqual(result.items[0]["state_mode"], "text")
        self.assertEqual(result.items[0]["snapshot_hash"], raw)
        self.assertEqual(result.items[0]["request_snapshot_hash"], request.snapshot_hash)

    def test_text_mode_respects_the_request_byte_cap(self):
        path = os.path.join(self.claude_dir, "big.jsonl")
        with open(path, "w", encoding="utf-8") as handle:
            for index in range(40):
                handle.write(
                    json.dumps(
                        {
                            "type": "user",
                            "uuid": f"u-big-{index}",
                            "sessionId": "claude-sess-1",
                            "session_id": "claude-parent-0",
                            "timestamp": f"2026-09-21T11:00:{index:02d}.000Z",
                            "message": {
                                "role": "user",
                                "content": [
                                    {
                                        "type": "text",
                                        "text": f"line-{index} api_key=supersecret{index} " + "x" * 4000,
                                    }
                                ],
                            },
                        }
                    )
                    + "\n"
                )
        self.collect(self.store)
        result, calls = self.drain([("classified", None)], state_mode="text")
        self.assertEqual(result.attempted, 1)
        request = calls[0]
        self.assertLessEqual(request.byte_length, REQUEST_BYTE_CAP)
        state = request.body["state"]
        self.assertGreaterEqual(state["text_mode"]["request_dropped_excerpts"], 1)
        self.assertLessEqual(len(state["excerpts"]), state["events"])
        self.assertTrue(any("line-" in excerpt["text"] for excerpt in state["excerpts"]))
        self.assertNotIn("supersecret", json.dumps(state))

    def test_drain_rejects_unknown_state_mode(self):
        with self.assertRaises(ObservatoryError):
            self.drain([("classified", None)], state_mode="bogus")

    def test_enqueue_ignores_unknown_session(self):
        self.assertEqual(enqueue_sessions(self.store, [("c", "h", "p", "nope")]), (0, 0))


class StatusAndCliTests(CollectorTestCase):
    def test_status_reports_coverage_and_lag(self):
        self.claude_source("a.jsonl")
        self.claude_source("b.jsonl")
        with ObservatoryStore(self.db) as store:
            self.collect(store, max_sources_per_run=1)
            status = collector_status(store, now=time.time() + 100, kill_switch_path=self.kill, environ=NO_ENV)
        self.assertEqual(status["sources"]["scoped"], 2)
        self.assertEqual(status["sources"]["current"], 1)
        self.assertEqual(status["sources"]["coverage"], 0.5)
        self.assertEqual(len(status["sources"]["lagging"]), 1)
        self.assertGreater(status["sources"]["max_lag_seconds"], 0)
        self.assertIsNone(status["disabled"])
        self.assertEqual(status["last_run"]["status"], "ok")

    def run_cli(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = main(list(argv))
        return code, out.getvalue(), err.getvalue()

    def test_cli_collect_status_and_switch(self):
        self.claude_source()
        common = ["--db", self.db, "--kill-switch", self.kill]
        base = ["collect", *common, "--root", self.root, "--city", "c", "--host", "h", "--debounce-seconds", "0"]
        code, out, _ = self.run_cli(*base)
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out)["by_status"], {"imported": 1})
        code, out, _ = self.run_cli("collector-switch", *common, "off")
        self.assertEqual(json.loads(out)["collector"], "disabled")
        code, out, _ = self.run_cli(*base)
        self.assertEqual(json.loads(out)["status"], "disabled")
        code, out, _ = self.run_cli("collect-status", *common)
        self.assertEqual(code, 0)
        status = json.loads(out)
        self.assertIn("kill switch", status["disabled"])
        self.assertEqual(status["queue"]["by_status"]["pending"], 1)
        self.run_cli("collector-switch", *common, "on")
        code, out, _ = self.run_cli(*base, "--watch", "--interval", "0.01", "--iterations", "2")
        self.assertEqual(code, 0)
        self.assertEqual(out.count('"status": "ok"'), 2)

    def test_cli_collect_defaults_max_source_bytes(self):
        from agent_observatory.cli import build_parser

        args = build_parser().parse_args(
            ["collect", "--db", self.db, "--root", self.root, "--city", "c", "--host", "h"]
        )
        self.assertEqual(args.max_source_bytes, DEFAULT_MAX_SOURCE_BYTES)


    def test_cli_backfill_usage_exposes_source_byte_cap(self):
        from agent_observatory.cli import build_parser

        default = build_parser().parse_args(["backfill-usage", "--db", self.db])
        explicit = build_parser().parse_args(
            [
                "backfill-usage",
                "--db",
                self.db,
                "--root",
                self.root,
                "--max-source-bytes",
                "1048576",
            ]
        )
        self.assertEqual(default.max_source_bytes, DEFAULT_MAX_SOURCE_BYTES)
        self.assertEqual(explicit.max_source_bytes, 1048576)
        self.assertEqual(explicit.root, [self.root])

    def test_cli_queue_drain_requires_request_ceiling(self):
        code, _, err = self.run_cli("queue-drain", "--db", self.db)
        self.assertEqual(code, 1)
        self.assertIn("--max-requests", err)

    def test_cli_refuses_cost_ceiling_without_prices(self):
        code, _, err = self.run_cli("queue-drain", "--db", self.db, "--max-requests", "1", "--max-cost-usd", "0.5")
        self.assertEqual(code, 1)
        self.assertIn("--price-per-million-input-usd", err)

    def test_cli_queue_drain_without_credential_stays_pending(self):
        self.claude_source()
        common = ["--db", self.db, "--kill-switch", self.kill]
        self.run_cli("collect", *common, "--root", self.root, "--city", "c", "--host", "h", "--debounce-seconds", "0")
        saved = {name: os.environ.pop(name) for name in ("TYPESAFE_API_KEY", "JEV_API_KEY", "JEV_KEY_FILE") if name in os.environ}
        self.addCleanup(os.environ.update, saved)
        code, out, _ = self.run_cli("queue-drain", *common, "--max-requests", "1")
        self.assertEqual(code, 1)
        result = json.loads(out)
        self.assertEqual(result["items"][0]["failure_class"], "credential_error")
        with ObservatoryStore(self.db) as store:
            row = store.conn.execute("SELECT status, attempts FROM collector_queue").fetchone()
        self.assertEqual((row["status"], row["attempts"]), ("pending", 0))

    def test_cli_queue_drain_text_state_flag_opts_in(self):
        self.claude_source()
        common = ["--db", self.db, "--kill-switch", self.kill]
        self.run_cli("collect", *common, "--root", self.root, "--city", "c", "--host", "h", "--debounce-seconds", "0")
        saved = {name: os.environ.pop(name) for name in ("TYPESAFE_API_KEY", "JEV_API_KEY", "JEV_KEY_FILE") if name in os.environ}
        self.addCleanup(os.environ.update, saved)
        code, out, _ = self.run_cli("queue-drain", *common, "--max-requests", "1", "--text-state")
        self.assertEqual(code, 1)  # no credential: execution outcome, not a crash
        item = json.loads(out)["items"][0]
        self.assertEqual(item["state_mode"], "text")
        self.assertTrue(item["text_mode"]["enabled"])

    def test_cli_queue_drain_is_text_state_by_default(self):
        from agent_observatory import cli as cli_module
        from agent_observatory.cli import build_parser

        args = build_parser().parse_args(["queue-drain", "--db", self.db, "--max-requests", "1"])
        self.assertFalse(args.metadata_state)
        metadata = build_parser().parse_args(
            ["queue-drain", "--db", self.db, "--max-requests", "1", "--metadata-state"]
        )
        self.assertTrue(metadata.metadata_state)

        captured: dict[str, str] = {}

        class _Result:
            status = "ok"

            def to_dict(self):
                return {}

        def fake_drain(store, taxonomy, **kwargs):
            captured["state_mode"] = kwargs["state_mode"]
            return _Result()

        with mock.patch.object(cli_module, "drain_queue", side_effect=fake_drain):
            with contextlib.redirect_stdout(io.StringIO()):
                cli_module._cmd_queue_drain(args)
        # The parser default is not enough: the handler must actually pass the
        # text scope through to drain_queue.
        self.assertEqual(captured["state_mode"], "text")

        with mock.patch.object(cli_module, "drain_queue", side_effect=fake_drain):
            with contextlib.redirect_stdout(io.StringIO()):
                cli_module._cmd_queue_drain(metadata)
        self.assertEqual(captured["state_mode"], "metadata")

    def test_cli_queue_drain_include_done_flag(self):
        from agent_observatory.cli import build_parser

        args = build_parser().parse_args(
            ["queue-drain", "--db", self.db, "--max-requests", "1", "--include-done"]
        )
        self.assertTrue(args.include_done)


class FrameworkStateTests(CollectorTestCase):
    def _write_session(self, path):
        lines = [
            {
                "type": "user",
                "uuid": "u-fw",
                "sessionId": "claude-fw",
                "timestamp": "2026-09-21T10:00:00.000Z",
                "message": {
                    "role": "user",
                    "content": [
                        {
                            "type": "text",
                            "text": (
                                "[city] gateway-llm/gc.worker-1 • 2026-09-21T10:00:00\n\n"
                                "# GC Role Worker\n\nYou are a role worker. gc hook --claim"
                            ),
                        }
                    ],
                },
            },
            {
                "type": "user",
                "uuid": "u-task",
                "sessionId": "claude-fw",
                "timestamp": "2026-09-21T10:00:01.000Z",
                "message": {
                    "role": "user",
                    "content": [{"type": "text", "text": "Please fix the flaky scheduler test"}],
                },
            },
        ]
        with open(path, "w", encoding="utf-8") as handle:
            for line in lines:
                handle.write(json.dumps(line) + "\n")
        return path

    def test_text_state_drops_framework_payloads(self):
        self._write_session(os.path.join(self.claude_dir, "fw.jsonl"))
        with ObservatoryStore(self.db) as store:
            self.collect(store)
            key = store.session_keys()[0]
            state = text_state(store, key)
        self.assertGreaterEqual(state["text_mode"]["injected_events_dropped"], 1)
        self.assertIn("framework_filter_version", state["text_mode"])
        encoded = json.dumps(state)
        self.assertIn("Please fix the flaky scheduler test", encoded)
        self.assertNotIn("GC Role Worker", encoded)
        self.assertNotIn("gc hook --claim", encoded)

    def test_include_done_reclassifies_in_a_new_data_scope(self):
        path = self._write_session(os.path.join(self.claude_dir, "fw.jsonl"))
        with ObservatoryStore(self.db) as store:
            self.collect(store)
        fake, calls = _fake_classify([("classified", None)])
        config = TransportConfig(budget=Budget(max_requests=10))
        with ObservatoryStore(self.db) as store:
            first = drain_queue(
                store, load_taxonomy(None), transport_config=config, max_items=10,
                classify_fn=fake, clock=_later(), environ=NO_ENV,
            )
            self.assertEqual((first.classified, len(calls)), (1, 1))
            fake2, calls2 = _fake_classify([("classified", None)])
            second = drain_queue(
                store, load_taxonomy(None), transport_config=TransportConfig(budget=Budget(max_requests=10)),
                max_items=10, classify_fn=fake2, clock=_later(), environ=NO_ENV,
                state_mode="text", include_done=True,
            )
            self.assertEqual((second.classified, len(calls2)), (1, 1))
            self.assertEqual(calls2[0].body["state"]["state_kind"], "session_transcript")
            row = store.conn.execute("SELECT status FROM collector_queue").fetchone()
            self.assertEqual(row["status"], "done")


if __name__ == "__main__":
    unittest.main()
