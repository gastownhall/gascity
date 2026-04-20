---
title: "Event Bus"
---


> Last verified against code: 2026-03-01

## Summary

The Event Bus is Gas City's Layer 0-1 primitive providing an append-only
pub/sub log of all system activity -- the universal observation substrate.
Every state change in the system (agent started, bead created, order
fired, controller lifecycle) is recorded as an immutable event with a
monotonically increasing sequence number. The event bus enables
infrastructure mechanisms like order trigger evaluation, CLI event
tailing, and audit logging without coupling producers to consumers.

## Key Concepts

- **Event**: A single immutable record of something that happened. Struct
  with Seq (monotonically increasing `uint64`), Type (dotted string like
  `bead.created`), Ts (`time.Time`), Actor (who did it), Subject (what
  was affected), Message (human-readable description), and Payload
  (optional `json.RawMessage` for structured data). Defined in
  `internal/events/events.go`.

- **Provider**: The full read/write interface for event backends. Embeds
  `Recorder` for writing and adds `List`, `LatestSeq`, `Watch`, and
  `Close`. Implementations: `FileRecorder` (built-in JSONL file), `Fake`
  (in-memory test double), `FailFake` (error-returning test double), and
  `exec.Provider` (user-supplied script). Defined in
  `internal/events/events.go`.

- **Recorder**: The write-only sub-interface. Contains a single method
  `Record(Event)` that is best-effort: errors are logged to stderr, never
  returned to callers. Used by subsystems that only need to emit events.
  Defined in `internal/events/events.go`.

- **Watcher**: A cursor that yields events one at a time. Created by
  `Provider.Watch(ctx, afterSeq)`. Blocks on `Next()` until a new event
  arrives, the context is canceled, or the watcher is closed. Defined in
  `internal/events/events.go`.

- **Filter**: A query predicate for `List` and `ReadFiltered`. Supports
  filtering by Type (exact match), Actor (exact match), Since
  (`time.Time` lower bound), and AfterSeq (`uint64` sequence cursor).
  Zero-valued fields are ignored. Multiple non-zero fields are ANDed.
  Defined in `internal/events/reader.go`.

- **Discard**: A sentinel `Recorder` that silently drops all events.
  Used when event recording is unwanted (e.g., certain test scenarios).
  Defined in `internal/events/events.go`.

## Architecture

The event bus is a single interface with three implementations, selected
at startup by the `[events].provider` config key or `GC_EVENTS` env var.

```
                     events.Provider (interface)
                    /        |          \
                   /         |           \
           FileRecorder    Fake        exec.Provider
          (JSONL file)   (in-memory)  (user script)

  Recorder (sub-interface: write-only)
      |
  Discard (no-op sentinel)
```

**Provider resolution** (in `cmd/gc/providers.go:newEventsProvider`):

1. `GC_EVENTS` env var (highest priority)
2. `[events].provider` in `city.toml`
3. Default: file-backed JSONL at `.gc/events.jsonl`

Valid provider values: `""` (default FileRecorder), `"fake"` (in-memory),
`"fail"` (broken test double), `"exec:<script-path>"` (user-supplied
script).

### Data Flow

The most common operation: recording an event and reading it back.

```
Record(Event{Type, Actor, Subject, Message, Payload})
  --> provider assigns Seq (monotonically increasing)
  --> provider fills Ts if zero (time.Now())
  --> provider writes one JSON line to .gc/events.jsonl (FileRecorder)
  --> errors logged to stderr, never returned (best-effort)

List(Filter{Type, Actor, Since, AfterSeq})
  --> reads all events from .gc/events.jsonl
  --> applies filter predicates (AND semantics)
  --> returns matching events as []Event

LatestSeq()
  --> scans file for highest Seq value
  --> returns 0 if empty or missing

Watch(ctx, afterSeq)
  --> returns a Watcher positioned after afterSeq
  --> Watcher.Next() blocks until new event arrives
  --> context cancellation unblocks Next() with ctx.Err()
```

Watch lifecycle for FileRecorder:

```
Watch(ctx, afterSeq=5)
  --> creates fileWatcher{path, afterSeq=5, poll=250ms}

Next() loop:
  1. Drain internal buffer (previously fetched events)
  2. Check context (return ctx.Err() if canceled)
  3. ReadFrom(path, byteOffset) to get new events since last read
  4. Filter to events with Seq > afterSeq
  5. Buffer matching events, drain on next iteration
  6. If no new events, sleep 250ms and retry
```

Watch lifecycle for Fake:

```
Watch(ctx, afterSeq=5)
  --> creates fakeWatcher{fake, afterSeq=5, ctx}

Next() loop:
  1. Scan in-memory Events slice under mutex
  2. Return first event with Seq > afterSeq
  3. If none found, block on select:
     - ctx.Done() --> return ctx.Err()
     - fake.notify channel --> new event recorded, retry
```

### Key Types

- **`Event`** (`internal/events/events.go`) -- The immutable event
  record. JSON-tagged for JSONL serialization. Payload uses
  `json.RawMessage` for arbitrary structured data and is omitted from
  JSON output when nil.

- **`Provider`** (`internal/events/events.go`) -- The full read/write
  interface. Embeds `Recorder` and adds List, LatestSeq, Watch, Close.

- **`Recorder`** (`internal/events/events.go`) -- The write-only
  sub-interface. Single method `Record(Event)` with best-effort
  semantics.

- **`Filter`** (`internal/events/reader.go`) -- Query predicate for
  List and ReadFiltered. Zero values are ignored; non-zero fields are
  ANDed.

- **`FileRecorder`** (`internal/events/recorder.go`) -- Production
  implementation. Appends JSONL to `.gc/events.jsonl` with `O_APPEND`
  for cross-process safety and a `sync.Mutex` for in-process
  serialization.

## Invariants

These properties must hold for any correct Provider implementation. They
are enforced by the conformance suite in
`internal/events/eventstest/conformance.go`.

1. **Seq is monotonically increasing.** For any two events recorded by
   the same provider, the later event has a strictly greater Seq.

2. **Seq is unique.** No two events share the same Seq value, even
   under concurrent recording.

3. **Seq is auto-filled by the provider.** Callers do not set Seq; the
   provider assigns it on Record.

4. **Ts is auto-filled when zero.** If the caller provides a zero Ts,
   the provider fills it with the current time. An explicit non-zero Ts
   is preserved.

5. **Record is best-effort.** Recording errors are logged to stderr but
   never returned to callers. The caller's operation must not fail
   because event recording failed.

6. **Events are immutable once recorded.** There is no Update or Delete
   operation. The append-only log only grows.

7. **List with empty Filter returns all events.** A zero-valued Filter
   matches everything.

8. **Filter fields are ANDed.** When multiple Filter fields are non-zero,
   an event must match all of them to be included.

9. **LatestSeq returns 0 for an empty provider.** Missing file, empty
   file, or no events all return (0, nil).

10. **Watch(ctx, afterSeq) yields only events with Seq > afterSeq.**
    Existing events at or before afterSeq are never returned.

11. **Watch.Next() blocks until an event arrives or the context is
    canceled.** Context cancellation returns `context.Canceled` or
    `context.DeadlineExceeded`.

12. **Malformed lines are skipped.** ReadAll, ReadFiltered, and ReadFrom
    silently skip lines that fail JSON unmarshalling. This handles
    partial writes from crashes.

13. **Missing file returns nil, not error.** ReadAll and ReadLatestSeq
    return (nil, nil) and (0, nil) respectively for nonexistent files.

14. **FileRecorder resumes Seq across restarts.** NewFileRecorder scans
    the existing file to find the maximum Seq, so new events continue
    monotonically even after a process restart.

15. **Payload is omitted from JSON when nil.** The `omitempty` tag
    ensures events without payloads produce compact JSON lines.

## Interactions

| Depends on | How |
|---|---|
| `encoding/json` | All serialization uses standard library JSON |
| `context` | Watch and Watcher use contexts for cancellation |
| (no internal Gas City dependencies) | Event Bus is a pure Layer 0-1 primitive with no upward dependencies |

| Depended on by | How |
|---|---|
| `cmd/gc/controller.go` | Records `controller.started` and `controller.stopped` events at lifecycle boundaries; passes `Recorder` to reconciliation and shutdown |
| `cmd/gc/reconcile.go` | Records `agent.started`, `agent.stopped`, `agent.crashed`, `agent.idle_killed`, `agent.quarantined`, `agent.suspended` events during reconciliation |
| `cmd/gc/order_dispatch.go` | Records `order.fired`, `order.completed`, `order.failed` events during order dispatch |
| `cmd/gc/cmd_events.go` | CLI `gc events` command: reads and displays events with filtering (`--type`, `--since`), watch mode (`--watch`), and sequence query (`--seq`) |
| `cmd/gc/cmd_event_emit.go` | CLI `gc event emit` command: records custom events from scripts and bd hooks (best-effort, always exits 0) |
| `cmd/gc/cmd_agent.go` | Records agent lifecycle events during start/stop/restart operations |
| `cmd/gc/cmd_suspend.go` | Records `city.suspended` and `city.resumed` events |
| `cmd/gc/cmd_mail.go` | Records `mail.sent` and `mail.read` events |
| `cmd/gc/cmd_convoy.go` | Records `convoy.created` and `convoy.closed` events |
| `internal/orders/triggers.go` | Event triggers query the Provider via `List(Filter{Type, AfterSeq})` to check if matching events exist since the last cursor position |

## Code Map

| Path | Description |
|---|---|
| `internal/events/events.go` | Event struct, Recorder interface, Provider interface, Watcher interface, event type constants, Discard sentinel |
| `internal/events/recorder.go` | FileRecorder: JSONL file-backed Provider with O_APPEND + mutex; fileWatcher with 250ms polling |
| `internal/events/reader.go` | Filter struct, ReadAll, ReadFiltered, ReadLatestSeq, ReadFrom (byte-offset incremental reading) |
| `internal/events/fake.go` | Fake: in-memory Provider for testing with channel-based watcher notification; FailFake: error-returning variant |
| `internal/events/exec/exec.go` | exec.Provider: delegates all operations to a user-supplied script via fork/exec with JSON wire protocol |
| `internal/events/exec/exec_test.go` | exec.Provider tests including stateful mock script, conformance suite, timeout, and error handling |
| `internal/events/eventstest/conformance.go` | RunProviderTests: 20+ subtests that any Provider must pass; RunConcurrencyTests: concurrent recording safety |
| `internal/events/conformance_test.go` | Wires FileRecorder and Fake into the conformance suite |
| `internal/events/events_test.go` | FileRecorder-specific tests: write, payload round-trip, monotonic seq, concurrent safety, seq resume, timestamp handling |
| `cmd/gc/providers.go` | eventsProviderName: resolution logic (GC_EVENTS env -> city.toml -> default); newEventsProvider: factory function |
| `cmd/gc/cmd_events.go` | `gc events` CLI: list, filter, watch, payload-match, seq query |
| `cmd/gc/cmd_event_emit.go` | `gc event emit` CLI: best-effort custom event recording |

### Event Type Constants

All event type constants are defined in `internal/events/events.go`:

| Constant | Value | Emitted by |
|---|---|---|
| `AgentStarted` | `agent.started` | Controller reconciliation on agent start |
| `AgentStopped` | `agent.stopped` | Controller reconciliation on agent stop, shutdown, or drain completion |
| `AgentCrashed` | `agent.crashed` | Controller reconciliation when a running agent's process is gone |
| `AgentDraining` | `agent.draining` | Agent drain command |
| `AgentUndrained` | `agent.undrained` | Agent undrain command |
| `AgentQuarantined` | `agent.quarantined` | Controller when crash loop threshold exceeded |
| `AgentIdleKilled` | `agent.idle_killed` | Controller when idle timeout exceeded |
| `AgentSuspended` | `agent.suspended` | Controller when agent is suspended via config |
| `BeadCreated` | `bead.created` | Bead creation hooks |
| `BeadClosed` | `bead.closed` | Bead close hooks |
| `BeadUpdated` | `bead.updated` | Bead update hooks |
| `MailSent` | `mail.sent` | Mail send command |
| `MailRead` | `mail.read` | Mail read command |
| `ConvoyCreated` | `convoy.created` | Convoy creation |
| `ConvoyClosed` | `convoy.closed` | Convoy close |
| `ControllerStarted` | `controller.started` | Controller startup |
| `ControllerStopped` | `controller.stopped` | Controller shutdown |
| `CitySuspended` | `city.suspended` | City suspend command |
| `CityResumed` | `city.resumed` | City resume command |
| `AutomationFired` | `order.fired` | Order dispatch when a trigger is due |
| `AutomationCompleted` | `order.completed` | Order dispatch on successful completion |
| `AutomationFailed` | `order.failed` | Order dispatch on failure |

## Configuration

The event bus backend is selected via the `[events]` section in
`city.toml`:

```toml
[events]
provider = ""   # "" (default: file JSONL), "fake", "fail", or "exec:/path/to/script"
```

The `GC_EVENTS` environment variable overrides the config file. This is
used primarily in tests (`GC_EVENTS=fake` for in-memory,
`GC_EVENTS=fail` for error path testing).

The default FileRecorder stores events at `.gc/events.jsonl` relative to
the city directory. The file is created automatically on first write.

### Storage Format

Events are stored as newline-delimited JSON (JSONL / NDJSON). Each line
is a complete, self-contained JSON object:

```json
{"seq":1,"type":"controller.started","ts":"2026-03-01T10:00:00Z","actor":"gc"}
{"seq":2,"type":"agent.started","ts":"2026-03-01T10:00:01Z","actor":"gc","subject":"worker-1","message":"agent started successfully"}
{"seq":3,"type":"bead.created","ts":"2026-03-01T10:00:05Z","actor":"human","subject":"gc-42","payload":{"title":"Fix bug","labels":["urgent"]}}
```

The JSONL format provides:
- **Append-only writes** -- new events are appended without reading or
  rewriting the file
- **Crash resilience** -- partial writes (truncated last line) are
  skipped by readers
- **Incremental reads** -- `ReadFrom(path, byteOffset)` reads only new
  data from a known position
- **Cross-process safety** -- `O_APPEND` flag ensures atomic appends
  at the OS level

### Exec Provider Wire Protocol

The exec provider (`exec:<script>`) delegates operations to a
user-supplied script. The script receives the operation name as its
first argument:

| Operation | Script invocation | Stdin | Stdout |
|---|---|---|---|
| `ensure-running` | `script ensure-running` | (none) | (ignored) |
| `record` | `script record` | JSON event | (ignored) |
| `list` | `script list` | JSON filter | JSON array of events |
| `latest-seq` | `script latest-seq` | (none) | Integer |
| `watch` | `script watch <afterSeq>` | (none) | NDJSON stream |

Exit code 2 means "unknown operation" and is treated as success
(forward compatible). `ensure-running` is called once per provider
lifetime via `sync.Once`.

## Testing

The event bus has a layered testing strategy aligned with
[TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md):

**Conformance suite** (`internal/events/eventstest/conformance.go`):
`RunProviderTests` runs 20+ subtests against any Provider
implementation, covering Record+List round-trip, auto-fill of Seq and
Ts, field preservation, List filtering (by type, actor, afterSeq, since,
combined), no-match and empty cases, LatestSeq (empty, after records,
monotonic), Watch (existing events, new events, afterSeq cursor, context
cancellation), and Close. `RunConcurrencyTests` verifies concurrent
Record safety with unique Seq values.

**FileRecorder-specific tests** (`internal/events/events_test.go`):
Tests for JSONL writing, payload round-trip, payload omission when nil,
monotonic Seq, concurrent safety (10 goroutines x 10 events), Seq
resume across process restarts, timestamp auto-fill and explicit
preservation.

**Reader tests** (`internal/events/events_test.go`): Tests for ReadAll
(missing file, empty file), ReadFiltered (by type, actor, since,
combined, AfterSeq, no match), ReadLatestSeq (missing, empty, after
writes), ReadFrom (full read, incremental from mid-file, missing file,
no new data).

**Fake tests** (`internal/events/events_test.go`): Record and List for
in-memory provider, LatestSeq, Watch with goroutine recording,
FailFake error paths.

**Conformance wiring** (`internal/events/conformance_test.go`): Runs
both `RunProviderTests` and `RunConcurrencyTests` against FileRecorder
and Fake.

**exec.Provider tests** (`internal/events/exec/exec_test.go`): Record
via stdin capture, List and LatestSeq with mock scripts,
Watch with NDJSON streaming, ensure-running called once, exit 2
handling, error propagation, timeout enforcement, and full conformance
suite against a stateful jq-based mock script.

**Compile-time interface checks**: Both `FileRecorder` and `Fake` have
`var _ Provider = (*T)(nil)` compile-time assertions in
`events_test.go`. The exec Provider has its own in `exec.go`.

## Known Limitations

- **FileRecorder Watch uses polling, not inotify.** The fileWatcher
  polls the JSONL file every 250ms via `ReadFrom`. This adds up to
  250ms latency for event delivery and uses CPU for polling. A future
  optimization could use `fsnotify` to wake on file changes. The Fake
  provider uses channel-based notification for zero-latency delivery
  in tests.

- **No event retention or rotation.** The JSONL file grows without
  bound. There is no built-in log rotation, retention policy, or
  compaction. For long-running cities, manual truncation or external
  log rotation is needed.

- **ReadFiltered scans the entire file.** Every `List` call reads all
  events from disk and filters in memory. There are no indexes. This
  is acceptable at current scale but will degrade with very large event
  logs. `ReadFrom` with byte offsets provides incremental reading for
  the Watch path.

- **No event schema validation.** Event types are string constants with
  no runtime validation. Recording an event with a misspelled type
  succeeds silently.

- **Filter does not support Subject.** The Filter struct supports Type,
  Actor, Since, and AfterSeq but not Subject. Filtering by subject
  requires post-filtering in the caller.

- **Exec provider Watch is subprocess-lifetime-bound.** The exec
  watcher reads from a long-running subprocess's stdout. If the
  subprocess exits, the watcher reports an error rather than
  reconnecting.

## See Also

- [Architecture glossary](glossary.md) -- authoritative definitions of
  event bus, order, trigger, and other terms used in this document
- [Health Patrol architecture](health-patrol.md) -- how the controller
  reconciliation loop records agent lifecycle events on every tick
- [Bead Store architecture](beads.md) -- the other Layer 0-1 primitive;
  events and beads together provide persistence + observation
- [Config architecture](config.md) -- how `[events].provider` is
  resolved and how progressive activation works
- [TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md) -- testing philosophy and tier
  boundaries for the conformance suite approach
- [CLAUDE.md](https://github.com/gastownhall/gascity/blob/main/CLAUDE.md) -- design principles including "Event
  Bus is the universal observation substrate" (layering invariant 3)
