# gc.active_work_bead — per-step attribution for usage facts (v0)

> Council-reviewed (2026-06-24). Decision 1 = store the work bead's bare `gc.step_id`
> (unanimous). Decision 2 = clear-on-terminal (majority). The compute read site is cut.

## Problem

Usage facts (`internal/usage`) and the spend rows derived from them carry `RunID`
and `SessionID`, but **`StepID` is always empty**. The usage record sites only have
the **session bead** in scope, not the **work bead** the session is executing. So a
run's cost cannot be broken down per step.

The events plane already carries `step_id`: a `bead.created`/`bead.closed` event reads
its **subject bead's own `gc.step_id`**. That works because the events record site
*is* the work bead. The usage record site is the session, so it needs a pointer FROM
the session TO the work it is running.

## The step identity (corrected; load-bearing)

`gc.step_id` (`beadmeta.StepIDMetadataKey`) is the **bare logical formula step id**
(e.g. `mol.finalize`), written by the control plane onto a work bead. It is
**distinct from the bead's runtime `bead.ID`**, which is namespaced per attempt/scope
(e.g. `mol.finalize.attempt.1`): `control.go:602-605/686-690` set `gc.step_id` to the
bare `child.ID`/`control.ID` onto a bead whose ID is `attemptPrefix + "." + child.ID`;
`ralph.go` sets `gc.step_id = step.ID` while the iteration bead id is
`step.ID + ".iteration.N"`; `molecule.go:1380` builds `index[stepID] = bead.ID`,
proving the two differ. Non-formula beads (ad-hoc, orders, manual) carry **no**
`gc.step_id`.

So the step_id that JOINS cross-plane is the **bare `gc.step_id`** — the events plane
already uses it, and the cost plane must use the same value. Storing `bead.ID` would
never join with events. **Decision 1 (locked, unanimous): the pointer holds the work
bead's `gc.step_id`** (empty when the bead has none).

## Goal

Introduce `gc.active_work_bead` on the **session bead**: write the current work bead's
`gc.step_id` at the claim transition, read it at the model-usage record site to
populate `usage.Fact.StepID`. Mirrors `gc.current_run_id` (`cmd/gc/cmd_hook_claim.go`).
Unblocks per-step cost for **formula/molecule runs** (the dominant attributed path);
ad-hoc/manual work correctly rolls up at run level with empty StepID, matching events.

## Mechanism

### Key
`internal/beadmeta/keys.go`: `ActiveWorkBeadMetadataKey = "gc.active_work_bead"` +
add to `KnownMetadataKeys`.

### Write — FOLDED into the run-id write (one atomic Update, same bead)
`cmd/gc/cmd_hook_claim.go` `recordHookClaimRunID` already writes `gc.current_run_id`
on the session bead at claim. **Fold** the step write into the SAME `store.Update`:
derive `stepID = bead.Metadata[StepIDMetadataKey]` from the SAME just-claimed `bead`
that yields `runID`, and write `{gc.current_run_id: runID, gc.active_work_bead: stepID}`
in one update. This (a) halves the bd calls + `bead.updated` events per claim, and
(b) locks the (run, step) tuple — the step is guaranteed to belong to the run stamped
in the same claim. **Unconditional per claim** (a current-pointer must follow a reused
pool session), including writing an EMPTY step (clearing a prior step when the new
work is non-formula).

### Read — model facts only (compute read CUT)
`internal/worker/invocation_telemetry.go modelUsageFact`: read
`bead.Metadata[ActiveWorkBeadMetadataKey]` → `Fact.StepID`, from the SAME session-bead
snapshot `b` already used for `ResolveRunID`, so StepID and RunID never come from two
reads. Nil-map read returns `""` (Go semantics) — safe, like the existing
`b.Metadata["provider_kind"]` read.

The compute emitter does **not** read it: `emitComputeFactForBead` fires at the
session's terminal/sleep transition where the pointer is definitionally stale, and v0
compute facts are omitted from spend anyway.

### Clear — at the terminal/sleep transition (Decision 2, locked majority)
`cmd/gc/usage_compute.go` already does `store.SetMetadata` on the session bead at every
terminal pass. Clear `gc.active_work_bead` there (write `""`), so an idle / manual-chat
invocation after work ends and before the next claim resolves `StepID=""` (run/session
level) rather than the last step's id.

## Honest limits (NOT a "rare race")
- **Stale-on-idle is routine, not rare** — the model read fires on EVERY prompt op
  (incl. idle/manual-chat), and reused pool/canonical sessions are the norm. The
  terminal clear (above) is what bounds it; it is REQUIRED before the per-step
  consumer (#51) ships, not deferred.
- **Read-record non-atomicity** (genuinely narrow, accepted, matches
  `gc.current_run_id`): a second claim between the transcript-tail read and the fact
  record can stamp the new step. Bounded; both pointers read from one snapshot keep
  StepID under the matching RunID.

## Seam coverage (gc hook --claim is NOT universal)
Work-starts the claim hook does NOT cover, where `gc.active_work_bead` is empty/stale:
manual chat / no work bead (→ empty StepID); API-server fresh-handle turns;
`RuntimeHandle` prompt ops (out of scope per `recordInvocationTelemetry`). **Empty
StepID for these is CORRECT** — non-attribution, identical to the events plane's empty
step_id for the same work. The only mis-attribution case is the pooled-session idle
window, which the terminal clear addresses.

## Test plan (TDD)
- `beadmeta`: the new key is in `KnownMetadataKeys`.
- claim hook: one `store.Update` writes BOTH `gc.current_run_id` AND
  `gc.active_work_bead` on the session bead; the step value = the work bead's
  `gc.step_id` — with a fixture where **`gc.step_id != bead.ID`** so a `bead.ID`
  regression can't pass; `ResolveRunID(workbead)` equals the stamped run id (tuple
  consistency); empty `GC_SESSION_ID` → skip; store error → best-effort (no panic);
  a non-formula bead (no `gc.step_id`) writes an empty step (clears any prior).
- `modelUsageFact`: reads the session bead's `gc.active_work_bead` → `Fact.StepID`,
  distinct from RunID/SessionID; empty when absent.
- compute terminal: clears `gc.active_work_bead` on the session bead.

## Scope
gc-side: the key; the folded write at claim; the model read; the terminal clear. OUT
of scope (separate tasks): the metered-path proxy `X-Gc-Step-Id` stamp; the dashboard
per-step rollup; the events-ingest `city_events.step_id` consumer.
