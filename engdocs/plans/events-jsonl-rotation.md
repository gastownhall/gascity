# Plan: events.jsonl in-process rotation

**Source bead:** `ga-b6y1` — designer's
decomposition + operator UX.
**Architecture parent:** `ga-9hf7` — closed,
recommendation: in-process size-triggered rotation in `FileRecorder`
with archive-aware reads, gzip-archive sibling files, default
unbounded retention with opt-in age cap.
**Owner pipeline:** architect → designer → **pm (this plan)** →
builder.

## Why this exists

`events.jsonl` grows without bound (~65 MB/day, ~50 K events/day on
the `gc-management` canary city). Today: 1.4 GB on disk, 1.1 M lines,
22 days of history. The architecture doc itself flagged this as a
known gap (`engdocs/architecture/event-bus.md:391-394`). Operators
have no built-in way to rotate, archive, or bound the active log;
manual truncation loses sequence continuity, and external
`logrotate` would require SIGUSR1 + cooperation that the recorder
doesn't have.

The architect (ga-9hf7) settled the technical shape — in-process
size-triggered rotation, gzip in place, archive-aware reads, default
keep-everything. The designer (ga-b6y1) settled the bead boundaries,
the operator-facing UX contract for `gc events rotate`, the
verbatim error catalog, and three open implementation questions.
This plan partitions the designer's work into four builder beads
that can land in parallel after B-1 closes.

## Decomposition

| Bead | Title | Effort | Surface | Depends on |
|---|---|---|---|---|
| `ga-b6y1.1` | B-1 — core rotation + archive-aware reads | ~1.5 day | `internal/events/` only | parent |
| `ga-b6y1.2` | B-2 — `[events.rotation]` config plumbing | ~0.5 day | `internal/config/`, `cmd/gc/providers.go` | B-1 |
| `ga-b6y1.3` | B-3 — force-rotate API + `gc events rotate` CLI | ~1 day | `internal/api/`, `cmd/gc/cmd_events.go`, OpenAPI regen | B-1 |
| `ga-b6y1.4` | B-4 — legacy archive migration + crash reaper | ~0.5 day | `internal/events/recorder.go` (constructor) | B-1 |

**Landing order.** B-1 first (foundation). Then B-2, B-3, B-4 unblock
together and may land in any order. Default `city.toml` continues to
work without B-2 (rotation enabled with defaults baked into B-1).
Supervisor continues to work without B-3 (operator can wait for
size-trigger). Recorder continues to work without B-4 (legacy
archives are read by B-1's `archiveListFor` glob; B-4 only normalises
the filename so the skip-fast path doesn't have to gunzip them).

**Why bundle architect's "A" + "B" candidates into B-1.** The
conformance subtest `"rotation preserves invariants"` covers
(a) seq monotonic across rotation, (b) `ReadAll` covers
active+archives, (c) `Watch` survives rotation. Items (b) and (c)
cannot be exercised without the read path being archive-aware.
Splitting them forces synthesised archive fixtures rather than
archives produced by the rotation path itself, losing the
round-trip guarantee that the conformance suite exists to provide.
(Designer's §2 rationale.)

## What's preserved verbatim from the designer

These contracts are pinned — if the builder changes them, log
scrapers and runbooks break:

- **CLI output schema** (designer §3, B-3 body).
- **Error catalog** (designer §6, reproduced in B-3 body) —
  exact stderr wording for every failure mode, exit code 1.
- **Empty-log no-op contract** — JSONL `{"rotated":false,…}` to
  stdout, exit 0; not an error.
- **`compression_status` lifecycle** — `pending` returned
  immediately; `complete` only after gzip + rename succeeds.
- **Accessibility / scriptability requirements** (designer §7) —
  no ANSI escapes ever, RFC3339-UTC timestamps, exit codes 0 or 1.

## What's preserved verbatim from the architect

- **ZFC invariant.** No imports from `internal/orders/`,
  `internal/api/`, or `cmd/gc/` into `internal/events/`. Rotation
  lives entirely in Layer 0-1.
- **Hot path budget.** ≤ 1 ms per `Record()` in steady state
  (NFR-01).
- **Critical section bound.** Recorder mutex held for
  close+rename+open+one-write+fsync only — tens of ms, never
  seconds (NFR-02). Gzip runs in a background goroutine.
- **Keep-everything default.** `archive_retain_age` empty/unset =
  archives kept forever. Deletion is opt-in only; warn at config
  load if the operator sets it under 7 days.
- **Crash safety.** Half-finished rotations either complete on
  restart or are detected and recovered, never lose data (NFR-06).
  Orphaned `.rotating-*` and `.gz.tmp` files reaped on
  `NewFileRecorder` open.

## Risks routed back to designer/builder

The designer's §8 pinned three open questions on the builder:

- **8.1 Watcher rotation handling (B-1, load-bearing).** Inode-
  comparison fix in `fileWatcher.Next`; conformance subtest (c)
  asserts no events are silently skipped across rotation.
- **8.2 `--wait` flag implementation (B-3).** Server-side wait —
  API endpoint accepts `?wait=true`; handler blocks on
  `ForceRotate()`'s done channel. Holds HTTP for ≤ 10 s; CLI
  timeout 30 s.
- **8.3 Filename-collision belt-and-braces (B-1).** Pre-rename
  `os.Stat` guard in `gzipAndArchive` — never silently overwrite
  an existing archive.

If any of these turn out to need an architecture call (e.g., the
inode fallback on a filesystem without `Sys().(*Stat_t).Ino`
breaks the conformance subtest), the builder escalates with
`bd create --label needs-architecture` rather than improvising.

## Operator handoff (post-deploy)

After all four beads close, operators get:

- **Default behaviour.** Rotation enabled at 256 MiB; archives kept
  forever. No config required.
- **Override.** `[events.rotation]` block in `city.toml` (or env
  vars `GC_EVENTS_ROTATION_*`).
- **Force-rotate.** `gc events rotate` (non-blocking) or
  `gc events rotate --wait` (blocks until gzip done).
- **Query crossing rotation.** `gc events --since=72h` continues
  to work; archive transparency is invisible. (Designer §5,
  load-bearing — if operators ever need an `--include-archives`
  flag, we've leaked an implementation detail.)
- **Migration.** The single legacy archive on `gc-management`
  (`events.jsonl.archive-20260416.gz`) renames automatically on
  first recorder open after B-4 lands.

## Out of scope

Per architect's framing:

- No external sink (S3 / GCS upload).
- No time-based rotation cadence (size-only).
- No retention beyond age — no count cap, no total-size cap.
- No multi-writer support — exactly one `FileRecorder` per city.
- No rotation for the `Fake` or `exec` providers (NFR-07).
- No coupling with the bd-issues prune in `ga-oeq5` — the two
  cleanups are orthogonal.

## Status

- ga-b6y1.1 — open, slung to builder.
- ga-b6y1.2 — open, blocked by B-1.
- ga-b6y1.3 — open, blocked by B-1.
- ga-b6y1.4 — open, blocked by B-1.
- ga-b6y1 — closing on creation of this plan; designer's own
  `needs-pm` work is done.
