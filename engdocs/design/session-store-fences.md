---
title: "Session Store Fences"
---

| Field | Value |
|---|---|
| Status | Accepted |
| Scope | Cross-process write safety for session-owned bead metadata |
| Consumers | Every session refactor slice that moves a metadata writer (`internal/session/PLAN.md` Steps 5-6 and beyond) |
| Related | `internal/session/REQUIREMENTS.md` (scenario ledger); design-review bead `ga-unpr2y` |

This document settles, once, the question every mutating session extraction
otherwise re-litigates: what fencing the beads store actually supports, and
which fences a slice may rely on when it moves a session metadata writer.
A slice that moves a writer cites this document and names its fence; it does
not negotiate store semantics per operation.

## What the store provides

Facts, from `internal/beads/beads.go` on this branch:

- **No conditional writes.** `UpdateOpts` has no expected-revision,
  expected-value, or token field. There is no compare-and-swap primitive
  anywhere in the `Store` interface; the optional
  `ConditionalAssignmentReleaser.ReleaseIfCurrent` is the one existing
  narrowly-scoped conditional write, and is the precedent for proposing
  another narrow conditional primitive rather than general CAS.
- **Batches are not atomic on external stores.** `SetMetadataBatch` applies
  all writes atomically on in-memory stores (MemStore, FileStore) but
  sequentially on external stores (BdStore, exec) — partial application is
  possible on mid-batch failure. Callers must design batch contents to be
  idempotent and tolerate partial writes.
- **Transactions cannot read.** The `Tx` interface exposes `Update`,
  `SetMetadataBatch`, and `Close` only. There is no `Get`, so
  read-modify-write cannot run inside a transaction, and implementations
  without native transaction support may execute writes sequentially.

Designing as if the store had CAS, atomic batches, or read-your-write
transactions is designing for a store we do not have. Conversely, demanding
that the store grow those capabilities before any extraction proceeds is a
multi-month detour no incident history justifies.

## The two sanctioned fences

Both fences below are already load-bearing in production and fixed real
races. New session-owned writers use one of them; which one depends on
whether the contended decision spans processes or only writers.

### Fence 1 — city identifier flock

`session.WithCitySessionIdentifierLocks` (`internal/session/names.go`)
serializes critical sections across OS processes with an exclusive
`syscall.Flock` on a per-identifier lock file under the city directory.
The holder re-reads live state inside the lock before writing.

Use when two *processes* (CLI, API server, controller) can race to create or
claim the same session identity. Proof in history: the session bead adoption
race was fixed exactly this way — serialize adoption-barrier creation on the
identifier lock, re-check for a live bead inside the lock
(`b0c53e84c`, #2051).

Rules:

- Lock scope is the session identifier (or identifier set), never a global
  lock.
- Re-read the contended state inside the lock; decide from the re-read, not
  from any snapshot taken before acquisition.
- Keep the critical section to decide-and-write; no provider calls, no
  subprocesses, no event emission while holding the lock.

### Fence 2 — token precondition with reread

A generation token stored in session metadata (`instance_token`) identifies
which incarnation of a session a pending action belongs to. Before a
destructive or commit action, the actor re-reads the bead and verifies the
token still matches; a mismatch means another incarnation superseded the
action, which is then dropped.

Use when the race is between a *stale async continuation* and current state:
async start commits, stop/interrupt of a possibly-recycled session, orphan
release. Proof in history: `verifiedStop` / `verifiedInterrupt`
(`cmd/gc/session_wake.go`) gate kills on `instance_token`; stale async start
results are rejected the same way (`4649e7105`, #1531); orphan release
re-reads live work state immediately before mutating (`ca81d000a`, #1781).

Rules:

- The token changes when the incarnation changes (fresh start, recycle),
  never on ordinary metadata writes.
- Verify token → write → (where the consequence is destructive) verify
  outcome. The reread and the write are not atomic — see the next section
  for why that is accepted.

## The accepted residual: reread-then-write is not atomic

Between the precondition reread and the write, another writer can still
interleave. With no store CAS this window cannot be closed by the client;
flock closes it only where both writers honor the same lock.

This residual is accepted deliberately, on the project's own principle that
the system converges because work persists: reconciliation is idempotent and
convergent. Every session-owned write must
be safe to lose the race — the next reconciler tick re-derives the decision
from durable state and converges. Concretely, this means a writer that
adopts Fence 2 must also satisfy:

- **Idempotent re-application.** Applying the same patch twice is harmless
  (the existing patch constructors in `internal/session` are maps of
  absolute values, not increments applied blindly — counters reread before
  accrual).
- **Edge-triggered consumption.** State that triggers an action clears in
  the same patch that records the action (`last_woke_at` clearing in the
  exit-classification family), so a lost race re-evaluates rather than
  double-fires.
- **Partial-batch tolerance.** Batch contents are ordered and chosen so any
  prefix leaves a state the reconciler heals (the existing
  `healState`/projection machinery is the recovery owner).

## What a mutating slice must state

One paragraph in the slice's PR description, not a contract artifact:

1. Which fence the moved writer uses (1, 2, or "single-writer: only the
   controller tick writes this key family" — which is itself a valid answer
   and the most common one).
2. Why a lost race converges (which tick, scan, or heal re-derives it).
3. The test that proves the contended path: a raced-writer test for Fence 1,
   a stale-token test for Fence 2, or a single-writer guard for the rest.

## What we are explicitly not doing

- Not adding CAS/conditional writes to the beads store as a refactor
  precondition. If a future operation genuinely needs it, that is a store
  feature with its own bead and justification.
- Not building a store-capability matrix artifact. The capability facts are
  three bullets above and live in `internal/beads/beads.go` doc comments —
  the code is the source of truth.
- Not treating events as a fence. Events are post-commit facts; safety-
  critical convergence comes from durable state scans (root `AGENTS.md`,
  the convergence-because-work-persists principle).
