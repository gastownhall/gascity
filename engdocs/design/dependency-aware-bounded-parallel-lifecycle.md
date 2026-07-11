---
title: "Dependency-Aware Bounded Parallel Lifecycle"
---

| Field | Value |
|---|---|
| Status | Implemented |
| Date | 2026-03-18 |
| Author(s) | Codex |
| Issue | N/A |
| Supersedes | N/A |

## Summary

Gas City currently makes per-city session lifecycle decisions in a
single-threaded reconciler tick and also executes most lifecycle
operations serially. That keeps the implementation simple, but it makes
slow providers dominate startup and restart latency. This proposal keeps
the existing single-writer decision model and introduces a separate
execution phase that runs session starts and bulk stops in bounded
parallel waves.

The key constraint is dependency safety. Session starts must respect the
`depends_on` graph, so dependencies are fully started before dependents
begin. Bulk force-stops should run in the reverse order so dependents
stop before their dependencies are killed. The design deliberately keeps
metadata mutation and event recording deterministic by doing planning
and result application serially, while only the provider calls execute
in parallel.

## Motivation

### Pain today

1. `gc init` and `gc start` spend tens of seconds waiting on provider
   startup, even when several agents could be started concurrently.
2. `gc stop` and `gc rig restart` kill sessions one-by-one, so a city
   with several agents pays the full sum of stop latency.
3. The reconciler already reasons about dependencies and wake budgets,
   but the execution path throws away that structure by calling
   `sp.Start()` inline.
4. The current code assumes single-threaded execution within a tick
   because session bead metadata maps are mutated in place. A safe
   parallel implementation must preserve that property instead of
   turning the reconciler into a shared-memory race.

### Goals

- Reduce session startup and bulk stop latency without weakening
  dependency correctness.
- Preserve the current per-tick metadata semantics.
- Keep retries and failure handling idempotent and predictable.
- Avoid turning provider errors into cross-session cascade failures.

### Non-goals

- Parallelizing every store mutation or event emission.
- Changing the wake contract, drain contract, or crash-loop semantics.
- Adding new user-facing daemon config for lifecycle concurrency in this
  first pass.

## Current Constraints

Today the reconciler has three useful properties that must survive:

1. **Single decision thread.** The reconciler evaluates session state in
   one pass and mutates bead metadata directly.
2. **Wake budget.** `defaultMaxWakesPerTick` limits the number of wake
   attempts in one tick to avoid a thundering herd after restart.
3. **Dependency gating.** `allDependenciesAlive` ensures a session only
   starts once its dependencies are live.

The problem is that these same properties are currently coupled to the
actual provider calls. `preWakeCommit`, `sp.Start`, event recording, and
hash persistence all happen inline in the main reconciler loop, so one
slow start stalls unrelated work in the same dependency layer.

## Design Principles

1. **Plan serially, execute concurrently, commit serially.**
2. **Dependencies are a barrier, not a hint.**
3. **Bound concurrency separately from wake budgeting.**
4. **Per-session failure must not poison unrelated sessions.**
5. **The next tick remains the retry mechanism.**
6. **Worker completion order must never affect committed state.**

## Proposed Design

### 1) Split lifecycle into three phases

Each reconciler tick becomes:

1. **Decision phase (serial).**
   Evaluate every session bead exactly as today: heal state, detect
   crash loops, handle drain requests, evaluate wake reasons, and decide
   whether a session should start, stay running, or begin draining.
2. **Execution phase (parallel).**
   Run provider `Start`, `Stop`, and `Interrupt` calls through bounded
   worker pools.
3. **Commit phase (serial).**
   Apply metadata updates, event recording, and bookkeeping derived from
   the execution results in a deterministic order.

This narrows the old “single-threaded tick” invariant to the parts that
actually require a single writer: bead metadata and recorder/store
interactions.

Every lifecycle candidate receives a stable order key during planning.
Starts use topo order with original session order as the tie-breaker.
Bulk stop helpers use reverse dependency wave order with stable
within-wave ordering. Commit-side effects always apply in that stable
planned order, never in worker completion order.

### 2) Start sessions in dependency-aware waves

The start pipeline is:

1. Build the candidate set during the decision phase.
2. For each candidate, determine which dependency templates are already
   satisfied by currently alive sessions.
3. Candidates with no unsatisfied dependency templates form the first
   ready wave.
4. Execute one ready wave at a time with bounded parallelism.
5. A template becomes satisfied for downstream candidates when at least
   one session of that template successfully starts in the current tick,
   or one was already alive before the tick.
6. Dependents only enter a later wave after all dependency templates are
   satisfied.
7. Before dispatching a later wave, dependency liveness is revalidated at
   the wave boundary. A dependency template counts as satisfied only if
   it still has a currently alive instance at dispatch time.

This matches current semantics for both singleton and pool dependencies:
dependents only need “an alive instance” of the dependency template.

### 3) Keep `preWakeCommit` outside the worker goroutines

`preWakeCommit` writes session metadata and updates the in-memory bead
snapshot. Those writes must stay serial. Before dispatching a ready wave,
the reconciler:

1. runs `preWakeCommit` for each candidate in that wave,
2. builds the final `runtime.Config`,
3. stores the precomputed core/live hashes needed after startup.

Only then does it launch parallel `sp.Start()` calls. If a start fails,
the commit phase clears the tentative wake markers and records a wake
failure exactly once, matching current behavior.

Workers are pure execution units. They may not mutate bead metadata,
hashes, recorder state, or dependency satisfaction state directly. They
return immutable results to the serial commit phase.

### 3a) Define terminal results explicitly

Each execution worker must return exactly one terminal result:

- `success`
- `provider_error`
- `deadline_exceeded`
- `canceled`
- `panic_recovered`

The serial commit phase applies one shared rollback rule for all
non-success results:

1. clear tentative wake markers that would otherwise cause false crash
   detection,
2. preserve the generation/token state written by `preWakeCommit` so the
   next tick can detect stale operations consistently,
3. record exactly one wake failure,
4. leave retry to the next tick under the existing wake/quarantine
   contract.

If a provider later makes a session visible after a timed-out start, the
next tick resolves the ambiguity the same way Gas City resolves any
out-of-band runtime state: provider liveness plus bead metadata healing
determine the new truth.

### 4) Reverse the dependency order for bulk force-stops

Bulk stop operations should treat dependencies in reverse:

- dependents first,
- dependencies last.

This matters most for force-stop phases (`gc stop`, controller shutdown,
provider swap, `gc rig restart`). A reverse dependency order reduces the
chance of tearing down a critical dependency while a dependent is still
trying to exit cleanly.

Subset stops must still preserve transitive dependency order between the
selected templates. If `api -> cache -> db` and only `api` plus `db`
are being stopped, `api` still stops before `db` even though `cache`
itself is not in the stop set.

Soft interrupts do not need the same strict ordering, because they are
best-effort nudges rather than destructive actions. They should be sent
as one bounded parallel broadcast to minimize shutdown latency.

For pool dependencies, stop ordering also stays template-scoped. All
instances of a dependent template are eligible before instances of the
templates they depend on.

### 5) Use small internal bounds first

This change adds internal lifecycle limits:

- `defaultMaxParallelStartsPerWave`
- `defaultMaxParallelStopsPerWave`

These are intentionally separate from `defaultMaxWakesPerTick`.

Wake budget answers “how many new sessions may we attempt this tick?”
Parallelism answers “how many provider calls may run at once?”

Keeping them separate lets the controller remain conservative about wake
storms while still exploiting concurrency for the starts it does allow.

Provider calls also run under bounded per-operation contexts derived
from the existing startup/shutdown deadlines. No wave may wait forever
on a hung provider call, and a timed-out call must still yield a single
terminal result for serial commit.

The maximum tick cost of one lifecycle wave is therefore bounded by the
largest per-operation deadline in that wave plus serial commit overhead.
The design does not attempt asynchronous commit across waves in this
first pass; it chooses bounded latency over maximum overlap.

### 6) Wave barriers are commit barriers

A later start wave may advance only after every candidate in the prior
wave has reached a terminal result and that result has been serially
committed. “Observed one success” is not enough. The barrier is:

1. wave execution complete,
2. results sorted by stable planned order,
3. serial commit finished,
4. dependency liveness revalidated for the next wave.

This keeps next-tick eligibility and same-tick dependency semantics
predictable even under partial failure.

## Reference Behavior

### Start example

Given:

```toml
[[agent]]
name = "db"

[[agent]]
name = "api"
depends_on = ["db"]

[[agent]]
name = "worker"
depends_on = ["api"]

[[agent]]
name = "audit"
depends_on = ["db"]
```

If all four are dead and should wake:

1. wave 1 starts `db`
2. wave 2 starts `api` and `audit` in parallel
3. wave 3 starts `worker`

If `api` fails in wave 2, `worker` does not start in that tick. `audit`
is unaffected.

### Stop example

For the same graph, a bulk force-stop runs:

1. `worker` and `audit` first
2. then `api`
3. then `db`

Within a wave, stops can execute in parallel up to the configured bound.

## Failure Handling

### Start failures

- A failed start only fails that session.
- Its dependents remain blocked for the current tick.
- Other independent branches continue.
- The next tick retries according to the existing wake contract.

### Stop failures

- A failed stop is reported, but other sessions in the wave continue.
- The next tick or next command retry handles survivors.
- Dependency order is advisory for cleanup safety, not a hard guarantee
  that all dependents must have stopped before any dependency can ever be
  touched.
- Soft-interrupt and force-stop phases are explicitly best-effort; the
  controller records survivors rather than pretending ordering guarantees
  imply successful teardown.

### Cycles

Cycles are already invalid at config-validation time. If the runtime
helper ever observes an unusable dependency graph, it falls back to the
existing strictly serial behavior for both start and stop paths and
emits a diagnostic. It does not attempt partial batching or speculative
reordering on an invalid graph.

## Implementation Plan

### Reconciler start path

1. Introduce an internal `startCandidate` plan type.
2. Leave the existing main pass serial, but enqueue start candidates
   instead of calling `sp.Start()` inline.
3. Add a helper that executes candidates in dependency-aware waves with
   bounded concurrency and returns ordered results.
4. Apply success/failure side effects serially after each wave.

### Shared graph helper

Start and stop paths must share one internal graph helper that:

- accepts a stable ordered candidate list,
- groups candidates into dependency waves,
- supports forward and reverse traversal modes,
- returns stable within-wave ordering,
- can explicitly fall back to strict serial execution.

The controller should not carry separate ad hoc topological planners for
start, stop, restart, and provider-swap paths.

### Bulk stop path

1. Introduce a helper that groups session names into reverse dependency
   waves.
2. Use it from `gracefulStopAll` for the force-stop phase.
3. Use it from `doRigRestart` for direct restart kills.
4. Use the same helper for provider-swap teardown and controller
   shutdown, so all bulk stop entry points share one ordering contract.

### Diagnostics

The implementation should emit enough information to debug lifecycle
behavior in production:

- log the computed wave number for started/stopped sessions,
- distinguish “deferred by dependency/barrier” from “failed to start,”
- record when the serial fallback path is used,
- record per-wave duration so slow providers are visible.

More concretely, each lifecycle candidate in a tick must end with one
operator-visible outcome:

- `started`
- `failed`
- `blocked_on_dependencies`
- `skipped_due_to_failed_dependency`
- `deferred_by_wake_budget`
- `already_satisfied`
- `stop_failed`
- `stop_slow_survivor`

Each outcome should be emitted with stable correlation fields:

- tick id
- wave id
- operation (`start`, `interrupt`, `stop`)
- session name
- template name
- dependency blocker set
- queue/enqueue time
- dispatch time
- provider completion time
- terminal result/outcome

The implementation should also expose metrics for:

- per-wave duration
- per-session provider `Start` / `Stop` latency by outcome
- blocked candidate counts by reason
- sessions deferred by wake budget
- sessions deferred by concurrency bound
- in-flight lifecycle operations
- stop survivors after interrupt phase
- active parallelism bounds

This is required so an operator can answer “why did session X not start
on tick Y?” without reconstructing behavior from interleaved logs.

## Testing Strategy

### Unit tests

- dependency wave construction for starts
- reverse dependency wave construction for stops
- wake budget remains enforced even with parallel starts
- a failed dependency start blocks dependents but not siblings
- max in-flight lifecycle operations never exceeds the configured bound
- commit order remains stable regardless of worker completion order
- wave-boundary dependency revalidation blocks dependents when the last
  satisfying dependency dies between waves
- invalid-graph fallback reverts to strictly serial execution

### Reconciler tests

- independent sessions in the same wave start concurrently
- dependents do not start before their dependencies complete
- start failure preserves wake-failure accounting
- bulk stop records all expected stop events while honoring reverse
  dependency order
- a hung provider call terminates via context deadline and does not
  stall the reconciler forever

### Regression tests

- existing drain, quarantine, and idle-timeout behavior remains intact
- provider swap and city shutdown still stop every running session
- event/log reason codes for blocked, deferred, failed, and survivor
  outcomes remain stable and attributable

### Provider safety tests

- runtime adapters must tolerate concurrent `Start`, `Interrupt`, and
  `Stop` calls on distinct session names
- if a provider cannot satisfy that contract, the controller must route
  it through a provider-local serialization shim before enabling the new
  lifecycle path

## Alternatives Considered

### Parallelize the entire reconciler

Rejected. The reconciler mutates bead metadata maps directly and relies
on deterministic side effects. Parallelizing the whole tick would create
shared-memory races and make failure accounting much harder.

### Keep starts serial and only parallelize stops

Rejected. Stop parallelism is useful, but the larger latency win is on
startup and restart, where provider `Start()` dominates.

### Add user-facing concurrency config now

Rejected for the first implementation. Internal defaults are easier to
validate. Once production behavior is proven, daemon config can expose
the bounds if needed.

## Open Questions

1. Whether `advanceSessionDrains` should later parallelize timed-out
   `verifiedStop` calls. This proposal leaves that path unchanged.
2. Whether provider conformance tests should explicitly require
   concurrent `Start`/`Stop` safety across distinct session names.
3. Whether wake budget should eventually become per-layer instead of
   per-tick.
